package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/push"
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

	// Steps 0–4 + the P3 preflight — everything local — live in
	// resolveLocalInput (cli#283). It mutates a.Spec in place, so the defer
	// above and the cluster steps below see the resolved spec; err feeds the
	// named return, so the defer fires on its failures too.
	layout, spec, specBytes, cancelled, err := resolveLocalInput(out, errOut, &a)
	if err != nil {
		return err
	}
	if cancelled {
		return nil
	}

	// Steps 5–8a — cluster discovery + the destination-table guard — live in
	// connectIngestTarget (cli#283). It may set a.Overwrite (the folded
	// interactive replace decision); the teardown below keys on that.
	target, existingTable, cancelled, err := connectIngestTarget(ctx, &a)
	if err != nil {
		return err
	}
	if cancelled {
		return nil
	}
	resolved, cs, release, pvc := target.Resolved, target.Clientset, target.Release, target.PVC
	tableExists := existingTable != ""

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
