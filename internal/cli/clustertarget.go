package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/client-go/kubernetes"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

// noParentReleaseError marks the exit-4 case where the reached cluster
// genuinely hosts no tracebloc release in the target namespace
// (cluster.ErrNoParentRelease) — as opposed to a present-but-PVC-missing
// release, an API/RBAC list failure, or an ambiguous multiple-release match.
// §7.3 uses it to turn an active-client binding miss into a clear "runs on
// another machine" message; the other failures keep their own diagnostics.
type noParentReleaseError struct{ err error }

func (e *noParentReleaseError) Error() string { return e.err.Error() }
func (e *noParentReleaseError) Unwrap() error { return e.err }

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
func resolveClusterTarget(ctx context.Context, p *ui.Printer, opts cluster.KubeconfigOptions, b activeClientBinding, needPVC bool) (*clusterTarget, error) {
	resolved, err := cluster.Load(opts)
	if err != nil {
		return nil, &exitError{code: 3, err: fmt.Errorf("loading kubeconfig: %w", err)}
	}
	cs, err := cluster.NewClientset(resolved)
	if err != nil {
		return nil, &exitError{code: 3, err: err}
	}
	// The cluster-wide fallback scan only engages when the target namespace is
	// the kubeconfig's default — i.e. nobody chose it: not the user (explicit
	// --namespace/--context) and not the active-client binding. A binding miss
	// must NOT silently redirect to some other client (§7.5 — that could be a
	// different machine's client); it keeps the §7.3 "runs elsewhere" message.
	release, nsUsed, err := discoverRelease(ctx, p, cs, resolved.Namespace, b.allowScan())
	if err != nil {
		// Only a genuine "namespace has no release" maps to the §7.3
		// "runs elsewhere" rewrite; an API/RBAC list failure or an
		// ambiguous multiple-release match keeps its own message.
		if errors.Is(err, cluster.ErrNoParentRelease) {
			return nil, &exitError{code: 4, err: &noParentReleaseError{err}}
		}
		return nil, &exitError{code: 4, err: err}
	}
	// The scan may have retargeted discovery to the namespace that actually
	// hosts the client; everything downstream (PVC discovery, dataset listing,
	// prints) keys on Resolved.Namespace, so it must follow.
	resolved.Namespace = nsUsed
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

// discoverRelease wraps DiscoverParentRelease with the cluster-wide fallback
// scan: when allowScan is set and the target namespace hosts no client, every
// namespace is scanned for one. Exactly one → target it, with a visible note
// (never a silent redirect); several → name them and ask the user to pick;
// none, or a scan failure (e.g. RBAC forbids the cluster-wide list) → the
// original discovery error stands. Returns the namespace actually used.
func discoverRelease(ctx context.Context, p *ui.Printer, cs kubernetes.Interface, namespace string, allowScan bool) (*cluster.ParentRelease, string, error) {
	release, err := cluster.DiscoverParentRelease(ctx, cs, namespace)
	if err == nil || !allowScan || !errors.Is(err, cluster.ErrNoParentRelease) {
		return release, namespace, err
	}
	found, scanErr := cluster.FindClientNamespaces(ctx, cs)
	if scanErr != nil || len(found) == 0 {
		return nil, namespace, err
	}
	if len(found) > 1 {
		return nil, namespace, fmt.Errorf(
			"%w in namespace %q, but tracebloc clients are running in: %s. "+
				"Pass --namespace to pick one, or set your active client with `tracebloc client use`.",
			cluster.ErrNoParentRelease, namespace, strings.Join(found, ", "))
	}
	if p != nil {
		p.Infof("No client in namespace %q — using the one in %q (override with --namespace).", namespace, found[0])
	}
	release, err = cluster.DiscoverParentRelease(ctx, cs, found[0])
	return release, found[0], err
}

// activeClientBinding records that a data command defaulted its target
// namespace to the active client's cached namespace (§7.3), so a subsequent
// "no release here" failure can be explained as "the active client runs
// elsewhere" rather than a bare discovery error.
type activeClientBinding struct {
	applied   bool
	explicit  bool // user pinned --namespace/--context themselves
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
		return activeClientBinding{explicit: true} // user was explicit — don't second-guess
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

// allowScan reports whether the cluster-wide fallback scan may engage: only
// when the target namespace is the kubeconfig's default — i.e. nobody chose
// it. An explicit --namespace/--context is never second-guessed, and a
// binding miss must NOT silently retarget to some other client (§7.5 — it
// could be a different machine's); it keeps the §7.3 "runs elsewhere" message.
func (b activeClientBinding) allowScan() bool { return !b.applied && !b.explicit }

// explain rewrites a "no tracebloc release in namespace" failure (exit 4) into
// §7.3's "client runs on another machine" guidance when the target namespace
// came from the active-client binding: the cluster the kubeconfig reaches
// doesn't host that client. Non-binding errors (and PVC-missing, where the
// release *was* found) pass through unchanged.
func (b activeClientBinding) explain(err error) error {
	if !b.applied {
		return err
	}
	var npr *noParentReleaseError
	if !errors.As(err, &npr) {
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
