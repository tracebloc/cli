package cli

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/client-go/kubernetes"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
)

// clusterTarget bundles the cluster handles the data commands resolve from a
// kubeconfig before doing any work: the resolved config, a clientset, the
// parent tracebloc release, and — when asked — the shared data PVC.
type clusterTarget struct {
	Resolved  *cluster.ResolvedConfig
	Clientset kubernetes.Interface
	Release   *cluster.ParentRelease
	PVC       *cluster.SharedPVC // nil unless needPVC was requested
}

// resolveClusterTarget centralizes the identical Load → NewClientset →
// DiscoverParentRelease (→ DiscoverSharedPVC) sequence that `data ingest`,
// `data list`, and `data delete` each repeated, together with its exit-code
// contract: exit 3 for kubeconfig / clientset failures (can't reach a cluster
// at all), exit 4 for a missing tracebloc release or shared PVC (reached a
// cluster, but it isn't a tracebloc workspace).
//
// `cluster doctor` is deliberately NOT a caller — it has a different exit
// contract (2/3 escalation, with discovery reported as a check Result rather
// than a hard error).
func resolveClusterTarget(ctx context.Context, opts cluster.KubeconfigOptions, needPVC bool) (*clusterTarget, error) {
	resolved, err := cluster.Load(opts)
	if err != nil {
		return nil, &exitError{code: 3, err: fmt.Errorf("loading kubeconfig: %w", err)}
	}
	cs, err := cluster.NewClientset(resolved)
	if err != nil {
		return nil, &exitError{code: 3, err: err}
	}
	release, err := cluster.DiscoverParentRelease(ctx, cs, resolved.Namespace)
	if err != nil {
		// The error carries cluster.ErrNoParentRelease for a genuine
		// "namespace has no release" (which explain() rewrites as §7.3
		// "runs elsewhere"); an API/RBAC list failure or ambiguous
		// multiple-release match wraps a different error and passes through.
		return nil, &exitError{code: 4, err: err}
	}
	t := &clusterTarget{Resolved: resolved, Clientset: cs, Release: release}
	if needPVC {
		pvc, err := cluster.DiscoverSharedPVC(ctx, cs, resolved.Namespace)
		if err != nil {
			return nil, &exitError{code: 4, err: err}
		}
		t.PVC = pvc
	}
	return t, nil
}

// activeClientBinding records that a data command defaulted its target
// namespace to the active client's cached namespace (§7.3), so a subsequent
// "no release here" failure can be explained as "the active client runs
// elsewhere" rather than a bare discovery error.
type activeClientBinding struct {
	applied   bool
	name      string
	namespace string
}

// bindActiveClientNamespace defaults opts.Namespace to the active client's
// cached namespace when the user overrode neither --namespace nor --context.
// It never fails: no config, no active client, or no cached namespace all
// leave opts untouched (unchanged current-context behavior), so this is
// backward compatible for anyone who hasn't run `client use`/`create`.
func bindActiveClientNamespace(opts *cluster.KubeconfigOptions) activeClientBinding {
	if opts.Namespace != "" || opts.Context != "" {
		return activeClientBinding{} // user was explicit — don't second-guess
	}
	cfg, err := config.Load()
	if err != nil {
		return activeClientBinding{}
	}
	p := cfg.Current()
	if p.ActiveClientNamespace == "" {
		return activeClientBinding{}
	}
	opts.Namespace = p.ActiveClientNamespace
	return activeClientBinding{applied: true, name: p.ActiveClientName, namespace: p.ActiveClientNamespace}
}

// explain rewrites a "no tracebloc release in namespace" failure (exit 4) into
// §7.3's "client runs on another machine" guidance when the target namespace
// came from the active-client binding: the cluster the kubeconfig reaches
// doesn't host that client. Gated on cluster.ErrNoParentRelease so a
// PVC-missing failure (release was found), an API/RBAC error, or an ambiguous
// multiple-release match all pass through unchanged. Callable by any command
// that binds — data ingest/list/delete and cluster info.
func (b activeClientBinding) explain(err error) error {
	if !b.applied || !errors.Is(err, cluster.ErrNoParentRelease) {
		return err
	}
	handle := b.name
	if handle == "" {
		handle = b.namespace
	}
	return &exitError{code: 4, err: fmt.Errorf(
		"active client %q runs on another machine — namespace %q isn't on the cluster your kubeconfig points at; "+
			"run this command there, `tracebloc client use` a client on this cluster, or override with --namespace/--context",
		handle, b.namespace)}
}
