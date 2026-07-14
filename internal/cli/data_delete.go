package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// runDataDeleteArgs is the resolved input to runDataDelete — same shape
// convention as runDataIngestArgs, so the command's RunE stays a thin
// flag-to-struct adapter and the logic is unit-testable.
type runDataDeleteArgs struct {
	Table      string
	Kubeconfig string
	Context    string
	Namespace  string
	DryRun     bool
	Yes        bool
	Printer    *ui.Printer
	Prompter   prompter // nil off a TTY or when --yes is set
	// OutputJSON routes human output to stderr and emits exactly one JSON
	// result object to JSONOut (stdout); set together by the RunE in
	// --output-json mode. Same contract as data list / data ingest.
	OutputJSON bool
	JSONOut    io.Writer
}

// newDataDeleteCmd implements `tracebloc data delete <table>` — the
// in-cluster teardown of a previously-ingested dataset. See
// internal/push.Teardown for the mechanism and the design note on the
// approach (CLI-direct vs a server-side delete endpoint).
//
// Aliases: "rm" is kept for one deprecation cycle so existing
// scripts continue to work.
func newDataDeleteCmd() *cobra.Command {
	var (
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
		dryRun          bool
		yes             bool
		outputJSON      bool
	)

	cmd := &cobra.Command{
		Use:     "delete <table>",
		Aliases: []string{"rm"},
		Short:   "Delete an ingested dataset's in-cluster artifacts (table + PVC files)",
		Long: `Removes the in-cluster artifacts a previous ` + "`data ingest`" + ` created
for a table: the MySQL table in ` + push.IngestionDatabase + ` and the dataset's
directories on the shared PVC. Destructive and not undoable.

The dataset's catalog metadata on the tracebloc backend is never removed — it
is kept as a record on tracebloc, marked unavailable, so a collaborator's run
that referenced it still has its history.

Exit codes:
  0  artifacts removed (or --dry-run, or the user declined)
  2  invalid table name
  3  kubeconfig error, or refused (no confirmation off a terminal)
  4  cluster reachable but no tracebloc client / shared storage missing,
     or the client's dataset list couldn't be read (can't confirm the target)
  5  no dataset by that name on this client (nothing to delete)
  7  teardown failed mid-flight (table drop or PVC rm errored)

With --output-json, stdout carries exactly one JSON result object per run
(human output goes to stderr) and the exit codes above are unchanged; see
docs/json-output.md for the shape and the stability promise.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Confirm interactively on a TTY unless --yes was passed.
			// --output-json never prompts (same as data ingest's
			// implies---no-input): a scripted delete must say --yes
			// (or --dry-run) explicitly.
			var pr prompter
			if !yes && !outputJSON && isInteractiveTTY() {
				pr = surveyPrompter{}
			}
			// In --output-json mode, human output goes to stderr so
			// stdout carries only the JSON — same split as data list.
			printer := printerFor(cmd)
			var jsonOut io.Writer
			if outputJSON {
				printer = printerForWriter(cmd, cmd.ErrOrStderr())
				jsonOut = cmd.OutOrStdout()
			}
			return runDataDelete(cmd.Context(), runDataDeleteArgs{
				Table:      args[0],
				Kubeconfig: kubeconfigPath,
				Context:    contextOverride,
				Namespace:  nsOverride,
				DryRun:     dryRun,
				Yes:        yes,
				Printer:    printer,
				Prompter:   pr,
				OutputJSON: outputJSON,
				JSONOut:    jsonOut,
			})
		},
	}

	addKubeconfigFlags(cmd, &kubeconfigPath, &contextOverride, kubeconfigFlagUsage, contextFlagUsage)
	addNamespaceFlag(cmd, &nsOverride, namespaceFlagUsage)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"show what would be deleted without deleting anything")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false,
		"skip the confirmation prompt (required when not on a terminal)")
	cmd.Flags().BoolVar(&outputJSON, "output-json", false,
		"emit the delete result as JSON on stdout (human output → stderr; never prompts — pass --yes to delete, or --dry-run)")

	return cmd
}

// runDataDelete discovers the cluster, shows the teardown plan, confirms,
// then removes the in-cluster artifacts. The flow mirrors runDataIngest
// (validate → discover → plan/pre-flight → act) so the two commands feel
// like siblings.
func runDataDelete(ctx context.Context, a runDataDeleteArgs) (err error) {
	// In --output-json mode, guarantee stdout always carries JSON: the
	// terminal paths (deleted / dry-run / declined) emit a result and set
	// jsonEmitted; this defer covers every failure return (bad name,
	// kubeconfig, no release, refused, teardown) with a JSON error
	// object, mirroring data list. (Bugbot #53)
	jsonEmitted := false
	defer func() {
		if a.OutputJSON && err != nil && !jsonEmitted {
			code := 1
			var ee *exitError
			if errors.As(err, &ee) {
				code = ee.Code()
			}
			writeDataDeleteErrorJSON(a.JSONOut, err, code)
		}
	}()

	p := a.Printer
	p.Banner("tracebloc", "delete an ingested dataset")
	p.Para(`This permanently removes a dataset you ingested earlier: it drops the table from
the cluster and deletes the dataset's files on the shared storage. It can't be
undone — re-ingesting the data is the only way back.`)

	// 0. Empty-arg guard with a DELETE-appropriate message (#76a). delete
	//    takes the dataset as a POSITIONAL arg, so an empty one must not fall
	//    through to ValidateTableName's "set --name" text (that flag belongs to
	//    `data ingest`, not here). ExactArgs(1) still accepts an explicit "".
	if a.Table == "" {
		return &exitError{code: 2, err: errors.New(
			"dataset name is required — pass it as an argument: tracebloc data delete <dataset>")}
	}

	// 1. Validate the name before we build any PVC path from it
	//    (push.PlanTeardown panics on an unsafe name by design).
	if err := push.ValidateTableName(a.Table); err != nil {
		return &exitError{code: 2, err: fmt.Errorf("invalid table name %q: %w", a.Table, err)}
	}

	// 2. Resolve cluster + clientset (kubeconfig errors = exit 3), then
	//    confirm the parent release + shared PVC exist (exit 4 if not) —
	//    both "is this the right cluster?" context and a guard against
	//    running teardown against a cluster with no tracebloc install.
	opts := cluster.KubeconfigOptions{Path: a.Kubeconfig, Context: a.Context, Namespace: a.Namespace}
	binding := bindActiveClientNamespace(&opts)
	target, err := resolveClusterTargetFn(ctx, a.Printer, opts, binding, true)
	if err != nil {
		return binding.explain(err)
	}
	resolved, cs, release, pvc := target.Resolved, target.Clientset, target.Release, target.PVC

	// 3. Resolve the requested name against the datasets actually on the
	//    client — case-INSENSITIVELY, the same EqualFold match `data ingest`'s
	//    destination guard uses (destTableExists / listDatasetsFn). The
	//    teardown itself (DROP TABLE / rm) is case-SENSITIVE on the cluster, so
	//    `data delete Churn` for a table named `churn` used to drop nothing and
	//    still exit 0 — a silent no-op that read as a successful delete
	//    (backend#1027). We tear down the REAL spelling from here on, and fail
	//    CLOSED (never exit 0) if the name isn't there or the list can't be
	//    read: a destructive, unrecoverable delete must confirm its target.
	matched, err := resolveDeleteTarget(ctx, cs, resolved, a.Table)
	if err != nil {
		return err
	}

	// 4. Show exactly what will be deleted — the customer's last look
	//    before destructive, unrecoverable work.
	plan := push.PlanTeardown(matched)

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
		if a.OutputJSON {
			writeDataDeleteJSON(a.JSONOut, "dry-run", resolved.Namespace, release.ReleaseName, plan, nil)
			jsonEmitted = true
		}
		return nil
	}

	// 6. Confirm. --yes skips; off a TTY without --yes we refuse rather
	//    than delete unprompted. (In --output-json mode the RunE never
	//    wires a Prompter, so a JSON run without --yes lands on the
	//    refusal above via exit 3 — but if a caller passes one anyway,
	//    a decline still keeps the stdout-always-JSON contract.)
	if !a.Yes {
		if a.Prompter == nil {
			return &exitError{code: 3, err: errors.New(
				"refusing to delete without confirmation: pass --yes or run on a terminal")}
		}
		p.PromptHint("This drops the table and removes the files listed above — there's no undo. Pass --yes next time to skip this prompt.")
		ok, err := a.Prompter.Confirm(fmt.Sprintf("Delete %q and its files?", matched), false)
		if err != nil {
			if errors.Is(err, errInteractiveCancelled) {
				p.Infof("Cancelled — nothing was deleted.")
				if a.OutputJSON {
					writeDataDeleteJSON(a.JSONOut, "declined", resolved.Namespace, release.ReleaseName, plan, nil)
					jsonEmitted = true
				}
				return nil
			}
			return &exitError{code: 3, err: err}
		}
		if !ok {
			p.Infof("Cancelled — nothing was deleted.")
			if a.OutputJSON {
				writeDataDeleteJSON(a.JSONOut, "declined", resolved.Namespace, release.ReleaseName, plan, nil)
				jsonEmitted = true
			}
			return nil
		}
	}

	// 7. Execute the in-cluster teardown. File removal runs in a
	//    short-lived pod that shares the stage pod's identity (uid
	//    65532 + fsGroup 65532), so it owns and can delete the staging
	//    files on any volume type — including hostPath, where fsGroup is
	//    a no-op (tracebloc/client#259).
	p.Infof("Removing in-cluster artifacts…")
	res, err := teardownFn(ctx, cs, &push.SPDYExecutor{Config: resolved.RestConfig, Client: cs}, resolved.Namespace, plan, push.PodSpecOptions{
		Namespace:          resolved.Namespace,
		PVCClaimName:       pvc.ClaimName,
		PVCMountPath:       pvc.MountPath,
		Table:              matched,
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
					"re-run `tracebloc data delete %s`, or delete the leftover staging dirs on the node",
				plan.Database, plan.Table, err, matched)}
		}
		return &exitError{code: 7, err: fmt.Errorf("teardown failed: %w", err)}
	}

	p.Newline()
	p.Successf("Deleted %s.%s and %d PVC path(s).", plan.Database, plan.Table, len(res.RemovedPaths))
	p.Infof("The dataset's catalog metadata is kept as a record on tracebloc, marked unavailable — never removed.")
	if a.OutputJSON {
		writeDataDeleteJSON(a.JSONOut, "deleted", resolved.Namespace, release.ReleaseName, plan, res.RemovedPaths)
		jsonEmitted = true
	}
	return nil
}

// dataDeleteJSON is the --output-json shape (owned by the CLI layer, the
// same convention as dataListJSON / pushJSONResult — see
// docs/json-output.md for the cross-command contract).
type dataDeleteJSON struct {
	Status       string   `json:"status"` // deleted | dry-run | declined
	Namespace    string   `json:"namespace"`
	Release      string   `json:"release"`
	Database     string   `json:"database"`
	Table        string   `json:"table"` // the REAL (case-resolved) spelling, not the raw argument
	PVCPaths     []string `json:"pvc_paths"`
	RemovedPaths []string `json:"removed_paths"`
}

// writeDataDeleteJSON serializes the delete result to w (stdout in
// --output-json mode). Marshal errors are dropped: marshaling our own
// struct can't fail in practice, and the exit code remains the contract.
func writeDataDeleteJSON(w io.Writer, status, namespace, release string, plan push.TeardownPlan, removed []string) {
	pvcPaths := plan.PVCPaths
	if pvcPaths == nil {
		pvcPaths = []string{} // emit [] not null
	}
	if removed == nil {
		removed = []string{} // emit [] not null
	}
	res := dataDeleteJSON{
		Status:       status,
		Namespace:    namespace,
		Release:      release,
		Database:     plan.Database,
		Table:        plan.Table,
		PVCPaths:     pvcPaths,
		RemovedPaths: removed,
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, string(b))
}

// writeDataDeleteErrorJSON emits a minimal JSON error object for
// --output-json runs that fail before a result is produced, so stdout
// is never empty on failure. The shape mirrors writeDataListErrorJSON
// EXACTLY ({status:"error", error, exit_code}) — the cross-command
// error contract documented in docs/json-output.md.
func writeDataDeleteErrorJSON(w io.Writer, e error, code int) {
	res := struct {
		Status   string `json:"status"`
		Error    string `json:"error"`
		ExitCode int    `json:"exit_code"`
	}{Status: "error", Error: e.Error(), ExitCode: code}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, string(b))
}

// resolveDeleteTarget maps the user-supplied dataset name onto the REAL
// spelling of a dataset that actually exists on the client, matching
// case-INSENSITIVELY exactly as `data ingest`'s destination guard does
// (see destTableExists — same listDatasetsFn seam, same strings.EqualFold).
// It exists because the teardown is case-SENSITIVE on the cluster: DROP TABLE
// / rm against a mis-cased name removes nothing yet still succeeds, so
// `data delete Churn` for a table named `churn` used to be a silent no-op that
// exited 0 (backend#1027). The caller tears down the returned name, never the
// raw flag.
//
// Unlike the ingest guard — which fails OPEN, because the in-cluster duplicate
// check still backstops it — a destructive, unrecoverable delete fails CLOSED:
//   - listing the datasets errored  → exit 4 (cluster reachable but we can't
//     confirm what's there; refuse rather than delete blind).
//   - no dataset matches (any case) → exit 5 (nothing to delete), naming what
//     IS on the client so the caller can fix the spelling.
func resolveDeleteTarget(ctx context.Context, cs kubernetes.Interface, resolved *cluster.ResolvedConfig, requested string) (string, error) {
	names, err := listDatasetsFn(ctx, cs, resolved.RestConfig, resolved.Namespace)
	if err != nil {
		return "", &exitError{code: 4, err: fmt.Errorf(
			"can't confirm %q exists on this client — refusing to delete without "+
				"confirming the target first: %w", requested, err)}
	}
	for _, n := range names {
		if strings.EqualFold(n, requested) {
			return n, nil
		}
	}
	return "", &exitError{code: 5, err: fmt.Errorf(
		"no dataset named %q on this client%s", requested, availableHint(names))}
}

// availableHint renders the "here's what IS on the client" tail of a
// not-found delete error, so a mis-typed name (case now resolves on its own)
// is easy to correct.
func availableHint(names []string) string {
	if len(names) == 0 {
		return " — this client has no ingested datasets"
	}
	return fmt.Sprintf(" (datasets on this client: %s)", strings.Join(names, ", "))
}
