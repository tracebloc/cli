// The cluster-touching half of `data ingest`: the ingestion-run tail
// (mint token -> port-forward -> submit -> classify -> reclaim), the
// destination-table guard, and the test seams over the cluster steps.
// Moved verbatim from data.go (cli#282) — behavior unchanged.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"k8s.io/client-go/kubernetes"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/submit"
	"github.com/tracebloc/cli/internal/ui"
)

// connectIngestTarget is the cluster half of `data ingest`'s pre-flight:
// resolve the kubeconfig and discover the parent release + shared PVC
// (steps 5–7), then run the destination-table guard (8a). Extracted
// verbatim from runDataIngest (cli#283) — step order, output, and exit
// codes unchanged.
//
// The folded replace decision can set a.Overwrite (interactive "replace
// it?" answered yes); the caller's teardown step keys on that, exactly as
// before. cancelled=true with a nil err is the user declining the replace
// prompt: the caller exits 0, nothing ingested. existingTable is the
// EXISTING table's exact spelling ("" when absent) — any teardown must act
// on it, not on the flag's casing.
func connectIngestTarget(ctx context.Context, a *runDataIngestArgs) (target *clusterTarget, existingTable string, cancelled bool, err error) {
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
	target, err = resolveClusterTarget(ctx, a.Printer, opts, binding, true)
	if err != nil {
		return nil, "", false, binding.explain(err)
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
		proceed, aerr := existingTableAction(a, existingTable)
		if aerr != nil {
			return nil, "", false, aerr
		}
		if !proceed {
			a.Printer.Infof("Cancelled — %q was left as-is; nothing was ingested.", existingTable)
			return nil, "", true, nil
		}
		a.Overwrite = true
	}
	if tableExists && a.Overwrite {
		a.Printer.Warnf("Table %q already exists — replacing it (table + files).", existingTable)
	}

	return target, existingTable, false, nil
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
	a.Printer.Step(3, 3, "Validate and load")
	if a.Detach {
		a.Printer.Hintf("Submitting the run — with --detach it keeps running on your workspace after this command returns; the reconnect command is shown below.")
	} else {
		a.Printer.Hintf("Submitting the run, then following along as tracebloc validates your data and loads it into the table — progress streams below.")
		a.Printer.Hintf("This follows the run for up to an hour; a longer run keeps going on its own (or start it with --detach and check back later).")
	}
	tok, err := mintIngestorTokenFn(ctx, cs, resolved.Namespace,
		release.IngestorSAName, 3600, nil)
	if err != nil {
		return false, &exitError{code: exitAuth, err: err}
	}

	// 11. Open a port-forward to a Pod backing the jobs-manager
	//     Service. The CLI runs off-cluster (on a laptop, in CI
	//     runners outside the cluster network), so the discovered
	//     *.svc.cluster.local URL isn't reachable — we tunnel
	//     through the kubeconfig-authenticated apiserver, same as
	//     `kubectl port-forward`. Bugbot PR #10 r3 caught the
	//     original broken-by-design direct-URL POST.
	// Opening the port-forward is a blocking wait (tunnel setup through the
	// apiserver), so it runs under a spinner — no wait on the happy path stays
	// silent (RFC-0002 "progress on every wait"). The submit POST itself is a
	// separate ~30s synchronous wait; its spinner lives in submit.Run, next to
	// the POST it covers.
	connectSpin := a.Printer.Spinner("Connecting to your workspace to submit the run", "")
	pf, err := portForwardJobsManagerFn(ctx, cs, resolved.RestConfig,
		resolved.Namespace, release.JobsManagerServiceName, release.JobsManagerPort)
	connectSpin.Stop()
	if err != nil {
		return false, &exitError{code: exitSubmitFailed, err: fmt.Errorf("setting up jobs-manager port-forward: %w", err)}
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
		reclaimSpin := a.Printer.Spinner("Reclaiming the temporary copy", "")
		cerr := cleanStagingFn(ctx, cs,
			&push.SPDYExecutor{Config: resolved.RestConfig, Client: cs},
			resolved.Namespace, a.Spec.Table, push.PodSpecOptions{
				Namespace:          resolved.Namespace,
				PVCClaimName:       pvc.ClaimName,
				PVCMountPath:       pvc.MountPath,
				Table:              a.Spec.Table,
				ServiceAccountName: release.IngestorSAName,
				Image:              a.StagePodImage,
			})
		reclaimSpin.Stop()
		if cerr != nil {
			a.Printer.Warnf("Couldn't reclaim the temporary copy (%v). It's harmless — the next re-ingest of %q or a `tracebloc data delete %s` will clear it.",
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

// printClusterSummary shows the discovered workspace target. It's Kubernetes
// plumbing (release / jobs-manager / shared PVC) the happy path hides, so the
// whole block — header, fields, and the RWO-PVC note — prints only under
// --verbose (RFC-0002 §6). Discovery + guards are unchanged; this is
// presentation only.
func printClusterSummary(p *ui.Printer, release *cluster.ParentRelease, pvc *cluster.SharedPVC) {
	if !p.Verbose() {
		return
	}
	p.Section("Target cluster")
	p.Detailf("release: %s (chart %s)", release.ReleaseName, release.ChartVersion)
	p.Detailf("jobs-manager: %s", release.JobsManagerService)
	p.Detailf("shared PVC: %s (%s)", pvc.ClaimName, pvc.Phase)
	if !pvc.IsReadWriteMany() {
		// Note but don't block — RWO clusters still work; the scheduler
		// co-locates the stage Pod with the existing mounter.
		p.Detailf("PVC is %v, not ReadWriteMany — the stage Pod will co-locate with the existing mounter", pvc.AccessModes)
	}
}

// listDatasetsFn is a test seam over push.ListDatasets.
var listDatasetsFn = push.ListDatasets

// teardownFn is a test seam over push.Teardown (the destructive DROP TABLE +
// file removal). Production points at the real Teardown; a test overrides it to
// drive the clean and the partial-failure (table dropped, files remain → exit
// 7) paths without a cluster.
var teardownFn = push.Teardown

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

// existingTableAction resolves what to do when the destination table
// already exists and --overwrite was NOT passed on the command line.
//
//   - proceed=true, err=nil     → replace it: the caller sets Overwrite and
//     runs the same teardown `data delete` does.
//   - proceed=false, err=nil    → the user declined the replace prompt; a
//     clean cancel (exit 0), nothing ingested.
//   - err != nil (exit 6)       → non-interactive and no --overwrite: refuse,
//     same hard contract scripts have always had.
//
// Interactive mode prompts to replace UNLESS a --idempotency-key was
// reused: a reused key + a replace is the data-loss trap the top-of-func
// guard forbids (the teardown removes the data, then the cluster replays
// the old run and ingests nothing), so that combination falls through to
// the exit-6 refusal rather than being offered as a prompt.
func existingTableAction(a *runDataIngestArgs, existingTable string) (proceed bool, err error) {
	if a.Interactive && a.Prompter != nil && a.IdempotencyKey == "" {
		ok, perr := a.Prompter.Confirm(fmt.Sprintf(
			"A dataset named %q already exists — replace it?", existingTable), false)
		if perr != nil {
			if errors.Is(perr, errInteractiveCancelled) {
				return false, nil
			}
			return false, &exitError{code: exitLocalEnv, err: fmt.Errorf("overwrite prompt: %w", perr)}
		}
		return ok, nil
	}
	return false, &exitError{code: exitTableExists, err: fmt.Errorf(
		"table %q already exists in this workspace. Re-ingesting the same table doesn't merge or replace — "+
			"the run would fail after uploading everything. Re-run with --overwrite to replace it, "+
			"or pick a different --name. (`tracebloc data delete %s` also removes it.)",
		existingTable, existingTable)}
}
