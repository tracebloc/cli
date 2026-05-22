package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/schema"
)

// newDatasetCmd wires the `tracebloc dataset` subtree. The dominant
// verb is `push`, introduced in Phase 3 (tracebloc/client#151) and
// landing across two PRs: PR-a (this one) implements the
// no-op-safe pre-flight; PR-b adds the actual file streaming via
// an ephemeral Pod + tar-over-exec. Future verbs (`dataset list`,
// `dataset rm`) hang off this parent in v0.2.
func newDatasetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dataset",
		Short: "Manage datasets in the parent client release",
		Long: `Commands for staging and managing datasets on the cluster's
shared PVC.

Today: ` + "`dataset push`" + ` stages a local directory and (in PR-b)
submits an ingestion run. ` + "`tracebloc cluster info`" + ` is the
pre-flight you'd typically run before the first push.`,
	}
	cmd.AddCommand(newDatasetPushCmd())
	return cmd
}

// newDatasetPushCmd implements `tracebloc dataset push <local-path>`.
//
// PR-a scope (what this implements today):
//
//   - Synthesize the ingest spec from flags (`internal/push.SpecArgs.Build`)
//   - Validate it against the embedded ingest.v1 schema
//   - Walk the local directory + enforce v0.1 size caps
//   - Discover cluster, parent release, and shared PVC
//   - Print a single-screen "ready to stage" summary
//   - --dry-run stops here (and so does this PR — actual staging
//     errors out with "coming in PR-b" until #151 PR-b merges)
//
// PR-b will add the ephemeral stage Pod, tar-over-exec stream,
// progress bar, and SIGINT-safe cleanup — slotting into the
// "TODO: PR-b" branch below without touching anything above it.
func newDatasetPushCmd() *cobra.Command {
	var (
		// Kubeconfig flags — same conventions as `cluster info`.
		// Promoting these to persistent on the root is a v0.2
		// follow-up (tracebloc/cli#3); for now they live on each
		// command that needs them.
		kubeconfigPath  string
		contextOverride string
		nsOverride      string

		// Ingest-spec flags. The set is intentionally
		// image_classification-only for v0.1 per epic #147
		// non-goals; other categories are one-PR additions in v0.2.
		table       string
		category    string
		intent      string
		labelColumn string

		// Operations flags.
		dryRun bool

		// Ingestor SA name override (only matters once PR-b mints
		// a token to talk to the future stage-pod-creation hook).
		// Plumbed today so PR-b doesn't have to touch flag wiring.
		ingestorSAName string
	)

	cmd := &cobra.Command{
		Use:   "push <local-path>",
		Short: "Stage a local dataset to the cluster's shared PVC",
		Long: `Stages a local image_classification dataset to the parent client
release's shared PVC, then (in PR-b) submits an ingestion run.

Expected local layout:

    <local-path>/
      labels.csv             (required)
      images/                (required)
        001.jpg
        002.jpg
        ...

Accepted image extensions: .jpg, .jpeg, .png, .webp (case-insensitive).

v0.1 caps the dataset at 1 GiB total + 500 MiB per file. Larger
datasets need the v0.2 cloud-source story (S3/GCS/HTTPS sources) —
see tracebloc/client#147 non-goals.

Exit codes:
  0   staged + (in PR-b) submitted successfully
  2   schema validation failed (synthesized spec rejected)
  3   local-layout or kubeconfig error
  4   cluster reachable but parent release missing
  6   pre-flight succeeded but the actual stage step isn't
      implemented yet (PR-b for #151 will deliver it)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDatasetPush(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				runDatasetPushArgs{
					LocalPath:      args[0],
					Kubeconfig:     kubeconfigPath,
					Context:        contextOverride,
					Namespace:      nsOverride,
					Spec:           push.SpecArgs{Table: table, Category: category, Intent: intent, LabelColumn: labelColumn},
					DryRun:         dryRun,
					IngestorSAName: ingestorSAName,
				})
		},
	}

	cmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	cmd.Flags().StringVar(&contextOverride, "context", "",
		"name of the kubeconfig context to use (default: kubeconfig's current-context)")
	cmd.Flags().StringVarP(&nsOverride, "namespace", "n", "",
		"namespace where the parent tracebloc/client release is installed")

	// Required spec flags. We DON'T mark them required-at-cobra-level
	// because cobra's "required flag" error message is terse and
	// pre-empts our richer schema-driven diagnostic. Instead, the
	// schema validator catches missing/empty values with the canonical
	// JSON-pointer-anchored error.
	cmd.Flags().StringVar(&table, "table", "",
		"destination table name (MySQL identifier; matches /data/shared/<table>/ on the PVC)")
	cmd.Flags().StringVar(&category, "category", "image_classification",
		"task category (v0.1 only supports image_classification; see tracebloc/client#147 non-goals)")
	cmd.Flags().StringVar(&intent, "intent", "",
		"intent: train|test")
	cmd.Flags().StringVar(&labelColumn, "label-column", "",
		"column name in labels.csv that holds the label")

	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"validate + discover + walk, but don't create any cluster resources")
	cmd.Flags().StringVar(&ingestorSAName, "ingestor-sa", "",
		"override the ingestor ServiceAccount name (default: \"ingestor\"); "+
			"set this if you customized ingestionAuthz.serviceAccountName in the parent client chart")

	return cmd
}

// runDatasetPushArgs collects every parameter runDatasetPush needs,
// so the body stays testable without going through cobra. The cobra
// RunE wrapper above is the ONLY caller in production; tests
// construct one of these directly.
type runDatasetPushArgs struct {
	LocalPath      string
	Kubeconfig     string
	Context        string
	Namespace      string
	Spec           push.SpecArgs
	DryRun         bool
	IngestorSAName string
}

// runDatasetPush is the PR-a slim implementation. It performs every
// pre-flight check and prints a summary; the actual file staging
// is gated behind a clear "not yet implemented" error so PR-a
// merging doesn't silently advertise a feature it can't deliver.
//
// Step order is "fail fast, fail local" — every step that doesn't
// need the cluster runs before any that does, so a customer with
// a bad label-column or oversized dataset gets the diagnostic in
// milliseconds without a kubeconfig round-trip.
func runDatasetPush(ctx context.Context, out, errOut io.Writer, a runDatasetPushArgs) error {
	// 1. Validate the table name BEFORE anything else. It's both
	//    the MySQL identifier and the /data/shared/<table>/ PVC
	//    subdirectory — an unsanitized traversal name (../../etc)
	//    would escape that subtree once PR-b's stage Pod writes to
	//    it. The embedded schema only checks minLength on `table`,
	//    so this CLI-side guard is the real fix. SpecArgs.Build()
	//    below calls StagedPrefix, which panics on an unsafe name —
	//    so this check MUST come first.
	if err := push.ValidateTableName(a.Spec.Table); err != nil {
		return &exitError{code: 2, err: err}
	}

	// 2. Synthesize the spec from flags + validate against schema.
	//    Catches "bad category", "missing intent" etc. BEFORE we
	//    touch the filesystem or the cluster. The error formatter
	//    is the same one ingest validate uses, so a customer who
	//    YAML'd manually first sees identical wording.
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

	// 3. Walk the local directory. Enforces layout + size caps;
	//    customer sees a clear pointer to expected layout if they
	//    pass the wrong directory.
	layout, err := push.Discover(a.LocalPath)
	if err != nil {
		return &exitError{code: 3, err: err}
	}

	// 4. Cluster discovery — same kubeconfig path as `cluster info`.
	//    Errors mirror that command's exit-code contract (3 for
	//    kubeconfig, 4 for missing release) so behaviour is
	//    consistent across pre-flight commands.
	resolved, err := cluster.Load(cluster.KubeconfigOptions{
		Path:      a.Kubeconfig,
		Context:   a.Context,
		Namespace: a.Namespace,
	})
	if err != nil {
		return &exitError{code: 3, err: fmt.Errorf("loading kubeconfig: %w", err)}
	}
	cs, err := cluster.NewClientset(resolved)
	if err != nil {
		return &exitError{code: 3, err: err}
	}
	release, err := cluster.DiscoverParentRelease(ctx, cs, resolved.Namespace)
	if err != nil {
		return &exitError{code: 4, err: err}
	}
	if a.IngestorSAName != "" {
		release.IngestorSAName = a.IngestorSAName
	}

	// 5. PVC discovery. New in this PR — confirms the chart's
	//    shared-data PVC is Bound before we waste time provisioning
	//    a Pod that can't mount it.
	pvc, err := cluster.DiscoverSharedPVC(ctx, cs, resolved.Namespace)
	if err != nil {
		return &exitError{code: 4, err: err}
	}

	// 6. Print the pre-flight summary. The output is the same in
	//    dry-run and (eventually) live mode — only the "what
	//    happens next" line differs. Customers iterating on a
	//    bad layout see this every attempt, so it's worth keeping
	//    skimmable: one fact per line, aligned by column.
	printPushPreflight(out, layout, release, pvc, spec, a.DryRun)

	// 7. Dry-run stop. Acknowledged success.
	if a.DryRun {
		_, _ = fmt.Fprintln(out, "Dry-run complete — no cluster resources were created.")
		return nil
	}

	// 8. The actual staging branch lands in PR-b. Failing here
	//    rather than silently returning success means a customer
	//    who pulled PR-a's binary and ran without --dry-run gets
	//    a clear "wait for PR-b" signal instead of "0 files
	//    transferred" confusion in Phase 4.
	return &exitError{code: 6, err: errors.New(
		"pre-flight succeeded but the actual file staging step isn't " +
			"implemented yet — wait for tracebloc/client#151 PR-b. " +
			"Re-run with --dry-run to validate without this error.")}
}

// printPushPreflight is the customer-facing summary. Mirrors
// `cluster info`'s layout for consistency: section header,
// indented key:value rows. Kept here (not on the layout/release/pvc
// types) because the formatting is policy and lives with the CLI,
// not the data.
func printPushPreflight(
	out io.Writer,
	layout *push.LocalLayout,
	release *cluster.ParentRelease,
	pvc *cluster.SharedPVC,
	spec map[string]any,
	dryRun bool,
) {
	// Explicit-discard the writer errors throughout — same rationale
	// as cli/cluster.go and cli/ingest.go: a pipe-write failure
	// shouldn't convert success into failure. The exit code is
	// the contract.
	_, _ = fmt.Fprintf(out, "Local dataset:\n")
	_, _ = fmt.Fprintf(out, "  root:          %s\n", layout.Root)
	_, _ = fmt.Fprintf(out, "  labels.csv:    %s\n", layout.LabelsCSV)
	_, _ = fmt.Fprintf(out, "  images:        %d files\n", len(layout.Images))
	_, _ = fmt.Fprintf(out, "  total size:    %s\n", humanBytesForSummary(layout.TotalBytes))
	_, _ = fmt.Fprintln(out)

	_, _ = fmt.Fprintf(out, "Target cluster:\n")
	_, _ = fmt.Fprintf(out, "  release:       %s (chart %s)\n", release.ReleaseName, release.ChartVersion)
	_, _ = fmt.Fprintf(out, "  jobs-manager:  %s\n", release.JobsManagerService)
	_, _ = fmt.Fprintf(out, "  shared PVC:    %s (%s)\n", pvc.ClaimName, pvc.Phase)
	if !pvc.IsReadWriteMany() {
		// Warn but don't block — RWO clusters still work, the
		// scheduler will co-locate the stage Pod with the existing
		// mounter. Phase 3 PR-b will surface the same warning at
		// pod-create time too.
		_, _ = fmt.Fprintf(out, "  access:        %v (warn: not ReadWriteMany — stage Pod will co-locate)\n", pvc.AccessModes)
	}
	_, _ = fmt.Fprintln(out)

	_, _ = fmt.Fprintf(out, "Synthesized ingest spec:\n")
	_, _ = fmt.Fprintf(out, "  table:         %s\n", spec["table"])
	_, _ = fmt.Fprintf(out, "  category:      %s\n", spec["category"])
	_, _ = fmt.Fprintf(out, "  intent:        %s\n", spec["intent"])
	_, _ = fmt.Fprintf(out, "  label column:  %s\n", spec["label"])
	_, _ = fmt.Fprintf(out, "  destination:   %s\n", push.StagedPrefix(spec["table"].(string)))
	_, _ = fmt.Fprintln(out)

	if !dryRun {
		_, _ = fmt.Fprintf(out, "Next: stage %d files (%s) → %s (coming in PR-b for #151)\n",
			1+len(layout.Images), humanBytesForSummary(layout.TotalBytes),
			push.StagedPrefix(spec["table"].(string)))
		_, _ = fmt.Fprintln(out)
	}
}

// humanBytesForSummary mirrors push.humanBytes but lives here to
// keep internal/push's API surface narrow (the internal helper is
// unexported). Yes, this is a tiny duplication; if a third caller
// shows up, we promote it to a shared util in v0.2.
func humanBytesForSummary(n int64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
