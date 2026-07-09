package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"k8s.io/client-go/kubernetes"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/schema"
	"github.com/tracebloc/cli/internal/submit"
	"github.com/tracebloc/cli/internal/ui"
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
		Short:   "Manage the datasets in your client",
		Long: `Commands for staging and managing the datasets your client holds —
the data models train on. It stays on your infrastructure.

` + "`data ingest`" + ` stages a local dataset into your client's storage,
submits the ingestion run, and watches it to completion (streaming
logs + the final summary). ` + "`data validate`" + ` checks an ingest.yaml
locally first.

` + "`tracebloc cluster info`" + ` is the pre-flight you'd typically run
before the first ingest.`,
		// A bare `tracebloc data` prints help; a mistyped subcommand errors with a
		// suggestion instead of silently exiting 0 (#75).
		RunE:                       runGroup,
		SuggestionsMinimumDistance: 2,
	}
	cmd.AddCommand(newDataIngestCmd())
	cmd.AddCommand(newDataListCmd())
	cmd.AddCommand(newDataDeleteCmd())
	cmd.AddCommand(newIngestValidateCmd())
	return cmd
}

// newDataIngestCmd implements `tracebloc data ingest <local-path>`.
//
// Phase 3 scope (now complete across PR-a + PR-b):
//
//   - Synthesize the ingest spec from flags (`internal/push.SpecArgs.Build`)
//   - Validate it against the embedded ingest.v1 schema
//   - Walk the local directory + enforce v0.1 size caps
//   - Discover cluster, parent release, and shared PVC
//   - Print a single-screen pre-flight summary
//   - Either --dry-run stop, OR create an ephemeral stage Pod
//     (alpine 3.20 pinned by digest, PSA-restricted security
//     context), tar local files into it via an SPDY exec stream
//     with a progress bar, then defer-delete the Pod
//
// Phase 4 (`tracebloc/client#152`) hooks the submit-to-jobs-manager
// step into the bottom of this command, replacing the "manually
// kick off helm ingestor" workaround in the success message.
//
// Aliases: "push" is kept for one deprecation cycle so existing
// scripts continue to work.
func newDataIngestCmd() *cobra.Command {
	var (
		// Kubeconfig flags — same conventions as `cluster info`.
		// Promoting these to persistent on the root is a v0.2
		// follow-up (tracebloc/cli#3); for now they live on each
		// command that needs them.
		kubeconfigPath  string
		contextOverride string
		nsOverride      string

		// Ingest-spec flags. image_classification + the tabular /
		// time-series family are supported today; text + detection +
		// segmentation land in later increments.
		table             string
		category          string
		intent            string
		labelColumn       string
		targetSize        string
		schemaFlag        string
		labelPolicy       string
		timeColumn        string
		numberOfKeypoints int

		// Operations flags.
		dryRun     bool
		overwrite  bool
		noInput    bool
		outputJSON bool

		// Stage Pod image override. Defaults to the digest-pinned
		// alpine that ships with the CLI; air-gapped customers
		// override this to an image their registry mirror serves.
		// Pin by digest in your override too — tag-only references
		// drift silently and break "all my pushes worked yesterday."
		stagePodImage string

		// Phase 4 flags. --detach exits immediately after the 201
		// from jobs-manager; --idempotency-key plumbs through to
		// the submit body for retry-safety across CLI invocations
		// (default: fresh per call); --image-digest pins the
		// ingestor image (default: jobs-manager picks the
		// cluster-configured one).
		detach         bool
		idempotencyKey string
		imageDigest    string
	)

	cmd := &cobra.Command{
		Use:     "ingest <local-path>",
		Aliases: []string{"push"},
		Short:   "Stage a local dataset into your client's storage",
		Long: `Stages a local dataset into your client's shared storage,
submits an ingestion run to jobs-manager, and watches the ingestor Job
to completion. Supports 9 task categories (image classification,
object/keypoint detection, text classification, masked language
modeling, and the tabular / time-series family); pick one with --category.

Expected local layout (image_classification shown):

    <local-path>/
      labels.csv             (required)
      images/                (required)
        001.jpg
        002.jpg
        ...

Accepted image extensions: .jpg, .jpeg, or .png (case-insensitive).
All images in one dataset must share a single type — the cluster
validates the type it was told to expect.

v0.1 caps the dataset at 1 GiB total + 500 MiB per file. Larger
datasets need the v0.2 cloud-source story (S3/GCS/HTTPS sources) —
see tracebloc/client#147 non-goals.

Exit codes:
  0   files staged + ingested successfully (or --detach: just staged + submitted)
  2   schema validation failed (synthesized spec rejected) or
      v0.1-unsupported category passed
  3   local-layout or kubeconfig error
  4   cluster reachable but no tracebloc client / shared storage missing
  5   ingestor SA token couldn't be obtained, or jobs-manager
      rejected the token (401/403)
  6   destination table already exists (re-run with --overwrite to
      replace it, or pick a different --table)
  7   pre-flight succeeded but staging the files failed
      (Pod creation, image pull, exec stream, or remote tar error) —
      or, with --overwrite, removing the old table failed
  8   jobs-manager rejected the submit (4xx/5xx other than auth)
  9   ingestion Job exited non-zero, or completed with row-level
      failures the summary panel reports`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var localPath string
			if len(args) > 0 {
				localPath = args[0]
			}
			// Guided mode: on a terminal (and unless --no-input), prompt
			// for whatever's still missing. Off a TTY / with --no-input,
			// prompter stays nil and runDataIngest keeps flag-only
			// behavior.
			interactive := !noInput && !outputJSON && isInteractiveTTY()
			var pr prompter
			if interactive {
				pr = surveyPrompter{}
			}
			// In --output-json mode, human output goes to stderr so
			// stdout carries only the JSON result.
			humanOut := cmd.OutOrStdout()
			printer := printerFor(cmd)
			var jsonOut io.Writer
			if outputJSON {
				humanOut = cmd.ErrOrStderr()
				printer = printerForWriter(cmd, cmd.ErrOrStderr())
				jsonOut = cmd.OutOrStdout()
			}
			return runDataIngest(cmd.Context(), humanOut, cmd.ErrOrStderr(),
				runDataIngestArgs{
					LocalPath:  localPath,
					Kubeconfig: kubeconfigPath,
					Context:    contextOverride,
					Namespace:  nsOverride,
					Spec: push.SpecArgs{
						Table: table, Category: category, Intent: intent,
						LabelColumn: labelColumn, LabelPolicy: labelPolicy, TimeColumn: timeColumn,
						NumberOfKeypoints: numberOfKeypoints,
					},
					TargetSizeFlag: targetSize,
					SchemaFlag:     schemaFlag,
					DryRun:         dryRun,
					Overwrite:      overwrite,
					StagePodImage:  stagePodImage,
					Detach:         detach,
					IdempotencyKey: idempotencyKey,
					ImageDigest:    imageDigest,
					Printer:        printer,
					Interactive:    interactive,
					Prompter:       pr,
					CategorySet:    cmd.Flags().Changed("category"),
					OutputJSON:     outputJSON,
					JSONOut:        jsonOut,
				})
		},
	}

	cmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	cmd.Flags().StringVar(&contextOverride, "context", "",
		"name of the kubeconfig context to use (default: kubeconfig's current-context)")
	cmd.Flags().StringVarP(&nsOverride, "namespace", "n", "",
		"namespace where your tracebloc client is installed")

	// Required spec flags. We DON'T mark them required-at-cobra-level
	// because cobra's "required flag" error message is terse and
	// pre-empts our richer schema-driven diagnostic. Instead, the
	// schema validator catches missing/empty values with the canonical
	// JSON-pointer-anchored error.
	cmd.Flags().StringVar(&table, "table", "",
		"destination table name (MySQL identifier; matches /data/shared/<table>/ on the PVC)")
	cmd.Flags().StringVar(&category, "category", "image_classification",
		"task category, one of: "+push.SupportedCategoriesList())
	cmd.Flags().StringVar(&intent, "intent", "",
		"intent: train|test")
	cmd.Flags().StringVar(&labelColumn, "label-column", "",
		"name of the label/target column (in labels.csv for image categories, in the data CSV for tabular)")
	cmd.Flags().StringVar(&targetSize, "target-size", "",
		"image categories only: resolution as WxH (e.g. 512x512). Default: auto-detected from the first image. "+
			"All images must share this resolution — the ingestor validates it, it does not resize.")
	cmd.Flags().StringVar(&schemaFlag, "schema", "",
		"tabular/time-series only: column types as col:TYPE,col:TYPE (e.g. age:INT,price:FLOAT). "+
			"Default: inferred from the CSV (INT/FLOAT/VARCHAR).")
	cmd.Flags().StringVar(&labelPolicy, "label-policy", "",
		"regression-class only (tabular_regression, time_series_forecasting, time_to_event_prediction): "+
			"passthrough|bucket (default bucket — bins the target so the raw value never leaves the cluster)")
	cmd.Flags().StringVar(&timeColumn, "time-column", "",
		"time_to_event_prediction only: name of the time/duration column (default: a column named \"time\")")
	cmd.Flags().IntVar(&numberOfKeypoints, "number-of-keypoints", 0,
		"keypoint_detection only: number of keypoints per sample (required; e.g. 17 for COCO pose)")

	cmd.Flags().BoolVar(&overwrite, "overwrite", false,
		"replace the destination table if it already exists: its current table + files are removed first (same as `tracebloc data delete`), then the new data is ingested. Not combinable with --idempotency-key")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"validate + discover + walk, but don't create any cluster resources")
	cmd.Flags().BoolVar(&noInput, "no-input", false,
		"disable interactive prompts; fail on missing required values (for CI/scripts)")
	cmd.Flags().BoolVar(&outputJSON, "output-json", false,
		"emit a machine-readable JSON result on stdout (human output → stderr; implies --no-input)")
	cmd.Flags().StringVar(&stagePodImage, "stage-pod-image", "",
		"override the ephemeral stage Pod's image (default: digest-pinned alpine 3.20 baked into the CLI). "+
			"Pin by digest in your override too — tag-only refs drift silently.")

	cmd.Flags().BoolVar(&detach, "detach", false,
		"exit immediately after jobs-manager accepts the run (no log streaming, no summary panel). "+
			"Use for CI scenarios; reconnect later with `kubectl logs -f -n <ns> job/<name>`.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "",
		"reuse this idempotency key across retry attempts (default: fresh per invocation). "+
			"jobs-manager treats a duplicate key as a replay and attaches to the existing Job "+
			"rather than spawning a new one — useful for at-most-once-across-attempts semantics.")
	cmd.Flags().StringVar(&imageDigest, "image-digest", "",
		"pin the ingestor container image to a specific digest (default: jobs-manager picks the "+
			"cluster-configured `images.ingestor.digest`). Format: sha256:<hex>.")

	return cmd
}

// runDataIngestArgs collects every parameter runDataIngest needs,
// so the body stays testable without going through cobra. The cobra
// RunE wrapper above is the ONLY caller in production; tests
// construct one of these directly.
type runDataIngestArgs struct {
	LocalPath      string
	Kubeconfig     string
	Context        string
	Namespace      string
	Spec           push.SpecArgs
	TargetSizeFlag string // raw --target-size; resolved after Discover (image)
	SchemaFlag     string // raw --schema; resolved or inferred after Discover (tabular)
	DryRun         bool
	Overwrite      bool
	StagePodImage  string

	// Printer renders the pre-flight summary + status output. Built in
	// the RunE from the persistent --plain flag (see printerFor).
	Printer *ui.Printer

	// Interactive guided mode (#28). When Interactive is true,
	// runDataIngest prompts (via Prompter) for any missing core inputs
	// before validation. CategorySet records whether --category was
	// passed explicitly (its non-empty default would otherwise look
	// like a deliberate choice). Prompter is nil off a TTY / --no-input.
	Interactive bool
	Prompter    prompter
	CategorySet bool

	// OutputJSON routes human output to stderr and emits a JSON result
	// to JSONOut (stdout); set together by the RunE in --output-json
	// mode (which also forces non-interactive).
	OutputJSON bool
	JSONOut    io.Writer

	// Phase 4 (#152) fields. See the flag declarations for the
	// per-knob rationale; all three are optional.
	Detach         bool
	IdempotencyKey string
	ImageDigest    string
}

// expandHome expands a leading ~ or ~/… to $HOME, leaving every other
// path (relative, absolute, empty) untouched. It mirrors
// cluster.expandPath — kept as a small local copy rather than coupling
// the data path-handling to the cluster package's internals; if a
// third caller appears, promote both to a shared pathutil.
func expandHome(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Can't resolve $HOME — leave it and let the downstream
		// Discover* error mention the literal path, which is more
		// useful than a generic failure here.
		return path
	}
	// path[1:] is "" for "~" (→ home) and "/x" for "~/x" (→ home/x);
	// filepath.Join cleans the join either way.
	return filepath.Join(home, path[1:])
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
This uploads a dataset from your machine into your tracebloc workspace so models
can be trained on it. Your files are sent to the Kubernetes cluster your
workspace was installed on — tracebloc checks them and loads them into a table
your training runs read from. Your data stays on that cluster the whole time;
other collaborators train against it without ever seeing the raw files.`))
	a.Printer.Hintf("Learn more: https://docs.tracebloc.io")

	// 0. Guided mode: prompt for any missing core inputs before
	//    validation. Flags already provided win; non-TTY / --no-input
	//    leaves Prompter nil and skips straight to the flag-only path.
	if a.Interactive && a.Prompter != nil {
		if err := runInteractive(a.Printer, a.Prompter, &a, a.CategorySet); err != nil {
			if errors.Is(err, errInteractiveCancelled) {
				a.Printer.Infof("Cancelled — nothing was ingested.")
				return nil
			}
			return &exitError{code: 3, err: fmt.Errorf("interactive setup: %w", err)}
		}
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
	//    Supported today: image_classification + the tabular /
	//    time-series family. The other image categories need sidecar
	//    (annotation/mask) staging the CLI doesn't do yet, and the
	//    text family needs a texts/sequences dir — both land in later
	//    increments. A typo'd category also lands here with a clear
	//    list rather than the schema's 11-option enum dump.
	switch {
	case a.Spec.Category == "":
		// Left empty by a caller; let the schema produce the canonical
		// "category is required" error downstream.
	case push.IsCLISupported(a.Spec.Category):
		// supported
	case push.IsKnown(a.Spec.Category):
		// A recognized category data ingest doesn't implement yet — image
		// (semantic_segmentation) or text (causal_language_modeling, seq2seq,
		// …). Routed here (not the default branch) so the
		// user gets the registry's per-category pending-support reason, not a
		// misleading "unrecognized category". Supported categories were already
		// caught above, so IsKnown here means known-but-unsupported.
		spec, _ := push.Lookup(a.Spec.Category)
		return &exitError{code: 2, err: fmt.Errorf(
			"category %q isn't supported by the CLI yet (%s). Supported categories: %s.",
			a.Spec.Category, spec.UnsupportedNote, push.SupportedCategoriesList())}
	default:
		return &exitError{code: 2, err: fmt.Errorf(
			"category %q isn't a recognized task category. Supported categories: %s.",
			a.Spec.Category, push.SupportedCategoriesList())}
	}

	// 3. Walk the local directory FIRST (local "fail fast"), dispatched
	//    by category family. Image categories expect labels.csv +
	//    images/; tabular / time-series categories expect a single
	//    data CSV. The walk also yields what the per-category
	//    resolution below needs (the image list for target-size, the
	//    CSV for schema inference).
	// err is the function's named return (see the --output-json defer
	// at the top), so it's not redeclared here.
	var layout *push.LocalLayout
	switch {
	case push.IsTabular(a.Spec.Category):
		layout, err = push.DiscoverTabular(a.LocalPath)
	case push.IsText(a.Spec.Category):
		layout, err = push.DiscoverText(a.Spec.Category, a.LocalPath)
	case a.Spec.Category == "object_detection":
		layout, err = push.DiscoverObjectDetection(a.LocalPath)
	default:
		// image_classification + keypoint_detection: labels.csv + images/.
		layout, err = push.Discover(a.LocalPath)
	}
	if err != nil {
		return &exitError{code: 3, err: err}
	}

	a.Printer.Step(1, 4, "Check your dataset")
	a.Printer.Hintf("Reading your files locally first — nothing has touched the cluster yet — so a layout or settings problem shows up right away.")

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

		// Column schema: an explicit --schema wins; otherwise infer
		// INT/FLOAT/VARCHAR types from the CSV so the customer doesn't
		// hand-write one for the common case.
		if a.SchemaFlag != "" {
			sch, perr := push.ParseSchema(a.SchemaFlag)
			if perr != nil {
				return &exitError{code: 2, err: perr}
			}
			a.Spec.Schema = sch
		} else {
			sch, skipped, empty, ierr := push.InferSchema(layout.LabelsCSV)
			if ierr != nil {
				return &exitError{code: 3, err: fmt.Errorf("inferring schema from CSV: %w", ierr)}
			}
			a.Spec.Schema = sch
			_, _ = fmt.Fprintf(out,
				"Inferred schema for %d column(s) from %s (override with --schema).\n",
				len(sch), filepath.Base(layout.LabelsCSV))
			if len(skipped) > 0 {
				_, _ = fmt.Fprintf(out,
					"  (skipped framework-managed column(s): %s)\n", strings.Join(skipped, ", "))
			}
			if len(empty) > 0 {
				_, _ = fmt.Fprintf(out,
					"  (warning: %d column(s) had no values in the sample and were typed FLOAT (nullable): %s)\n",
					len(empty), strings.Join(empty, ", "))
			}
		}
	case push.IsImage(a.Spec.Category):
		// keypoint_detection needs --number-of-keypoints (dataset-
		// specific, no default). Catch it here with an actionable
		// message rather than letting the ingestor fail mid-run.
		if a.Spec.Category == "keypoint_detection" && a.Spec.NumberOfKeypoints <= 0 {
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
		// Text family: no extra per-category resolution. The label (for
		// text_classification) comes straight from --label-column;
		// masked_language_modeling needs neither a label nor a schema.
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
	a.Printer.Step(2, 4, "Connect to your workspace")
	a.Printer.Hintf("Finding your tracebloc workspace and the shared storage your dataset will live on.")
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

	// 7. Show what we found on the cluster — the customer's last look
	//    before any bytes move.
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
		return &exitError{code: 6, err: fmt.Errorf(
			"table %q already exists in this client. Re-ingesting the same table doesn't merge or replace — "+
				"the run would fail after uploading everything. Re-run with --overwrite to replace it, "+
				"or pick a different --table. (`tracebloc data delete %s` also removes it.)",
			existingTable, existingTable)}
	}
	if tableExists && a.Overwrite {
		a.Printer.Warnf("Table %q already exists — --overwrite replaces it (table + files).", existingTable)
	}

	// 8. Dry-run stop. Acknowledged success, plus a reminder of the
	//    live-only steps (stage + ingest) the customer just skipped.
	if a.DryRun {
		a.Printer.Newline()
		a.Printer.Successf("Dry-run complete — your dataset and cluster check out; nothing was created.")
		a.Printer.Hintf("A real run continues with step 3 (stage your files) and step 4 (run the ingestion).")
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
		a.Printer.Infof("Removing the existing %q first…", existingTable)
		plan := push.PlanTeardown(existingTable)
		if _, terr := push.Teardown(ctx, cs, &push.SPDYExecutor{Config: resolved.RestConfig, Client: cs}, resolved.Namespace, plan, push.PodSpecOptions{
			Namespace:          resolved.Namespace,
			PVCClaimName:       pvc.ClaimName,
			PVCMountPath:       pvc.MountPath,
			Table:              existingTable,
			ServiceAccountName: release.IngestorSAName,
			Image:              a.StagePodImage,
		}); terr != nil {
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
	a.Printer.Step(3, 4, "Stage your files")
	a.Printer.Hintf("Your files upload securely into your workspace's storage — set up and cleaned up for you.")
	progress := push.NewProgress(out, layout.TotalBytes,
		fmt.Sprintf("Staging %s", a.Spec.Table))
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

// runIngestionRun is the money path's outcome tail. It mints the ingestor
// token, port-forwards to jobs-manager, POSTs the run, classifies the result
// into a status + process exit code (kept in lockstep by classifyPushOutcome),
// emits the machine-readable JSON in --output-json mode, and reclaims the
// staged source copy on a clean success only.
//
// Split out of runDataIngest purely for testability: the four cluster-touching
// steps go through package-level seams (mintIngestorTokenFn /
// portForwardJobsManagerFn / submitRunFn / cleanStagingFn), so a table test can
// drive the full classify → exit-code → JSON → reclaim matrix — including the
// "must NOT reclaim on partial failure" gate — without standing up a cluster
// (#1009).
//
// Returns jsonEmitted so runDataIngest's --output-json error defer knows
// whether a result object already reached stdout: the mint / port-forward
// failures return before the emit and rely on that defer; the submit path
// always emits.
func runIngestionRun(ctx context.Context, out io.Writer, a runDataIngestArgs, target *clusterTarget, specBytes []byte, spec map[string]any) (jsonEmitted bool, err error) {
	resolved, cs, release, pvc := target.Resolved, target.Clientset, target.Release, target.PVC

	// 10. Mint the SA token Phase 4 uses to authenticate the POST
	//     to jobs-manager. Expiry is 1 hour (vs cluster info's 10
	//     min) because the full Phase 4 lifecycle — submit + watch
	//     + log stream — can run that long for large ingestions.
	//     The chart's helm flow uses the same token-mint code path.
	a.Printer.Step(4, 4, "Run the ingestion")
	if a.Detach {
		a.Printer.Hintf("Submitting the run — with --detach it keeps running on your workspace after this command returns; the reconnect command is shown below.")
	} else {
		a.Printer.Hintf("Submitting the run, then following along as tracebloc validates your data and loads it into the table — progress streams below.")
		a.Printer.Hintf("This follows the run for up to an hour; a longer run keeps going on its own (or start it with --detach and check back later).")
	}
	tok, err := mintIngestorTokenFn(ctx, cs, resolved.Namespace,
		release.IngestorSAName, 3600, nil)
	if err != nil {
		return false, &exitError{code: 5, err: err}
	}

	// 11. Open a port-forward to a Pod backing the jobs-manager
	//     Service. The CLI runs off-cluster (on a laptop, in CI
	//     runners outside the cluster network), so the discovered
	//     *.svc.cluster.local URL isn't reachable — we tunnel
	//     through the kubeconfig-authenticated apiserver, same as
	//     `kubectl port-forward`. Bugbot PR #10 r3 caught the
	//     original broken-by-design direct-URL POST.
	a.Printer.Infof("Connecting to your workspace to submit the run…")
	pf, err := portForwardJobsManagerFn(ctx, cs, resolved.RestConfig,
		resolved.Namespace, release.JobsManagerServiceName, release.JobsManagerPort)
	if err != nil {
		return false, &exitError{code: 8, err: fmt.Errorf("setting up jobs-manager port-forward: %w", err)}
	}
	defer pf.Close()

	// 12. Phase 4: POST to jobs-manager via the local port,
	//     watch the spawned ingestor Job, render the parsed
	//     INGESTION SUMMARY panel.
	//
	//     Exit-code mapping:
	//        SubmitError 401/403         → 5 (auth — same bucket as
	//                                       token-mint, shared
	//                                       "your SA can't do this"
	//                                       diagnostic class)
	//        SubmitError other 4xx/5xx   → 8 (submit failed)
	//        WatchResult Failed          → 9 (ingest failed)
	//        WatchResult Succeeded +
	//          summary.HasFailures()     → 9 (some rows failed
	//                                       even though Job exited 0;
	//                                       the ingestor surfaces
	//                                       partial-failure summaries)
	//        WatchResult Detached        → 0 (cluster keeps running)
	//        WatchResult Succeeded clean → 0
	localEndpoint := fmt.Sprintf("http://localhost:%d", pf.LocalPort)
	submitRes, err := submitRunFn(ctx, submit.Options{
		Submitter:        submit.NewHTTPSubmitter(localEndpoint, tok.Token),
		Client:           cs,
		IngestConfigYAML: string(specBytes),
		IdempotencyKey:   a.IdempotencyKey,
		ImageDigest:      a.ImageDigest,
		Detach:           a.Detach,
		Out:              out,
		Printer:          a.Printer,
	})
	// Classify once: a machine-readable status + the process exit error
	// in lockstep, so --output-json emits exactly one result object on
	// EVERY path (success / partial / failure / submit-or-watch error)
	// whose status matches the exit code. (Bugbot #38.)
	status, exitErr := classifyPushOutcome(submitRes, err)

	// Emit the machine-readable result BEFORE the best-effort staging
	// reclaim below, so a scripted --output-json consumer gets its result
	// object at ingest-completion latency and never waits on a slow
	// cluster-side cleanup that has no bearing on the ingest outcome.
	if a.OutputJSON {
		var summary *submit.Summary
		var ns, jobName string
		if submitRes != nil {
			if submitRes.Watch != nil {
				summary = submitRes.Watch.Summary
			}
			if submitRes.Submit != nil {
				ns, jobName = submitRes.Submit.Namespace, submitRes.Submit.JobName
			}
		}
		writePushJSON(a.JSONOut, status, spec, summary, ns, jobName)
		jsonEmitted = true
	}

	// Reclaim the staged source copy on a CLEAN success only (see
	// shouldReclaimStaging). Best-effort and time-bounded
	// (push.StagingCleanupTimeout): a failed or slow reclaim must not
	// fail — or noticeably delay — a successful ingest.
	if shouldReclaimStaging(status) {
		a.Printer.Infof("Reclaiming the temporary staging copy on the cluster…")
		if cerr := cleanStagingFn(ctx, cs,
			&push.SPDYExecutor{Config: resolved.RestConfig, Client: cs},
			resolved.Namespace, a.Spec.Table, push.PodSpecOptions{
				Namespace:          resolved.Namespace,
				PVCClaimName:       pvc.ClaimName,
				PVCMountPath:       pvc.MountPath,
				Table:              a.Spec.Table,
				ServiceAccountName: release.IngestorSAName,
				Image:              a.StagePodImage,
			}); cerr != nil {
			a.Printer.Warnf("Couldn't reclaim the temporary staging copy (%v). It's harmless — the next re-ingest of %q or a `tracebloc data delete %s` will clear it.",
				cerr, a.Spec.Table, a.Spec.Table)
		}
	}

	if exitErr != nil {
		return jsonEmitted, exitErr
	}
	return jsonEmitted, nil
}

// shouldReclaimStaging reports whether the staged source copy should be
// reclaimed after the run. ONLY on a clean success: the ingestor copies (not
// moves) the staged files into the table, so leaving .tracebloc-staging/<table>
// behind doubles PVC use for file-bearing datasets until the next --overwrite
// or `data delete` (the staging-leak found by the ingest UX audit; cli#166 /
// epic #67). Everything else keeps the source:
//   - a detached run ("detached") — the Job is still reading it;
//   - a partial ("completed_with_failures") or failed/errored run — the user
//     may want the source to inspect or retry.
//
// This is the "must NOT reclaim on partial failure" gate (#1009), named so the
// invariant is table-testable in isolation.
func shouldReclaimStaging(status string) bool {
	return status == "succeeded"
}

// classifyPushOutcome maps the result of submit.Run to a machine-
// readable status string + the process exit error, kept in lockstep so
// --output-json's status always agrees with the exit code (a nil
// *exitError = success, exit 0). It also covers the error paths
// (auth/submit/watch) so --output-json can still emit a result object
// when submit.Run returns an error. (Bugbot #38.)
func classifyPushOutcome(res *submit.Result, err error) (string, *exitError) {
	if err != nil {
		switch {
		case submit.IsAuthError(err):
			return "auth_error", &exitError{code: 5, err: err}
		case submit.IsWatchError(err):
			// jobs-manager accepted the run; the cluster is doing the
			// work, the CLI just couldn't follow along — ingest-side
			// (exit 9), not submit-side (8).
			return "watch_error", &exitError{code: 9, err: err}
		default:
			return "submit_error", &exitError{code: 8, err: err}
		}
	}
	// --detach (no watch) or SIGINT-mid-watch: success; cluster runs on.
	if res == nil || res.Watch == nil || res.Watch.Outcome == submit.JobOutcomeDetached {
		return "detached", nil
	}
	switch res.Watch.Outcome {
	case submit.JobOutcomeFailed:
		return "failed", &exitError{code: 9, err: errors.New("ingestion Job exited non-zero — see logs above")}
	case submit.JobOutcomeUnknown:
		return "unknown", &exitError{code: 9, err: errors.New(
			"ingestion Job's final status couldn't be determined within the watch window — " +
				"check `kubectl get job -n " + res.Submit.Namespace + " " + res.Submit.JobName + "` for the outcome")}
	case submit.JobOutcomeSucceeded:
		// Job exited 0, but rows can still have failed — exit 9, and the
		// JSON status must say so, NOT "succeeded". (Bugbot #38.)
		if res.Watch.Summary != nil && res.Watch.Summary.HasFailures() {
			return "completed_with_failures", &exitError{code: 9, err: errors.New(
				"ingestion Job completed but the summary reports failures — see panel above")}
		}
		return "succeeded", nil
	}
	return "unknown", nil
}

// printLocalSummary shows what the CLI found on disk plus the ingest
// settings it assembled — the detail under step 1 ("Check your
// dataset"). Split from the cluster summary so each sits under its own
// numbered step. Mirrors `cluster info`'s section/Field layout.
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
		if _, ok := layout.ExtraFiles["tokenizer.json"]; ok {
			p.Field("tokenizer", "tokenizer.json")
		}
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
	}
	p.Field("total size", push.HumanBytes(layout.TotalBytes))

	p.Section("Ingest settings")
	p.Field("table", fmt.Sprintf("%v", spec["table"]))
	p.Field("category", fmt.Sprintf("%v", spec["category"]))
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

// printClusterSummary shows the discovered workspace cluster target —
// the detail under step 2 ("Connect to your workspace").
func printClusterSummary(p *ui.Printer, release *cluster.ParentRelease, pvc *cluster.SharedPVC) {
	p.Section("Target cluster")
	p.Field("release", fmt.Sprintf("%s (chart %s)", release.ReleaseName, release.ChartVersion))
	p.Field("jobs-manager", release.JobsManagerService)
	p.Field("shared PVC", fmt.Sprintf("%s (%s)", pvc.ClaimName, pvc.Phase))
	if !pvc.IsReadWriteMany() {
		// Warn but don't block — RWO clusters still work; the scheduler
		// co-locates the stage Pod with the existing mounter.
		p.Warnf("PVC is %v, not ReadWriteMany — the stage Pod will co-locate with the existing mounter", pvc.AccessModes)
	}
}

// pushJSONResult is the machine-readable shape emitted by --output-json.
// It's a presentation type owned by the CLI layer, so submit.Summary
// stays json-tag-free and this wire format can evolve independently.
type pushJSONResult struct {
	Status    string           `json:"status"` // dry-run|succeeded|completed_with_failures|failed|detached|unknown|auth_error|submit_error|watch_error|error
	Table     string           `json:"table"`
	Category  string           `json:"category"`
	Intent    string           `json:"intent"`
	Namespace string           `json:"namespace,omitempty"`
	JobName   string           `json:"job_name,omitempty"`
	Summary   *pushJSONSummary `json:"summary,omitempty"`
	Error     string           `json:"error,omitempty"`
	ExitCode  int              `json:"exit_code,omitempty"`
}

type pushJSONSummary struct {
	IngestorID           string  `json:"ingestor_id,omitempty"`
	TotalRecords         int64   `json:"total_records"`
	InsertedRecords      int64   `json:"inserted_records"`
	SentToAPI            int64   `json:"sent_to_api"`
	SkippedRecords       int64   `json:"skipped_records"`
	FileTransferFailures int64   `json:"file_transfer_failures"`
	DBInsertFailures     int64   `json:"db_insert_failures"`
	SuccessRate          float64 `json:"success_rate"`
}

// writePushJSON serializes the push result to w (stdout in
// --output-json mode). Errors are dropped: marshaling our own struct
// can't fail in practice, and the exit code remains the contract.
func writePushJSON(w io.Writer, status string, spec map[string]any, s *submit.Summary, ns, jobName string) {
	res := pushJSONResult{
		Status:    status,
		Table:     fmt.Sprintf("%v", spec["table"]),
		Category:  fmt.Sprintf("%v", spec["category"]),
		Intent:    fmt.Sprintf("%v", spec["intent"]),
		Namespace: ns,
		JobName:   jobName,
	}
	if s != nil {
		res.Summary = &pushJSONSummary{
			IngestorID:           s.IngestorID,
			TotalRecords:         s.TotalRecords,
			InsertedRecords:      s.InsertedRecords,
			SentToAPI:            s.APISentRecords,
			SkippedRecords:       s.SkippedRecords,
			FileTransferFailures: s.FileTransferFailures,
			DBInsertFailures:     s.FailedRecords,
			SuccessRate:          s.SuccessRate(),
		}
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, string(b))
}

// writePushErrorJSON emits a JSON error object for --output-json runs
// that fail before a result is produced (validation, discovery,
// staging, token, port-forward). Keeps the stdout-always-JSON contract
// so a script parsing it never hits empty output on failure. (Bugbot #49)
func writePushErrorJSON(w io.Writer, sp push.SpecArgs, e error, code int) {
	res := pushJSONResult{
		Status:   "error",
		Table:    sp.Table,
		Category: sp.Category,
		Intent:   sp.Intent,
		Error:    e.Error(),
		ExitCode: code,
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, string(b))
}

// listDatasetsFn is a test seam over push.ListDatasets.
var listDatasetsFn = push.ListDatasets

// Test seams over the cluster-touching steps of runIngestionRun (#1009).
// Production wires them to the real functions; a table test overrides them to
// drive the classify → exit-code → JSON → reclaim matrix without a cluster
// (mirrors the listDatasetsFn seam). cleanStagingFn is here too so a test can
// observe whether the staging reclaim ran (the must-NOT-reclaim gate).
var (
	mintIngestorTokenFn      = cluster.MintIngestorToken
	portForwardJobsManagerFn = submit.PortForwardJobsManager
	submitRunFn              = submit.Run
	cleanStagingFn           = push.CleanStaging
)

// destTableExists reports whether the destination table already holds an
// ingested dataset, via the same query `data list` uses. It fails OPEN: a
// broken check returns (false, note) so the ingest proceeds — the in-cluster
// duplicate check still backstops it — but the note tells the user the guard
// didn't run rather than silently skipping it.
// The first return is the EXISTING table's exact name ("" when absent):
// matching is case-insensitive (mysql's catalog may be), but any teardown
// must act on the real spelling — DROP/rm against the flag's casing would
// silently no-op on case-sensitive systems and then claim success.
func destTableExists(ctx context.Context, cs kubernetes.Interface, resolved *cluster.ResolvedConfig, table string) (string, string) {
	names, err := listDatasetsFn(ctx, cs, resolved.RestConfig, resolved.Namespace)
	if err != nil {
		return "", fmt.Sprintf("(couldn't check whether %q already exists — continuing; the cluster still refuses duplicates: %v)", table, err)
	}
	for _, n := range names {
		if strings.EqualFold(n, table) {
			return n, ""
		}
	}
	return "", ""
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
	code := 3
	if problem.BadFlag {
		code = 2
	}
	return &exitError{code: code, err: problem.Err}
}
