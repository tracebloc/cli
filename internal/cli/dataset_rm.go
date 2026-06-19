package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// runDatasetRmArgs is the resolved input to runDatasetRm — same shape
// convention as runDatasetPushArgs, so the command's RunE stays a thin
// flag-to-struct adapter and the logic is unit-testable.
type runDatasetRmArgs struct {
	Table      string
	Kubeconfig string
	Context    string
	Namespace  string
	DryRun     bool
	Yes        bool
	Printer    *ui.Printer
	Prompter   prompter // nil off a TTY or when --yes is set
}

// newDatasetRmCmd implements `tracebloc dataset rm <table>` — the
// in-cluster teardown of a previously-pushed dataset. See
// internal/push.Teardown for the mechanism and the design note on the
// approach (CLI-direct vs a server-side delete endpoint).
func newDatasetRmCmd() *cobra.Command {
	var (
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
		dryRun          bool
		yes             bool
	)

	cmd := &cobra.Command{
		Use:   "rm <table>",
		Short: "Delete a pushed dataset's in-cluster artifacts (table + PVC files)",
		Long: `Removes the in-cluster artifacts a previous ` + "`dataset push`" + ` created
for a table: the MySQL table in ` + push.IngestionDatabase + ` and the dataset's
directories on the shared PVC. Destructive and not undoable.

The dataset's catalog metadata on the tracebloc backend is removed
automatically after deletion — no manual step required.

Exit codes:
  0  artifacts removed (or --dry-run, or the user declined)
  2  invalid table name
  3  kubeconfig error, or refused (no confirmation off a terminal)
  4  cluster reachable but parent release / shared PVC missing
  7  teardown failed mid-flight (table drop or PVC rm errored)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Confirm interactively on a TTY unless --yes was passed.
			var pr prompter
			if !yes && isInteractiveTTY() {
				pr = surveyPrompter{}
			}
			return runDatasetRm(cmd.Context(), runDatasetRmArgs{
				Table:      args[0],
				Kubeconfig: kubeconfigPath,
				Context:    contextOverride,
				Namespace:  nsOverride,
				DryRun:     dryRun,
				Yes:        yes,
				Printer:    printerFor(cmd),
				Prompter:   pr,
			})
		},
	}

	cmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	cmd.Flags().StringVar(&contextOverride, "context", "",
		"name of the kubeconfig context to use (default: kubeconfig's current-context)")
	cmd.Flags().StringVarP(&nsOverride, "namespace", "n", "",
		"namespace where the parent tracebloc/client release is installed")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"show what would be deleted without deleting anything")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false,
		"skip the confirmation prompt (required when not on a terminal)")

	return cmd
}

// runDatasetRm discovers the cluster, shows the teardown plan, confirms,
// then removes the in-cluster artifacts. The flow mirrors runDatasetPush
// (validate → discover → plan/pre-flight → act) so the two commands feel
// like siblings.
func runDatasetRm(ctx context.Context, a runDatasetRmArgs) error {
	p := a.Printer
	p.Banner("tracebloc", "delete a pushed dataset")
	p.Para(`This permanently removes a dataset you pushed earlier: it drops the table from
the cluster and deletes the dataset's files on the shared storage. It can't be
undone — re-pushing the data is the only way back.`)

	// 1. Validate the name before we build any PVC path from it
	//    (push.PlanTeardown panics on an unsafe name by design).
	if err := push.ValidateTableName(a.Table); err != nil {
		return &exitError{code: 2, err: fmt.Errorf("invalid table name %q: %w", a.Table, err)}
	}

	// 2. Resolve cluster + clientset (kubeconfig errors = exit 3).
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

	// 3. Confirm the parent release + shared PVC exist (exit 4 if not) —
	//    both "is this the right cluster?" context and a guard against
	//    running teardown against a cluster with no tracebloc install.
	release, err := cluster.DiscoverParentRelease(ctx, cs, resolved.Namespace)
	if err != nil {
		return &exitError{code: 4, err: err}
	}
	pvc, err := cluster.DiscoverSharedPVC(ctx, cs, resolved.Namespace)
	if err != nil {
		return &exitError{code: 4, err: err}
	}

	// 4. Show exactly what will be deleted — the customer's last look
	//    before destructive, unrecoverable work.
	plan := push.PlanTeardown(a.Table)

	p.Section("Target")
	p.Field("context", resolved.Context)
	p.Field("namespace", resolved.Namespace)
	p.Field("release", release.ReleaseName)
	p.Field("shared PVC", pvc.ClaimName)

	p.Section("Will delete")
	p.Field("mysql table", plan.Database+"."+plan.Table)
	for _, path := range plan.PVCPaths {
		p.Field("pvc path", path)
	}
	p.Warnf("Destructive and cannot be undone.")

	// 5. Dry-run stop.
	if a.DryRun {
		p.Newline()
		p.Successf("Dry-run — nothing was deleted.")
		return nil
	}

	// 6. Confirm. --yes skips; off a TTY without --yes we refuse rather
	//    than delete unprompted.
	if !a.Yes {
		if a.Prompter == nil {
			return &exitError{code: 3, err: errors.New(
				"refusing to delete without confirmation: pass --yes or run on a terminal")}
		}
		p.PromptHint("This drops the table and removes the files listed above — there's no undo. Pass --yes next time to skip this prompt.")
		ok, err := a.Prompter.Confirm(fmt.Sprintf("Delete %q and its files?", a.Table), false)
		if err != nil {
			if errors.Is(err, errInteractiveCancelled) {
				p.Infof("Cancelled — nothing was deleted.")
				return nil
			}
			return &exitError{code: 3, err: err}
		}
		if !ok {
			p.Infof("Cancelled — nothing was deleted.")
			return nil
		}
	}

	// 7. Execute the in-cluster teardown. File removal runs in a
	//    short-lived pod that shares the stage pod's identity (uid
	//    65532 + fsGroup 65532), so it owns and can delete the staging
	//    files on any volume type — including hostPath, where fsGroup is
	//    a no-op (tracebloc/client#259).
	p.Infof("Removing in-cluster artifacts…")
	res, err := push.Teardown(ctx, cs, &push.SPDYExecutor{Config: resolved.RestConfig, Client: cs}, resolved.Namespace, plan, push.PodSpecOptions{
		Namespace:          resolved.Namespace,
		PVCClaimName:       pvc.ClaimName,
		PVCMountPath:       pvc.MountPath,
		Table:              a.Table,
		ServiceAccountName: release.IngestorSAName,
		// Image left empty → push.DefaultStagePodImage (alpine; has rm).
	})
	if err != nil {
		// Two sequential destructive ops. If the table dropped but file
		// removal didn't, the drop is idempotent (DROP TABLE IF EXISTS),
		// so re-running is safe; if it keeps failing, remove the leftover
		// staging dirs on the node directly.
		if res.DroppedTable {
			return &exitError{code: 7, err: fmt.Errorf(
				"teardown incomplete — the table %s.%s was dropped, but removing its files failed: %w; "+
					"re-run `tracebloc dataset rm %s`, or delete the leftover staging dirs on the node",
				plan.Database, plan.Table, err, a.Table)}
		}
		return &exitError{code: 7, err: fmt.Errorf("teardown failed: %w", err)}
	}

	p.Newline()
	p.Successf("Deleted %s.%s and %d PVC path(s).", plan.Database, plan.Table, len(res.RemovedPaths))
	p.Infof("The dataset's catalog metadata will be removed automatically — no further action needed.")
	return nil
}
