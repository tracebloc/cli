package cli

import (
	"context"
	"fmt"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

// clusterIDFromFn is a test seam over cluster.ClusterIDFrom.
var clusterIDFromFn = cluster.ClusterIDFrom

// guardActiveClientCluster refuses a MUTATING operation when the cluster actually
// reached is not the one the active client lives on.
//
// WHY THIS EXISTS (backend#2863). Every command resolved its target cluster from the
// ambient kubeconfig + current-context, while binding only the NAMESPACE from the
// active client. So on a machine whose current-context pointed elsewhere — a laptop
// that also administers a managed cluster, which is the normal case for anyone who
// runs both — a mutating command acted on that other cluster:
//
//   - `data ingest` staged a private dataset onto it
//   - `data delete` dropped a table and removed files from its shared PVC
//   - `resources set` rolled its jobs-manager with a new envelope
//   - `tracebloc delete` uninstalled a release of the same name
//
// The namespace binding made this MORE likely, not less: it supplied a namespace that
// probably exists on the other cluster too, so discovery succeeded and nothing looked
// wrong. The hazard was already documented at internal/nodeboot/nodeboot.go, whose
// comment ends "preserving the default-context behavior" — that clause was the bug.
//
// The check is on IDENTITY, not on the context name. Pinning a context string would
// break bring-your-own-cluster installs (EKS/AKS/OpenShift have no k3d context) and
// would still pass if two kubeconfigs named the same context differently. The
// kube-system namespace UID is the same anchor the backend record uses, so local and
// remote agree by construction.
//
// FAILURE MODES, deliberately asymmetric:
//
//   - mismatch            -> REFUSE. Naming both ids and the way forward.
//   - id unreadable       -> REFUSE. We are about to write to a cluster we cannot
//     identify. Every caller needs API access anyway, so this
//     costs nothing legitimate.
//   - no anchor recorded  -> WARN and proceed. Configs written before this field
//     exists must not be locked out of their own commands;
//     `client create` records it and the warning names that.
//
// Read-only commands never call this: being wrong about which cluster you are
// READING is a confusing answer, not a destructive act, and the target is already
// printed by doctor / cluster info.
func guardActiveClientCluster(ctx context.Context, p *ui.Printer, t *clusterTarget) error {
	if t == nil || t.Clientset == nil {
		return nil // nothing resolved (a test seam, or a command that mutates nothing)
	}
	cfg, err := config.Load()
	if err != nil {
		// No readable config means no anchor to compare against — same case as an
		// unrecorded anchor below, not a reason to block.
		p.Warnf("Couldn't read the local config, so this machine's cluster couldn't be " +
			"verified before changing anything. Proceeding.")
		return nil
	}
	want := cfg.Current().ActiveClientClusterID
	if want == "" {
		p.Warnf("This machine hasn't recorded which cluster its secure environment runs on, "+
			"so the target couldn't be verified before changing anything. Run `tracebloc client "+
			"create` to record it. Proceeding against %s.", t.Resolved.ServerURL)
		return nil
	}
	got, idErr := clusterIDFromFn(ctx, t.Clientset)
	if idErr != nil {
		return &exitError{code: exitLocalEnv, err: fmt.Errorf(
			"couldn't confirm which cluster this is before changing anything (%w).\n"+
				"  Refusing rather than writing to an unidentified cluster.\n"+
				"  reached: %s", idErr, t.Resolved.ServerURL)}
	}
	if got != want {
		return &exitError{code: exitLocalEnv, err: fmt.Errorf(
			"this is not the cluster your secure environment runs on — refusing to change anything.\n"+
				"  reached:  %s  (cluster %s, context %q)\n"+
				"  expected: cluster %s\n"+
				"  Your kubeconfig's current context points somewhere else. Either switch it, or pass\n"+
				"  --context/--kubeconfig for the cluster your secure environment runs on.",
			t.Resolved.ServerURL, short(got), t.Resolved.Context, short(want))}
	}
	return nil
}

// short trims a UID for messages — enough to compare by eye, not a wall of hex.
func short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}
