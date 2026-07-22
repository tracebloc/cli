// The local, cluster-free half of `data ingest`: resolveLocalInput (the
// whole pre-cluster pipeline — prompts, validation, layout walk, spec
// synthesis + schema validation), path expansion + the
// path-existence-first guard, the local dataset summary, and the
// preflight that previews the ingestor's validators on the local data.
// Moved verbatim from data.go (cli#282, cli#283) — behavior unchanged.

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tracebloc/cli/internal/pathutil"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/schema"
	"github.com/tracebloc/cli/internal/ui"
)

// sortedKeys returns m's keys in sorted order — used to list a CSV's inferred
// columns in the friendly missing-label message (#214) deterministically.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// expandHome expands a leading ~ (current user or ~user) to a home
// directory, leaving every other path untouched. It's the CLI-local
// name for the shared pathutil.ExpandHome; cluster.expandPath resolves
// to the same helper, so ~-expansion is identical across subcommands
// (a --kubeconfig ~alice/... resolves alice's home just like a data
// ingest path does). See pathutil.ExpandHome for the full contract. (#181)
func expandHome(path string) string {
	return pathutil.ExpandHome(path)
}

// statDatasetPath is the "path existence FIRST" guard (#181): a typo'd
// path fails plainly on the path — a clean "no such file or directory" —
// before any family sniff, label preview, or schema work touches it.
// Both entry points call it: the flag-only path from runDataIngest's 0b
// step, and the guided path from runInteractive (before the family sniff),
// so the invariant holds on every route rather than only the flag path.
func statDatasetPath(path string) error {
	if _, serr := os.Stat(path); serr != nil {
		if errors.Is(serr, os.ErrNotExist) {
			return &exitError{code: exitLocalEnv, err: fmt.Errorf(
				"no such file or directory: %q — check the path to your dataset", path)}
		}
		return &exitError{code: exitLocalEnv, err: fmt.Errorf(
			"can't read %q: %w", path, serr)}
	}
	return nil
}

// resolveLocalInput is everything `data ingest` does BEFORE touching the
// cluster: the --overwrite/--idempotency-key guard, the intro explainer, the
// guided prompts, path expansion + the existence-first check, table-name /
// category / misapplied-flag validation, the local layout walk, per-category
// spec resolution, spec synthesis + schema validation, the P3 content
// preflight, and the local summary. Extracted verbatim from runDataIngest
// (cli#283) — step order, output, and exit codes unchanged.
//
// It mutates a in place (a.LocalPath's ~-expansion; a.Spec's intent default
// and resolved schema / target size / min size / extension), exactly as the
// inline code mutated its copy — so runDataIngest's --output-json error
// defer (which stays next to the named return it reads) and the cluster
// steps that follow see the resolved spec. cancelled=true with a nil err is
// the guided flow's clean Ctrl-C: the caller exits 0, nothing ingested.
func resolveLocalInput(out, errOut io.Writer, a *runDataIngestArgs) (layout *push.LocalLayout, spec map[string]any, specBytes []byte, cancelled bool, err error) {
	// Intro: a plain-English explainer of what an ingest does, so a
	// first-time user understands it before any prompts.
	// Routed through a.Printer, so --output-json keeps it on stderr and
	// --plain/non-TTY degrade cleanly. (#31)
	// --overwrite + a reused --idempotency-key is a data-loss trap: the
	// teardown removes the existing data, then jobs-manager treats the
	// duplicate key as a REPLAY and attaches to the previous run instead of
	// ingesting anything — old data gone, new data never loaded, exit 0 from
	// the old Job's status. Refuse the combination outright.
	if a.Overwrite && a.IdempotencyKey != "" {
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: errors.New(
			"--overwrite can't be combined with --idempotency-key: a reused key makes the cluster replay the previous run instead of ingesting the new data — after --overwrite's removal that would report success while loading nothing. Drop one of the two (a fresh per-run key is the default).")}
	}

	a.Printer.Newline()
	a.Printer.Para("Ingest a dataset — your files never leave this machine.")
	a.Printer.Hintf("Learn how: https://docs.tracebloc.io/create-use-case/prepare-dataset")

	// 0. Guided mode: prompt for any missing core inputs before
	//    validation. Flags already provided win; non-TTY / --no-input
	//    leaves Prompter nil and skips straight to the flag-only path.
	if a.Interactive && a.Prompter != nil {
		if err := runInteractive(a.Printer, a.Prompter, a, a.TaskSet); err != nil {
			if errors.Is(err, errInteractiveCancelled) {
				a.Printer.Infof("Cancelled — nothing was ingested.")
				return nil, nil, nil, true, nil
			}
			// A typed exitError from a guided step (e.g. the path-existence
			// guard, which runInteractive runs before the family sniff)
			// already carries its own code + clean message — surface it as-is
			// rather than burying it under "interactive setup:".
			var ee *exitError
			if errors.As(err, &ee) {
				return nil, nil, nil, false, err
			}
			return nil, nil, nil, false, &exitError{code: exitLocalEnv, err: fmt.Errorf("interactive setup: %w", err)}
		}
	}
	// --intent defaults to train. Applied after the interactive block so
	// the guided flow still asks "training or test?" (it prompts on an
	// empty value); a non-interactive run that omits --intent gets train
	// without erroring (RFC-0002 §5). The wire field stays "intent".
	if a.Spec.Intent == "" {
		a.Spec.Intent = "train"
	}
	if a.LocalPath == "" {
		return nil, nil, nil, false, &exitError{code: exitLocalEnv, err: errors.New(
			"local dataset path is required — pass it as an argument, or run " +
				"on a terminal without --no-input for guided prompts")}
	}
	// Expand a leading ~ ourselves. The shell expands an unquoted ~ on
	// the command line, but a path typed at the interactive prompt (or
	// a quoted/literal ~ arg) reaches us unexpanded — and filepath.Abs
	// would just prepend the CWD, yielding ".../cwd/~/...". Mirrors
	// cluster.expandPath; done here so it covers both entry points
	// before any push.Discover* call. (#37)
	a.LocalPath = expandHome(a.LocalPath)

	// 0b. Path existence FIRST — before any spec / schema / family
	//     validation. A typo'd path should fail on the path with a plain
	//     "no such file or directory", not surface later as a confusing
	//     downstream error (e.g. the task gate asking which task the
	//     non-existent data is for). runInteractive runs this same guard
	//     before its family sniff / label preview, so the invariant holds on
	//     the guided route too; this re-check covers the flag-only path and
	//     is cheap (one stat). The family walk below stats again for its
	//     layout-specific diagnostics; this is only about ordering the first
	//     failure a customer sees. (#181)
	if err := statDatasetPath(a.LocalPath); err != nil {
		return nil, nil, nil, false, err
	}

	// 1. Validate the table name BEFORE anything else. It's both
	//    the MySQL identifier and the /data/shared/<table>/ PVC
	//    subdirectory — an unsanitized traversal name (../../etc)
	//    would escape that subtree once the stage Pod writes to
	//    it. The embedded schema only checks minLength on `table`,
	//    so this CLI-side guard is the real fix. SpecArgs.Build()
	//    below calls StagedPrefix, which panics on an unsafe name —
	//    so this check MUST come first.
	if err := push.ValidateTableName(a.Spec.Table); err != nil {
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: err}
	}

	// 2. Category gate. Runs BEFORE schema validation so an
	//    unsupported category gets a clear, actionable CLI message
	//    rather than the schema's terse enum / missing-property error.
	//    Every schema task category is CLI-supported now; this gate stays as
	//    defensive routing so a future known-but-not-yet-wired category gets a
	//    clear per-category message, and a typo'd category gets the supported
	//    list rather than the schema's raw enum dump.
	switch {
	case a.Spec.Category == "":
		// No task chosen. In guided mode the picker already filled this;
		// reaching here means a non-interactive run (or --no-input /
		// --output-json) that omitted --task. Give a clear, actionable
		// error instead of silently assuming images (the old default).
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: fmt.Errorf(
			"which task is this data for? pass --task — one of: %s. "+
				"(On a terminal without --no-input, tracebloc asks you to pick.)",
			push.SupportedCategoriesList())}
	case push.IsCLISupported(a.Spec.Category):
		// supported
	case push.IsKnown(a.Spec.Category):
		// A recognized category the CLI doesn't implement yet. None today — every
		// schema category is wired — but kept as defensive routing so a future
		// known-but-unsupported category gets the registry's per-category reason,
		// not a misleading "unrecognized category". Supported categories were
		// already caught above, so IsKnown here means known-but-unsupported.
		spec, _ := push.Lookup(a.Spec.Category)
		reason := ""
		if spec.UnsupportedNote != "" {
			reason = " (" + spec.UnsupportedNote + ")"
		}
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: fmt.Errorf(
			"task %q isn't supported by the CLI yet%s. Supported tasks: %s.",
			a.Spec.Category, reason, push.SupportedCategoriesList())}
	default:
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: fmt.Errorf(
			"task %q isn't a recognized task. Supported tasks: %s.",
			a.Spec.Category, push.SupportedCategoriesList())}
	}

	// Image-only flags. --target-size / --min-size describe image
	// resolution, so they're meaningless on a tabular / text task.
	// Reject them explicitly here: without this guard they'd be parsed
	// only inside the image branch below, so on a non-image task the
	// value — even a malformed one — was silently dropped with no error.
	if !push.IsImage(a.Spec.Category) {
		for _, f := range []struct{ name, val string }{
			{"--target-size", a.TargetSizeFlag},
			{"--min-size", a.MinSizeFlag},
		} {
			if f.val != "" {
				return nil, nil, nil, false, &exitError{code: exitBadInput, err: fmt.Errorf(
					"%s is image tasks only; it doesn't apply to task %q",
					f.name, a.Spec.Category)}
			}
		}
	}

	// Task-scoped flags. Like --target-size/--min-size above, each of these is
	// read only inside the one category branch that consumes it, so passing one
	// on a task that doesn't use it silently dropped the value — and the user's
	// intent — with no error, even though the help text says each is scoped.
	// Reject a misapplied flag explicitly so it fails fast instead of being
	// ignored (the scope mirrors spec.go's build gates exactly).
	if a.SchemaFlag != "" && !push.IsTabular(a.Spec.Category) {
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: fmt.Errorf(
			"--schema is tabular/time-series tasks only; it doesn't apply to task %q", a.Spec.Category)}
	}
	if a.Spec.LabelPolicy != "" && !push.IsRegressionClass(a.Spec.Category) {
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: fmt.Errorf(
			"--label-policy is regression-class tasks only (tabular_regression, "+
				"time_series_forecasting, time_to_event_prediction); it doesn't apply to task %q",
			a.Spec.Category)}
	}
	if a.Spec.TimeColumn != "" && a.Spec.Category != "time_to_event_prediction" {
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: fmt.Errorf(
			"--time-column is time_to_event_prediction only; it doesn't apply to task %q", a.Spec.Category)}
	}
	if a.Spec.NumberOfKeypoints != 0 && a.Spec.Category != "keypoint_detection" {
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: fmt.Errorf(
			"--number-of-keypoints is keypoint_detection only; it doesn't apply to task %q", a.Spec.Category)}
	}
	// --label-column is meaningless for self-supervised text (the label is the
	// text itself); buildText drops it, so accepting it silently discarded the
	// user's value and the review echoed a column that never shipped.
	if a.Spec.LabelColumn != "" && push.SelfSupervisedText(a.Spec.Category) {
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: fmt.Errorf(
			"--label-column doesn't apply to task %q — it trains on the text itself, with no label column",
			a.Spec.Category)}
	}

	// 3. Walk the local directory FIRST (local "fail fast"), dispatched
	//    by category family. Image categories expect labels.csv +
	//    images/; tabular / time-series categories expect a single
	//    data CSV. The walk also yields what the per-category
	//    resolution below needs (the image list for target-size, the
	//    CSV for schema inference).
	// err and layout are this function's named returns, so neither is
	// redeclared here. The walk can take a moment on a large tree, so it
	// gets a spinner — no blocking wait stays silent.
	walkSpin := a.Printer.Spinner("Reading your files", "")
	switch {
	case push.IsTabular(a.Spec.Category):
		layout, err = push.DiscoverTabular(a.LocalPath)
	case push.IsText(a.Spec.Category):
		layout, err = push.DiscoverText(a.Spec.Category, a.LocalPath)
	case a.Spec.Category == "object_detection":
		layout, err = push.DiscoverObjectDetection(a.LocalPath)
	case a.Spec.Category == "semantic_segmentation":
		layout, err = push.DiscoverSemanticSegmentation(a.LocalPath)
	default:
		// image_classification + keypoint_detection: labels.csv + images/.
		layout, err = push.Discover(a.LocalPath)
	}
	walkSpin.Stop()
	if err != nil {
		return nil, nil, nil, false, &exitError{code: exitLocalEnv, err: err}
	}

	a.Printer.Step(1, 3, "Check your data")
	a.Printer.Hintf("Reading your files locally first — nothing has touched your secure environment yet — so a layout or settings problem shows up right away.")

	// 3a. Per-category spec resolution from the local data, so the
	//     synthesized spec carries the right fields before validation.
	switch {
	case push.IsTabular(a.Spec.Category):
		// P3 (cli#71): a BOM'd tabular CSV is doomed in-cluster AND would
		// corrupt InferSchema's own header read below — reject before
		// either. The rest of the content preflight runs after the spec
		// schema validation (mirroring the in-cluster order).
		if perr := push.CheckTabularBOM(layout.LabelsCSV); perr != nil {
			return nil, nil, nil, false, &exitError{code: exitLocalEnv, err: perr}
		}
		if perr := push.CheckHasDataRows(layout.LabelsCSV); perr != nil {
			return nil, nil, nil, false, &exitError{code: exitLocalEnv, err: perr}
		}

		// Column schema. An explicit --schema wins (raw flag, or the
		// optional override the interactive prompt captures into SchemaFlag).
		// Otherwise infer the types here — mirroring the ingestor's own rules
		// (di#349) — and EMIT the result explicitly (below, via a.Spec.Schema
		// → spec.schema), so the ingestor uses the CLI's answer regardless of
		// its own version. Inference runs on both a no-schema non-interactive
		// run and an interactive run where the user left the schema prompt
		// blank; the risky cases below are surfaced as warnings.
		if a.SchemaFlag != "" {
			sch, perr := push.ParseSchema(a.SchemaFlag)
			if perr != nil {
				return nil, nil, nil, false, &exitError{code: exitBadInput, err: perr}
			}
			a.Spec.Schema = sch
		} else {
			res, ierr := push.InferSchema(layout.LabelsCSV)
			if ierr != nil {
				return nil, nil, nil, false, &exitError{code: exitLocalEnv, err: fmt.Errorf("inferring schema from CSV: %w", ierr)}
			}
			a.Spec.Schema = res.Schema
			_, _ = fmt.Fprintf(out,
				"Inferred schema for %d column(s) from %s (override with --schema).\n",
				len(res.Schema), filepath.Base(layout.LabelsCSV))
			if len(res.Skipped) > 0 {
				_, _ = fmt.Fprintf(out,
					"  (skipped framework-managed column(s): %s)\n", strings.Join(res.Skipped, ", "))
			}
			if len(res.Empty) > 0 {
				_, _ = fmt.Fprintf(out,
					"  (warning: %d column(s) had no values in the sample and were typed VARCHAR(1): %s)\n",
					len(res.Empty), strings.Join(res.Empty, ", "))
			}
			if len(res.IDLike) > 0 {
				_, _ = fmt.Fprintf(out,
					"  (warning: %d column(s) look like identifiers (all-unique integers): %s — "+
						"if any is a zero-padded code, pass --schema to type it VARCHAR)\n",
					len(res.IDLike), strings.Join(res.IDLike, ", "))
			}
		}
	case push.IsImage(a.Spec.Category):
		// keypoint_detection needs --number-of-keypoints (dataset-
		// specific, no default). Catch it here with an actionable
		// message rather than letting the ingestor fail mid-run. Split
		// UNSET from SET-BUT-INVALID (#76b): a Go 0 could mean either, so
		// key on whether the flag was passed (ChangedFlags). Unset → the
		// "requires" nudge; set to a non-positive value → name the bad
		// value so the user sees exactly what was rejected.
		if a.Spec.Category == "keypoint_detection" && a.Spec.NumberOfKeypoints <= 0 {
			if a.ChangedFlags["number-of-keypoints"] {
				return nil, nil, nil, false, &exitError{code: exitBadInput, err: fmt.Errorf(
					"--number-of-keypoints must be a positive integer (got %d); "+
						"it's the number of keypoints per sample (e.g. 17 for COCO pose)",
					a.Spec.NumberOfKeypoints)}
			}
			return nil, nil, nil, false, &exitError{code: exitBadInput, err: errors.New(
				"keypoint_detection requires --number-of-keypoints (e.g. " +
					"--number-of-keypoints 17); it's dataset-specific and has no default")}
		}
		// Image target resolution: the ingestor's image_classification
		// default is 512x512 and it VALIDATES (it does not resize), so
		// a mismatch hard-fails. Honour an explicit --target-size;
		// otherwise auto-detect from the first image so the common
		// "all my images are NxN" case just works.
		if a.TargetSizeFlag != "" {
			w, h, perr := push.ParseTargetSize(a.TargetSizeFlag)
			if perr != nil {
				return nil, nil, nil, false, &exitError{code: exitBadInput, err: perr}
			}
			a.Spec.TargetSize = []int{w, h}
		} else if len(layout.Images) > 0 {
			if w, h, derr := push.DetectImageSize(layout.Images[0]); derr == nil {
				a.Spec.TargetSize = []int{w, h}
				_, _ = fmt.Fprintf(out,
					"Auto-detected image target size %dx%d from %s (override with --target-size).\n",
					w, h, filepath.Base(layout.Images[0]))
			} else {
				_, _ = fmt.Fprintf(errOut,
					"Note: couldn't auto-detect image size (%v); using the ingestor "+
						"default. Pass --target-size WxH if ingestion reports a "+
						"resolution mismatch.\n", derr)
			}
		}
		// Minimum-size floor override (#348): plumb an explicit --min-size to
		// spec.file_options.min_size. When unset, no spec field is emitted, so
		// the ingestor applies its own default (none on the deployed
		// v0.5.7/v0.6.0; 32x32 on develop post-#348) — and the local preview
		// applies NO floor either (PreflightDataset only previews the floor
		// when --min-size is set, so it never rejects an ingest the live
		// cluster accepts). The below-floor reject is previewed in
		// runLocalPreflight (ValidateImages).
		if a.MinSizeFlag != "" {
			w, h, perr := push.ParseMinSize(a.MinSizeFlag)
			if perr != nil {
				return nil, nil, nil, false, &exitError{code: exitBadInput, err: perr}
			}
			a.Spec.MinSize = []int{w, h}
		}
		// Extension: every image must share one type, and the spec tells
		// the cluster which one to validate against (file_options.extension).
		// Without this the ingestor checked its .jpeg convention default and
		// rejected .jpg/.png datasets AFTER the full upload (cli#68).
		ext, exterr := push.DetectExtension(layout.Images)
		if exterr != nil {
			return nil, nil, nil, false, &exitError{code: exitLocalEnv, err: exterr}
		}
		a.Spec.Extension = ext
	default:
		// Text family: no extra per-category resolution. The supervised text
		// tasks (text_classification, token_classification,
		// sentence_pair_classification) carry a label straight from
		// --label-column; the self-supervised ones (masked/causal language
		// modeling, seq2seq, embeddings) need neither a label nor a schema.
		// buildText emits the label for exactly the supervised set, keyed on
		// the registry's SelfSupervised flag (not a hardcoded id).
	}

	// 3b. Friendly missing-label pre-check (#214). Tabular / time-series tasks
	//     AND semantic_segmentation carry a required label column (the ingest
	//     schema's allOf requires `label` for them). With no --label-column the
	//     synthesized spec's `label` is an empty string, which trips the schema's
	//     label oneOf and the raw validation below dumps an opaque "got object,
	//     want string" / "minLength" pair. semseg is especially prone to this —
	//     its per-image label reads as vestigial beside the pixel masks, so the
	//     flag is easy to forget. Intercept ONLY that specific missing case here —
	//     a label present-but-not-in-the-CSV still flows to runLocalPreflight's
	//     CheckLabelColumn, and every other schema error still reaches the dump —
	//     and name the flag to fix instead.
	if (push.IsTabular(a.Spec.Category) || a.Spec.Category == "semantic_segmentation") && a.Spec.LabelColumn == "" {
		msg := "this task needs a label column, but --label-column wasn't set — " +
			"pass --label-column with the name of the target column in your data CSV"
		if cols := sortedKeys(a.Spec.Schema); len(cols) > 0 {
			msg += " (columns: " + strings.Join(cols, ", ") + ")"
		}
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: errors.New(msg)}
	}

	// 4. Synthesize the spec from flags + validate against schema.
	//    Catches "bad category", "missing intent" etc. BEFORE we
	//    touch the cluster. The error formatter is the same one
	//    ingest validate uses, so a customer who YAML'd manually
	//    first sees identical wording.
	spec = a.Spec.Build()
	specBytes, err = yaml.Marshal(spec)
	if err != nil {
		return nil, nil, nil, false, &exitError{code: exitLocalEnv, err: fmt.Errorf("marshaling synthesized spec: %w", err)}
	}
	v, err := schema.NewV1Validator()
	if err != nil {
		return nil, nil, nil, false, &exitError{code: exitLocalEnv, err: fmt.Errorf("loading embedded schema: %w", err)}
	}
	_, errs, parseErr := v.ValidateYAML(specBytes)
	if parseErr != nil {
		// "Parse" failing on a spec we marshaled ourselves is a
		// programming error, not a customer error — surface it
		// with the bytes so we can diagnose. Exit 3 (the
		// "internal" bucket) matches the marshal-failure branch
		// above.
		return nil, nil, nil, false, &exitError{code: exitLocalEnv, err: fmt.Errorf("internal: re-parsing synthesized spec: %w\n%s", parseErr, specBytes)}
	}
	if len(errs) > 0 {
		// Use the SAME formatter `ingest validate` uses, so the
		// experience is identical whether the customer authored
		// YAML by hand or via flags. Diagnostics go to stderr
		// (matching ingest validate) so a downstream pipe of
		// stdout (e.g. piping the summary to jq once that's a
		// JSON output mode) isn't polluted by error text. Exit 2
		// is reserved for schema violations across the CLI.
		_, _ = fmt.Fprintf(errOut, "synthesized spec failed schema validation (%d issue%s):\n",
			len(errs), plural(len(errs)))
		_, _ = fmt.Fprintln(errOut, schema.FormatErrors(errs))
		return nil, nil, nil, false, &exitError{code: exitBadInput, err: errors.New("synthesized spec failed schema validation; check the flag values above")}
	}

	// P3 content preflight (backend#828, cli#69/#71/#72/#73): preview the
	// ingestor's own validators locally — AFTER the spec schema validation,
	// mirroring the in-cluster order (jsonschema first, then validators),
	// and BEFORE any cluster work. Each check names the rule it previews;
	// parity is pinned by internal/push/parity_golden_test.go.
	if perr := runLocalPreflight(*a, layout, errOut); perr != nil {
		return nil, nil, nil, false, perr
	}

	printLocalSummary(a.Printer, layout, spec)

	return layout, spec, specBytes, false, nil
}

// printLocalSummary shows what the CLI found on disk plus the ingest
// settings it assembled — the detail under step 1 ("Check your data").
// Mirrors `cluster info`'s section/Field layout.
func printLocalSummary(p *ui.Printer, layout *push.LocalLayout, spec map[string]any) {
	cat, _ := spec["category"].(string)

	p.Section("Local dataset")
	p.Field("root", layout.Root)
	switch {
	case push.IsTabular(cat):
		p.Field("data CSV", layout.LabelsCSV)
		if sch, ok := spec["schema"].(map[string]string); ok {
			p.Field("columns", fmt.Sprintf("%d", len(sch)))
		}
	case push.IsText(cat):
		dir := push.TextSidecarDir(cat)
		p.Field("labels.csv", layout.LabelsCSV)
		p.Field(dir, fmt.Sprintf("%d files", len(layout.Sidecars[dir])))
	default:
		p.Field("labels.csv", layout.LabelsCSV)
		imagesVal := fmt.Sprintf("%d files", len(layout.Images))
		if ext, _ := spec["spec"].(map[string]any); ext != nil {
			if fo, _ := ext["file_options"].(map[string]any); fo != nil {
				if e, _ := fo["extension"].(string); e != "" {
					imagesVal = fmt.Sprintf("%d files (%s)", len(layout.Images), e)
				}
			}
		}
		p.Field("images", imagesVal)
		if anns := layout.Sidecars["annotations"]; len(anns) > 0 {
			p.Field("annotations", fmt.Sprintf("%d files", len(anns)))
		}
		if masks := layout.Sidecars["masks"]; len(masks) > 0 {
			p.Field("masks", fmt.Sprintf("%d files", len(masks)))
		}
	}
	p.Field("total size", push.HumanBytes(layout.TotalBytes))

	p.Section("Ingest settings")
	p.Field("name", fmt.Sprintf("%v", spec["table"]))
	p.Field("task", fmt.Sprintf("%v", spec["category"]))
	p.Field("intent", fmt.Sprintf("%v", spec["intent"]))
	switch lbl := spec["label"].(type) {
	case string:
		p.Field("label column", lbl)
	case map[string]any:
		p.Field("label column", fmt.Sprintf("%v (policy: %v)", lbl["column"], lbl["policy"]))
	}
	if tc, ok := spec["time_column"].(string); ok && tc != "" {
		p.Field("time column", tc)
	}
	p.Field("destination", push.FinalDestPrefix(spec["table"].(string)))
}

// runLocalPreflight maps push.PreflightDataset — THE shared preview
// dispatch, also exercised verbatim by the parity harness — onto the CLI's
// conventions: notes print dim to errOut, a BadFlag problem exits 2 (fix a
// flag), anything else exits 3 (fix the data).
func runLocalPreflight(a runDataIngestArgs, layout *push.LocalLayout, errOut io.Writer) error {
	notes, problem := push.PreflightDataset(a.Spec, layout)
	for _, n := range notes {
		_, _ = fmt.Fprintln(errOut, n)
	}
	if problem == nil {
		return nil
	}
	code := exitLocalEnv
	if problem.BadFlag {
		code = exitBadInput
	}
	return &exitError{code: code, err: problem.Err}
}
