// The `data ingest` command surface: cobra wiring, the full flag set
// (canonical + hidden deprecated aliases), and the args struct the
// testable runDataIngest body consumes. Moved verbatim from data.go
// (cli#282) — behavior unchanged.

package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// newDataIngestCmd implements `tracebloc data ingest <dataset>`.
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

		// Ingest-spec flags. All schema task categories are CLI-supported now
		// (image classification / detection / segmentation / keypoint, the full
		// text family, and the tabular / time-series family).
		//
		// --name/--task are the canonical flags (#180); --table/--category
		// stay on as hidden deprecated aliases so existing scripts keep
		// working. --intent is unchanged. The wire/spec field names don't
		// change — this is a CLI-surface rename only.
		name              string
		tableAlias        string
		task              string
		categoryAlias     string
		intent            string
		labelColumn       string
		targetSize        string
		minSize           string
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
		Use:     "ingest <dataset>",
		Aliases: []string{"push"},
		Short:   "Ingest a local dataset into your workspace",
		// The task COUNT and the text-family subdir names are derived from the
		// registry / vendored layout contract (push.SupportedCategoryIDs +
		// push.TextSidecarDir) rather than hardcoded, so the help can't drift
		// from what the CLI actually supports (cli#215): the count used to read
		// a stale "9", and the text example showed texts/ for every text task
		// even though masked_language_modeling stages into sequences/.
		Long: fmt.Sprintf(`Ingests a local dataset into your workspace's storage,
submits the ingestion run, and follows it to completion (streaming
progress + the final summary). Your data never leaves your own
infrastructure. Supports %[1]d tasks across the image, text, and
tabular / time-series families; pick one with --task.

<dataset> is the data itself. What it looks like depends on the task:

  tabular / time-series — the dataset is a single CSV. Pass the .csv
  file directly, or a folder holding exactly one .csv:

      churn.csv                (the .csv file itself)
    or
      churn/
        data.csv               (the one .csv in the folder)

  image (classification, object/keypoint detection) — a folder with
  labels.csv + an images/ subfolder:

      cats_dogs/
        labels.csv             (required)
        images/                (required)
          001.jpg
          ...

  text (classification, masked language modeling) — a folder with
  labels.csv + a %[2]s/ subfolder (masked language modeling uses %[3]s/):

      reviews/
        labels.csv             (required)
        %[2]s/                 (required — %[3]s/ for masked language modeling)
          001.txt
          ...

A bare .csv file is accepted only for the tabular / time-series family;
image and text datasets must be a folder.

Accepted image extensions: .jpg, .jpeg, or .png (case-insensitive).
All images in one dataset must share a single type — the cluster
validates the type it was told to expect.

v0.1 caps the dataset at 1 GiB total + 500 MiB per file. Larger
datasets need the v0.2 cloud-source story (S3/GCS/HTTPS sources) —
see tracebloc/client#147 non-goals.

Exit codes:
  0   files staged + ingested successfully (or --detach: just staged + submitted)
  2   schema validation failed (synthesized spec rejected) or
      v0.1-unsupported task passed
  3   local-layout or kubeconfig error
  4   cluster reachable but no tracebloc client / shared storage missing
  5   ingestor SA token couldn't be obtained, or jobs-manager
      rejected the token (401/403)
  6   destination table already exists (re-run with --overwrite to
      replace it, or pick a different --name)
  7   pre-flight succeeded but staging the files failed
      (Pod creation, image pull, exec stream, or remote tar error) —
      or, with --overwrite, removing the old table failed
  8   jobs-manager rejected the submit (4xx/5xx other than auth)
  9   ingestion Job exited non-zero, or completed with row-level
      failures the summary panel reports`,
			len(push.SupportedCategoryIDs()),
			push.TextSidecarDir("text_classification"),
			push.TextSidecarDir("masked_language_modeling")),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var localPath string
			if len(args) > 0 {
				localPath = args[0]
			}
			// Resolve the deprecated flag aliases (#180): the canonical
			// flag wins; a hidden legacy alias fills in only when the new
			// flag wasn't passed, so old scripts keep working without the
			// new surface silently shadowing them. The wire/spec field
			// names are unchanged — this is a CLI rename only.
			nameVal := name
			if cmd.Flags().Changed("table") && !cmd.Flags().Changed("name") {
				nameVal = tableAlias
			}
			taskVal := task
			if cmd.Flags().Changed("category") && !cmd.Flags().Changed("task") {
				taskVal = categoryAlias
			}
			// Whether the task was chosen at all (via either spelling).
			// Dropping --task's old image_classification default means an
			// unset task now drives the picker (TTY) or a clear error
			// (non-interactive), never a silent image assumption.
			taskSet := cmd.Flags().Changed("task") || cmd.Flags().Changed("category")
			// Record whether --number-of-keypoints was explicitly passed, so
			// the keypoint set-vs-unset message (#76b) can distinguish an
			// explicit zero value from an unset flag (both look like the Go
			// zero value in the spec).
			changedFlags := map[string]bool{}
			if cmd.Flags().Changed("number-of-keypoints") {
				changedFlags["number-of-keypoints"] = true
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
						Table: nameVal, Category: taskVal, Intent: intent,
						LabelColumn: labelColumn, LabelPolicy: labelPolicy, TimeColumn: timeColumn,
						NumberOfKeypoints: numberOfKeypoints,
					},
					TargetSizeFlag: targetSize,
					MinSizeFlag:    minSize,
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
					TaskSet:        taskSet,
					ChangedFlags:   changedFlags,
					OutputJSON:     outputJSON,
					JSONOut:        jsonOut,
				})
		},
	}

	addKubeconfigFlags(cmd, &kubeconfigPath, &contextOverride, kubeconfigFlagUsage, contextFlagUsage)
	addNamespaceFlag(cmd, &nsOverride, namespaceFlagUsage)

	// Required spec flags. We DON'T mark them required-at-cobra-level
	// because cobra's "required flag" error message is terse and
	// pre-empts our richer schema-driven diagnostic. Instead, the
	// schema validator catches missing/empty values with the canonical
	// JSON-pointer-anchored error.
	cmd.Flags().StringVar(&name, "name", "",
		"a name for this dataset — start with a letter or underscore, then letters/digits/underscores — you'll reference it by this name when you start a training run")
	cmd.Flags().StringVar(&tableAlias, "table", "",
		"deprecated alias for --name")
	_ = cmd.Flags().MarkHidden("table")
	cmd.Flags().StringVar(&task, "task", "",
		"the task this data is for, one of: "+push.SupportedCategoriesList()+
			". Omit it on a terminal to pick interactively.")
	cmd.Flags().StringVar(&categoryAlias, "category", "",
		"deprecated alias for --task")
	_ = cmd.Flags().MarkHidden("category")
	cmd.Flags().StringVar(&intent, "intent", "",
		"is this training or test data? train|test (default train)")
	cmd.Flags().StringVar(&labelColumn, "label-column", "",
		"name of the label/target column (in labels.csv for image tasks, in the data CSV for tabular)")
	cmd.Flags().StringVar(&targetSize, "target-size", "",
		"image tasks only: the resolution your images already are, as WxH (e.g. 512x512). tracebloc never "+
			"resizes — it checks every image is exactly this size and rejects any that differ. Default: "+
			"read from your first image.")
	cmd.Flags().StringVar(&minSize, "min-size", "",
		"image tasks only: reject images smaller than WxH before the ingest (e.g. 64x64). Set it to the "+
			"smallest size your model can train on — raise or lower it freely. Default: unset (no local "+
			"size check).")
	cmd.Flags().StringVar(&schemaFlag, "schema", "",
		"tabular/time-series only: column types as col:TYPE,col:TYPE (e.g. age:INT,price:FLOAT). "+
			"Default: inferred from the CSV (INT/BIGINT/FLOAT/BOOLEAN/DATE/DATETIME/VARCHAR(n)).")
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
	MinSizeFlag    string // raw --min-size; resolved after Discover (image) — #348 floor override
	SchemaFlag     string // raw --schema; resolved or inferred after Discover (tabular)
	DryRun         bool
	Overwrite      bool
	StagePodImage  string

	// Printer renders the pre-flight summary + status output. Built in
	// the RunE from the persistent --plain flag (see printerFor).
	Printer *ui.Printer

	// Interactive guided mode (#28). When Interactive is true,
	// runDataIngest prompts (via Prompter) for any missing core inputs
	// before validation. TaskSet records whether the task was passed
	// explicitly (via --task or the hidden --category alias); an unset
	// task drives the picker rather than assuming a default. Prompter is
	// nil off a TTY / --no-input.
	Interactive bool
	Prompter    prompter
	TaskSet     bool

	// ChangedFlags records which CLI flags were EXPLICITLY set
	// (cmd.Flags().Changed), decoupling "was it passed" from "is its value
	// non-zero" — the value alone can't tell `--number-of-keypoints 0` (an
	// explicit, invalid value) from an unset flag (also 0). The RunE
	// populates it for --number-of-keypoints; the keypoint set-vs-unset
	// diagnostic (#76b) reads it. Nil in direct-construction tests that
	// don't exercise that path.
	ChangedFlags map[string]bool

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
