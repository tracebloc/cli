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
	"github.com/tracebloc/cli/internal/submit"
)

// newDatasetCmd wires the `tracebloc dataset` subtree. The dominant
// verb is `push`, completed in Phase 3 (tracebloc/client#151) across
// PR-a (pre-flight: spec synth, validation, layout walk, cluster
// discovery) and PR-b (this one: ephemeral stage Pod + tar-over-
// exec stream + progress bar + SIGINT-safe cleanup). Future verbs
// (`dataset list`, `dataset rm`) hang off this parent in v0.2.
func newDatasetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dataset",
		Short: "Manage datasets in the parent client release",
		Long: `Commands for staging and managing datasets on the cluster's
shared PVC.

Today: ` + "`dataset push`" + ` stages a local directory to the cluster's
shared PVC. Submission to jobs-manager (so the ingestor Job actually
runs) lands in Phase 4 (` + "`tracebloc/client#152`" + `); for now the staged
files are picked up by the existing helm ` + "`tracebloc/ingestor`" + ` flow.

` + "`tracebloc cluster info`" + ` is the pre-flight you'd typically run
before the first push.`,
	}
	cmd.AddCommand(newDatasetPushCmd())
	return cmd
}

// newDatasetPushCmd implements `tracebloc dataset push <local-path>`.
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

		// Ingestor SA name override. Used as the ServiceAccountName
		// of the ephemeral stage Pod, so the Pod inherits whatever
		// imagePullSecrets + PSA exemptions the admin already
		// configured for that SA.
		ingestorSAName string

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
  0   files staged + ingested successfully (or --detach: just staged + submitted)
  2   schema validation failed (synthesized spec rejected) or
      v0.1-unsupported category passed
  3   local-layout or kubeconfig error
  4   cluster reachable but parent release / shared PVC missing
  5   ingestor SA token couldn't be obtained, or jobs-manager
      rejected the token (401/403)
  7   pre-flight succeeded but staging the files failed
      (Pod creation, image pull, exec stream, or remote tar error)
  8   jobs-manager rejected the submit (4xx/5xx other than auth)
  9   ingestion Job exited non-zero, or completed with row-level
      failures the summary panel reports`,
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
					StagePodImage:  stagePodImage,
					Detach:         detach,
					IdempotencyKey: idempotencyKey,
					ImageDigest:    imageDigest,
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
	StagePodImage  string

	// Phase 4 (#152) fields. See the flag declarations for the
	// per-knob rationale; all three are optional.
	Detach         bool
	IdempotencyKey string
	ImageDigest    string
}

// runDatasetPush is the full Phase 3 implementation: pre-flight
// checks, then either --dry-run stop or stage Pod + tar stream +
// cleanup. Phase 4 (#152) will hook submit-to-jobs-manager after
// the staging step.
//
// Step order is "fail fast, fail local" — every step that doesn't
// need the cluster runs before any that does, so a customer with
// a bad label-column or oversized dataset gets the diagnostic in
// milliseconds without a kubeconfig round-trip.
func runDatasetPush(ctx context.Context, out, errOut io.Writer, a runDatasetPushArgs) error {
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

	// 2. v0.1 category gate. Runs BEFORE schema validation because
	//    schema-valid-but-unsupported categories (e.g.
	//    tabular_classification) would otherwise fail with the
	//    schema's "missing property 'schema'" message — confusing
	//    for the customer who has no way to set --schema in v0.1.
	//    Nonsense categories (typos) also hit this gate; the
	//    "only image_classification in v0.1" message is more
	//    actionable than the schema's 11-option enum list anyway.
	//    Bugbot's review-on-self caught the missing gate on PR-a.
	if a.Spec.Category != "" && a.Spec.Category != "image_classification" {
		return &exitError{code: 2, err: fmt.Errorf(
			"category %q is not supported in v0.1 (only image_classification). "+
				"Other categories are one-PR additions in v0.2 — see "+
				"tracebloc/client#147 non-goals.", a.Spec.Category)}
	}

	// 3. Synthesize the spec from flags + validate against schema.
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

	// 4. Walk the local directory. Enforces layout + size caps;
	//    customer sees a clear pointer to expected layout if they
	//    pass the wrong directory.
	layout, err := push.Discover(a.LocalPath)
	if err != nil {
		return &exitError{code: 3, err: err}
	}

	// 5. Cluster discovery — same kubeconfig path as `cluster info`.
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

	// 6. PVC discovery — confirms the chart's shared-data PVC is
	//    Bound before we waste time provisioning a Pod that can't
	//    mount it.
	pvc, err := cluster.DiscoverSharedPVC(ctx, cs, resolved.Namespace)
	if err != nil {
		return &exitError{code: 4, err: err}
	}

	// 7. Print the pre-flight summary. The output is the same in
	//    dry-run and live mode — only the "what happens next" line
	//    differs. Customers iterating on a bad layout see this
	//    every attempt, so it's worth keeping skimmable: one fact
	//    per line, aligned by column.
	printPushPreflight(out, layout, release, pvc, spec, a.DryRun)

	// 8. Dry-run stop. Acknowledged success.
	if a.DryRun {
		_, _ = fmt.Fprintln(out, "Dry-run complete — no cluster resources were created.")
		return nil
	}

	// 9. Stage the files: create ephemeral Pod → wait Ready → tar
	//    stream → cleanup. The deferred cleanup inside push.Stage
	//    runs on success and failure (including ctx cancellation
	//    from a SIGINT handler), so no orphan Pod is left behind.
	//
	//    Exit code 7 ("staging failed") is distinct from the
	//    pre-flight codes so customers can branch on whether the
	//    failure was their environment vs the actual data transfer.
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

	// 10. Mint the SA token Phase 4 uses to authenticate the POST
	//     to jobs-manager. Expiry is 1 hour (vs cluster info's 10
	//     min) because the full Phase 4 lifecycle — submit + watch
	//     + log stream — can run that long for large ingestions.
	//     The chart's helm flow uses the same token-mint code path.
	_, _ = fmt.Fprintln(out)
	tok, err := cluster.MintIngestorToken(ctx, cs, resolved.Namespace,
		release.IngestorSAName, 3600, nil)
	if err != nil {
		return &exitError{code: 5, err: err}
	}

	// 11. Open a port-forward to a Pod backing the jobs-manager
	//     Service. The CLI runs off-cluster (on a laptop, in CI
	//     runners outside the cluster network), so the discovered
	//     *.svc.cluster.local URL isn't reachable — we tunnel
	//     through the kubeconfig-authenticated apiserver, same as
	//     `kubectl port-forward`. Bugbot PR #10 r3 caught the
	//     original broken-by-design direct-URL POST.
	_, _ = fmt.Fprintln(out, "Opening port-forward to jobs-manager...")
	pf, err := submit.PortForwardJobsManager(ctx, cs, resolved.RestConfig,
		resolved.Namespace, release.JobsManagerServiceName, release.JobsManagerPort)
	if err != nil {
		return &exitError{code: 8, err: fmt.Errorf("setting up jobs-manager port-forward: %w", err)}
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
	submitRes, err := submit.Run(ctx, submit.Options{
		Submitter:        submit.NewHTTPSubmitter(localEndpoint, tok.Token),
		Client:           cs,
		IngestConfigYAML: string(specBytes),
		IdempotencyKey:   a.IdempotencyKey,
		ImageDigest:      a.ImageDigest,
		Detach:           a.Detach,
		Out:              out,
	})
	if err != nil {
		switch {
		case submit.IsAuthError(err):
			return &exitError{code: 5, err: err}
		case submit.IsWatchError(err):
			// Watch-phase failure: jobs-manager already accepted
			// the run, the cluster is doing the work, the CLI
			// just couldn't follow along. Exit 9 (ingest-side)
			// not 8 (submit-side). Bugbot flagged the
			// previously-undifferentiated mapping on PR #10.
			return &exitError{code: 9, err: err}
		default:
			return &exitError{code: 8, err: err}
		}
	}

	// Detach paths (--detach flag OR SIGINT-mid-watch) are
	// success — cluster keeps running; the orchestrator already
	// printed the reconnect hint.
	if submitRes.Watch == nil || submitRes.Watch.Outcome == submit.JobOutcomeDetached {
		return nil
	}

	// Watch outcomes. Both Failed and Unknown route to exit 9
	// (Unknown = finalJobStatus timed out without seeing a
	// terminal condition, which we can't claim as success).
	// Bugbot flagged the prior switch's missing Unknown branch
	// on PR #10.
	switch submitRes.Watch.Outcome {
	case submit.JobOutcomeFailed:
		return &exitError{code: 9, err: errors.New("ingestion Job exited non-zero — see logs above")}
	case submit.JobOutcomeUnknown:
		return &exitError{code: 9, err: errors.New(
			"ingestion Job's final status couldn't be determined within the watch window — " +
				"check `kubectl get job -n " + submitRes.Submit.Namespace + " " + submitRes.Submit.JobName + "` for the outcome")}
	case submit.JobOutcomeSucceeded:
		if submitRes.Watch.Summary != nil && submitRes.Watch.Summary.HasFailures() {
			return &exitError{code: 9, err: errors.New(
				"ingestion Job completed but the summary reports failures — see panel above")}
		}
	}
	return nil
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
	_, _ = fmt.Fprintf(out, "  total size:    %s\n", push.HumanBytes(layout.TotalBytes))
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
		_, _ = fmt.Fprintf(out, "Next: stage %d files (%s) → %s\n",
			1+len(layout.Images), push.HumanBytes(layout.TotalBytes),
			push.StagedPrefix(spec["table"].(string)))
		_, _ = fmt.Fprintln(out)
	}
}
