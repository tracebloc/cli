package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/schema"
)

// newDataCmd wires the `tracebloc data` subtree. The dominant
// verb is `ingest`, completed in Phase 3 (tracebloc/client#151) across
// PR-a (pre-flight: spec synth, validation, layout walk, cluster
// discovery) and PR-b (this one: ephemeral stage Pod + tar-over-
// exec stream + progress bar + SIGINT-safe cleanup). `data delete`
// (#30) removes an ingested dataset's in-cluster artifacts; `data
// list` lists the ingested datasets.
//
// Aliases: "dataset" is kept for one deprecation cycle so existing
// scripts continue to work.
func newDataCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "data",
		Aliases: []string{"dataset"},
		Short:   "Manage the datasets in your workspace",
		Long: `Commands for ingesting and managing the datasets your workspace holds —
the data models train on. It stays on your infrastructure.

` + "`data ingest`" + ` ingests a local dataset into your workspace's storage,
submits the ingestion run, and watches it to completion (streaming
logs + the final summary). ` + "`data validate`" + ` checks an ingest.yaml
locally first.

What a dataset looks like depends on the task:
  tabular / time-series — a .csv file, or a folder with one .csv
  image                  — a folder with labels.csv + images/
  text                   — a folder with labels.csv + texts/

` + "`tracebloc cluster info`" + ` is the pre-flight you'd typically run
before the first ingest.`,
		// A bare `tracebloc data` prints help; a mistyped subcommand errors with a
		// suggestion instead of silently exiting 0 (#75).
		RunE:                       runGroup,
		SuggestionsMinimumDistance: 2,
	}
	// Deprecation notices (#879): the data verb was renamed (dataset→data,
	// push→ingest, rm→delete). The old spellings still work as aliases for one
	// cycle, but an aliased invocation warns once on stderr. root.go has no
	// PersistentPreRunE, so this — the closest hook for any `data <sub>` — is
	// where we detect and warn; cobra passes the executed (leaf) command in.
	cmd.PersistentPreRunE = func(leaf *cobra.Command, _ []string) error {
		warnDeprecatedAlias(leaf, leaf.ErrOrStderr())
		return nil
	}
	cmd.AddCommand(newDataIngestCmd())
	cmd.AddCommand(newDataListCmd())
	cmd.AddCommand(newDataDeleteCmd())
	cmd.AddCommand(newIngestValidateCmd())
	return cmd
}

// deprecatedAliasCanonical maps each deprecated command alias to the canonical
// invocation to steer the user to. Keyed by the alias token; the value is the
// full canonical form so the notice nudges the whole rename (e.g. a `push`
// steers to `data ingest`, covering the dataset→data group rename too).
var deprecatedAliasCanonical = map[string]string{
	"dataset": "data",
	"push":    "data ingest",
	"rm":      "data delete",
}

// warnDeprecatedAlias prints a one-line deprecation notice to w when the executed
// command was invoked through a deprecated alias. It reads cobra's exported
// Command.CalledAs(), which reliably reports the alias for the EXECUTED command —
// so `data push` / `dataset push` warn (leaf `ingest` called as `push`), `data
// rm` / `dataset rm` warn, and a bare `dataset` warns (the `data` group has a
// RunE, so it is the executed command). We intentionally do NOT chase a parent
// group's alias for `dataset <canonical-verb>` (cobra doesn't expose an
// ancestor's invoked-as name without reaching into its internals); the verb
// notices already point at the full `data <verb>` form, which nudges the group
// rename. Canonical invocations warn for nothing.
func warnDeprecatedAlias(cmd *cobra.Command, w io.Writer) {
	invoked := cmd.CalledAs()
	if canonical, ok := deprecatedAliasCanonical[invoked]; ok {
		_, _ = fmt.Fprintf(w,
			"%q is deprecated and will be removed in a future release — use %q instead.\n",
			invoked, canonical)
	}
}

// runDataIngest is the full Phase 3 implementation: pre-flight
// checks, then either --dry-run stop or stage Pod + tar stream +
// cleanup. Phase 4 (#152) will hook submit-to-jobs-manager after
// the staging step.
//
// Step order is "fail fast, fail local" — every step that doesn't
// need the cluster runs before any that does, so a customer with
// a bad label-column or oversized dataset gets the diagnostic in
// milliseconds without a kubeconfig round-trip.
func runDataIngest(ctx context.Context, out, errOut io.Writer, a runDataIngestArgs) (err error) {
	// In --output-json mode, guarantee stdout always carries a JSON
	// object. The dry-run + post-submit paths emit a result and set
	// jsonEmitted; this defer covers every early-failure return (bad
	// table, discovery, staging, token, port-forward) with a JSON error
	// object, so `… --output-json | jq` never sees empty stdout. (Bugbot #49)
	jsonEmitted := false
	defer func() {
		if a.OutputJSON && err != nil && !jsonEmitted {
			code := 1
			var ee *exitError
			if errors.As(err, &ee) {
				code = ee.Code()
			}
			writePushErrorJSON(a.JSONOut, a.Spec, err, code)
		}
	}()

	// Intro header: brand + a plain-English explainer of what an ingest
	// does, so a first-time user understands it before any prompts.
	// Routed through a.Printer, so --output-json keeps it on stderr and
	// --plain/non-TTY degrade cleanly. (#31)
	// --overwrite + a reused --idempotency-key is a data-loss trap: the
	// teardown removes the existing data, then jobs-manager treats the
	// duplicate key as a REPLAY and attaches to the previous run instead of
	// ingesting anything — old data gone, new data never loaded, exit 0 from
	// the old Job's status. Refuse the combination outright.
	if a.Overwrite && a.IdempotencyKey != "" {
		return &exitError{code: 2, err: errors.New(
			"--overwrite can't be combined with --idempotency-key: a reused key makes the cluster replay the previous run instead of ingesting the new data — after --overwrite's removal that would report success while loading nothing. Drop one of the two (a fresh per-run key is the default).")}
	}

	a.Printer.Banner("tracebloc", "data ingest")
	a.Printer.Para(strings.TrimSpace(`
This ingests a dataset so models can train on it. Your files never leave your
own infrastructure — tracebloc copies them into your workspace's storage,
checks them, and loads them into a table your training runs read from. Other
collaborators can train against that table without ever seeing the raw files.`))
	a.Printer.Hintf("Learn more: https://docs.tracebloc.io")

	// 0. Guided mode: prompt for any missing core inputs before
	//    validation. Flags already provided win; non-TTY / --no-input
	//    leaves Prompter nil and skips straight to the flag-only path.
	if a.Interactive && a.Prompter != nil {
		if err := runInteractive(a.Printer, a.Prompter, &a, a.TaskSet); err != nil {
			if errors.Is(err, errInteractiveCancelled) {
				a.Printer.Infof("Cancelled — nothing was ingested.")
				return nil
			}
			// A typed exitError from a guided step (e.g. the path-existence
			// guard, which runInteractive runs before the family sniff)
			// already carries its own code + clean message — surface it as-is
			// rather than burying it under "interactive setup:".
			var ee *exitError
			if errors.As(err, &ee) {
				return err
			}
			return &exitError{code: 3, err: fmt.Errorf("interactive setup: %w", err)}
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
		return &exitError{code: 3, err: errors.New(
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
		return err
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
		return &exitError{code: 2, err: err}
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
		return &exitError{code: 2, err: fmt.Errorf(
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
		return &exitError{code: 2, err: fmt.Errorf(
			"task %q isn't supported by the CLI yet%s. Supported tasks: %s.",
			a.Spec.Category, reason, push.SupportedCategoriesList())}
	default:
		return &exitError{code: 2, err: fmt.Errorf(
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
				return &exitError{code: 2, err: fmt.Errorf(
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
		return &exitError{code: 2, err: fmt.Errorf(
			"--schema is tabular/time-series tasks only; it doesn't apply to task %q", a.Spec.Category)}
	}
	if a.Spec.LabelPolicy != "" && !push.IsRegressionClass(a.Spec.Category) {
		return &exitError{code: 2, err: fmt.Errorf(
			"--label-policy is regression-class tasks only (tabular_regression, "+
				"time_series_forecasting, time_to_event_prediction); it doesn't apply to task %q",
			a.Spec.Category)}
	}
	if a.Spec.TimeColumn != "" && a.Spec.Category != "time_to_event_prediction" {
		return &exitError{code: 2, err: fmt.Errorf(
			"--time-column is time_to_event_prediction only; it doesn't apply to task %q", a.Spec.Category)}
	}
	if a.Spec.NumberOfKeypoints != 0 && a.Spec.Category != "keypoint_detection" {
		return &exitError{code: 2, err: fmt.Errorf(
			"--number-of-keypoints is keypoint_detection only; it doesn't apply to task %q", a.Spec.Category)}
	}
	// --label-column is meaningless for self-supervised text (the label is the
	// text itself); buildText drops it, so accepting it silently discarded the
	// user's value and the review echoed a column that never shipped.
	if a.Spec.LabelColumn != "" && push.SelfSupervisedText(a.Spec.Category) {
		return &exitError{code: 2, err: fmt.Errorf(
			"--label-column doesn't apply to task %q — it trains on the text itself, with no label column",
			a.Spec.Category)}
	}

	// 3. Walk the local directory FIRST (local "fail fast"), dispatched
	//    by category family. Image categories expect labels.csv +
	//    images/; tabular / time-series categories expect a single
	//    data CSV. The walk also yields what the per-category
	//    resolution below needs (the image list for target-size, the
	//    CSV for schema inference).
	// err is the function's named return (see the --output-json defer
	// at the top), so it's not redeclared here. The walk can take a moment
	// on a large tree, so it gets a spinner — no blocking wait stays silent.
	var layout *push.LocalLayout
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
		return &exitError{code: 3, err: err}
	}

	a.Printer.Step(1, 3, "Check your data")
	a.Printer.Hintf("Reading your files locally first — nothing has touched your workspace yet — so a layout or settings problem shows up right away.")

	// 3a. Per-category spec resolution from the local data, so the
	//     synthesized spec carries the right fields before validation.
	switch {
	case push.IsTabular(a.Spec.Category):
		// P3 (cli#71): a BOM'd tabular CSV is doomed in-cluster AND would
		// corrupt InferSchema's own header read below — reject before
		// either. The rest of the content preflight runs after the spec
		// schema validation (mirroring the in-cluster order).
		if perr := push.CheckTabularBOM(layout.LabelsCSV); perr != nil {
			return &exitError{code: 3, err: perr}
		}
		if perr := push.CheckHasDataRows(layout.LabelsCSV); perr != nil {
			return &exitError{code: 3, err: perr}
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
				return &exitError{code: 2, err: perr}
			}
			a.Spec.Schema = sch
		} else {
			res, ierr := push.InferSchema(layout.LabelsCSV)
			if ierr != nil {
				return &exitError{code: 3, err: fmt.Errorf("inferring schema from CSV: %w", ierr)}
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
				return &exitError{code: 2, err: fmt.Errorf(
					"--number-of-keypoints must be a positive integer (got %d); "+
						"it's the number of keypoints per sample (e.g. 17 for COCO pose)",
					a.Spec.NumberOfKeypoints)}
			}
			return &exitError{code: 2, err: errors.New(
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
				return &exitError{code: 2, err: perr}
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
				return &exitError{code: 2, err: perr}
			}
			a.Spec.MinSize = []int{w, h}
		}
		// Extension: every image must share one type, and the spec tells
		// the cluster which one to validate against (file_options.extension).
		// Without this the ingestor checked its .jpeg convention default and
		// rejected .jpg/.png datasets AFTER the full upload (cli#68).
		ext, exterr := push.DetectExtension(layout.Images)
		if exterr != nil {
			return &exitError{code: 3, err: exterr}
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
		return &exitError{code: 2, err: errors.New(msg)}
	}

	// 4. Synthesize the spec from flags + validate against schema.
	//    Catches "bad category", "missing intent" etc. BEFORE we
	//    touch the cluster. The error formatter is the same one
	//    ingest validate uses, so a customer who YAML'd manually
	//    first sees identical wording.
	spec := a.Spec.Build()
	specBytes, err := yaml.Marshal(spec)
	if err != nil {
		return &exitError{code: 3, err: fmt.Errorf("marshaling synthesized spec: %w", err)}
	}
	v, err := schema.NewV1Validator()
	if err != nil {
		return &exitError{code: 3, err: fmt.Errorf("loading embedded schema: %w", err)}
	}
	_, errs, parseErr := v.ValidateYAML(specBytes)
	if parseErr != nil {
		// "Parse" failing on a spec we marshaled ourselves is a
		// programming error, not a customer error — surface it
		// with the bytes so we can diagnose. Exit 3 (the
		// "internal" bucket) matches the marshal-failure branch
		// above.
		return &exitError{code: 3, err: fmt.Errorf("internal: re-parsing synthesized spec: %w\n%s", parseErr, specBytes)}
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
		return &exitError{code: 2, err: errors.New("synthesized spec failed schema validation; check the flag values above")}
	}

	// P3 content preflight (backend#828, cli#69/#71/#72/#73): preview the
	// ingestor's own validators locally — AFTER the spec schema validation,
	// mirroring the in-cluster order (jsonschema first, then validators),
	// and BEFORE any cluster work. Each check names the rule it previews;
	// parity is pinned by internal/push/parity_golden_test.go.
	if perr := runLocalPreflight(a, layout, errOut); perr != nil {
		return perr
	}

	printLocalSummary(a.Printer, layout, spec)

	// 5. Cluster discovery — same kubeconfig path as `cluster info`.
	//    Errors mirror that command's exit-code contract (3 for
	//    kubeconfig, 4 for missing release) so behaviour is
	//    consistent across pre-flight commands.
	// Connecting to the workspace + discovering its shared storage is
	// Kubernetes plumbing (release / PVC / jobs-manager) the happy path keeps
	// quiet — it's no longer a numbered step (RFC-0002 §6), and --verbose adds
	// the release/PVC detail below. But the discovery itself is several blocking
	// apiserver round-trips (kubeconfig load, release + PVC discovery, then the
	// destination-exists check), so it still needs a visible status line — no
	// silent wait on the happy path (RFC-0002 "progress on every wait").
	// A plain line, not a spinner: discoverRelease can print its own
	// namespace-fallback note mid-call, and a spinner's \r redraw would clobber
	// it. ALL the logic below (discovery + the exit-6 destination guard) is
	// unchanged; only the presentation moved.
	a.Printer.Infof("Connecting to your workspace…")
	// 6. PVC discovery (needPVC) confirms the chart's shared-data PVC is
	//    Bound before we waste time provisioning a Pod that can't mount it.
	opts := cluster.KubeconfigOptions{Path: a.Kubeconfig, Context: a.Context, Namespace: a.Namespace}
	binding := bindActiveClientNamespace(&opts)
	target, err := resolveClusterTarget(ctx, a.Printer, opts, binding, true)
	if err != nil {
		return binding.explain(err)
	}
	resolved, cs, release, pvc := target.Resolved, target.Clientset, target.Release, target.PVC
	// release.IngestorSAName is discovered from the ingestionAuthz ConfigMap by
	// DiscoverParentRelease (#7) and flows into the stage/teardown pods + the
	// jobs-manager token mint below — no --ingestor-sa override.

	// 7. Under --verbose, show what we found on the cluster; the happy path
	//    keeps this Kubernetes detail hidden (printClusterSummary is a no-op
	//    without --verbose).
	printClusterSummary(a.Printer, release, pvc)

	// 8a. Destination guard (cli#70, P4-lite): re-ingesting an existing
	//     table used to stage EVERYTHING and then fail the in-cluster Job
	//     on the ingestor's duplicate check — a full upload burned to learn
	//     the table exists. One cheap read heads that off. The check fails
	//     open (dim note) — the ingestor still refuses duplicates, so a
	//     broken check can't cause silent data loss.
	existingTable, checkNote := destTableExists(ctx, cs, resolved, a.Spec.Table)
	if checkNote != "" {
		a.Printer.Hintf("%s", checkNote)
	}
	tableExists := existingTable != ""
	if tableExists && !a.Overwrite {
		// Folded decision (RFC-0002): in interactive mode a pre-existing table
		// is a question, not a wall. Prompt to replace it; a "no" cancels
		// cleanly (exit 0). Non-interactive (or --output-json / --no-input)
		// still hard-fails exit 6 — a script must opt in with --overwrite.
		proceed, aerr := existingTableAction(&a, existingTable)
		if aerr != nil {
			return aerr
		}
		if !proceed {
			a.Printer.Infof("Cancelled — %q was left as-is; nothing was ingested.", existingTable)
			return nil
		}
		a.Overwrite = true
	}
	if tableExists && a.Overwrite {
		a.Printer.Warnf("Table %q already exists — replacing it (table + files).", existingTable)
	}

	// 8. Dry-run stop. Acknowledged success, plus a reminder of the
	//    live-only steps (stage + ingest) the customer just skipped.
	if a.DryRun {
		a.Printer.Newline()
		a.Printer.Successf("Dry-run complete — your data and workspace check out; nothing was created.")
		a.Printer.Hintf("A real run continues with step 2 (copy into your workspace) and step 3 (validate and load).")
		if a.OutputJSON {
			writePushJSON(a.JSONOut, "dry-run", spec, nil, "", "")
			jsonEmitted = true
		}
		return nil
	}

	// 8b. --overwrite: remove the existing table + files before staging —
	//     the same teardown `data delete` runs, so the semantics match.
	if tableExists && a.Overwrite {
		// Tear down the MATCHED name, not the flag's spelling — table names
		// are case-sensitive on Linux MySQL and PVC paths always are, so
		// acting on a differently-cased --table would silently no-op the
		// DROP/rm and then "succeed".
		plan := push.PlanTeardown(existingTable)
		rmSpin := a.Printer.Spinner(fmt.Sprintf("Removing the existing %q first", existingTable), "")
		_, terr := push.Teardown(ctx, cs, &push.SPDYExecutor{Config: resolved.RestConfig, Client: cs}, resolved.Namespace, plan, push.PodSpecOptions{
			Namespace:          resolved.Namespace,
			PVCClaimName:       pvc.ClaimName,
			PVCMountPath:       pvc.MountPath,
			Table:              existingTable,
			ServiceAccountName: release.IngestorSAName,
			Image:              a.StagePodImage,
		})
		rmSpin.Stop()
		if terr != nil {
			// The teardown drops the table before removing files, so a
			// partial failure can leave files the DB-backed guard can no
			// longer see — a plain re-run would upload everything and then
			// hit them in-cluster. data delete first is the real recovery.
			return &exitError{code: 7, err: fmt.Errorf(
				"replacing table %q failed partway — its removal may be incomplete, and a plain re-run "+
					"would hit the leftovers after uploading everything. Run `tracebloc data delete %s` "+
					"first, then re-run this ingest. Nothing new was staged. (%w)",
				existingTable, existingTable, terr)}
		}
		a.Printer.Successf("Removed the old %q — ingesting the new data.", existingTable)
	}

	// 9. Stage the files: create ephemeral Pod → wait Ready → tar
	//    stream → cleanup. The deferred cleanup inside push.Stage
	//    runs on success and failure (including ctx cancellation
	//    from a SIGINT handler), so no orphan Pod is left behind.
	//
	//    Exit code 7 ("staging failed") is distinct from the
	//    pre-flight codes so customers can branch on whether the
	//    failure was their environment vs the actual data transfer.
	a.Printer.Step(2, 3, "Copy into your workspace")
	a.Printer.Hintf("Your files are copied securely into your workspace's storage — set up and cleaned up for you.")
	progress := push.NewProgress(out, layout.TotalBytes,
		fmt.Sprintf("Copying %s", a.Spec.Table))
	// Defer Finish so a failure path that returns BEFORE
	// StreamLayout (e.g. CreateStagePod fails on PSA rejection,
	// WaitForStagePodReady times out) still clears the TTY
	// progress UI. push.StreamLayout's own deferred Finish would
	// otherwise be unreachable. Calling Finish twice on the same
	// schollz bar is a no-op, so the double-call on the happy
	// path is safe. Bugbot flagged on PR-b round 5.
	defer progress.Finish()
	stageErr := push.Stage(ctx, push.StageOptions{
		Client: cs,
		Executor: &push.SPDYExecutor{
			Config: resolved.RestConfig,
			Client: cs,
		},
		Namespace:      resolved.Namespace,
		IngestorSAName: release.IngestorSAName,
		PVCClaimName:   pvc.ClaimName,
		PVCMountPath:   pvc.MountPath,
		Layout:         layout,
		Table:          a.Spec.Table,
		StagePodImage:  a.StagePodImage,
		Progress:       progress,
		Out:            out,
	})
	if stageErr != nil {
		return &exitError{code: 7, err: stageErr}
	}

	// 10–12. The ingestion-run tail: mint token → port-forward → submit →
	//         classify → emit JSON → reclaim staging. Extracted so its
	//         outcome matrix (exit 5/8/9, JSON emission, and the
	//         must-NOT-reclaim-on-partial gate) is table-testable via the
	//         injected seams without a cluster (#1009). jsonEmitted flows
	//         back so the --output-json error defer above stays correct.
	je, runErr := runIngestionRun(ctx, out, a, target, specBytes, spec)
	jsonEmitted = je
	return runErr
}
