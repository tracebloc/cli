package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/installer"
	"github.com/tracebloc/cli/internal/ui"
)

// noParentReleaseError marks the exit-4 case where the reached cluster
// genuinely hosts no tracebloc release in the target namespace
// (cluster.ErrNoParentRelease) — as opposed to a present-but-PVC-missing
// release, an API/RBAC list failure, or an ambiguous multiple-release match.
// §7.3 uses it to turn an active-client binding miss into a clear "runs on
// another machine" message; the other failures keep their own diagnostics.
//
// probe carries the read-only handles explain needs to say what IS on the
// reached cluster before advising (#515). It is attached wherever the error is
// built — both sites already hold a clientset and the resolved server URL — and
// travels ON the error rather than through the call signature so a caller can
// never hand explain a clientset for a DIFFERENT cluster than the one that
// missed. A nil probe (a synthesised error, a resolveClusterTargetFn test
// double) means "we could not look", and explain then claims nothing.
type noParentReleaseError struct {
	err   error
	probe *clusterProbe
}

func (e *noParentReleaseError) Error() string { return e.err.Error() }
func (e *noParentReleaseError) Unwrap() error { return e.err }

// clusterProbe is the pair explain needs to diagnose before advising: a
// clientset for the cluster the kubeconfig actually reached, and that cluster's
// server URL (which isLocalServerURL judges).
type clusterProbe struct {
	cs        kubernetes.Interface
	serverURL string
}

// explainScanTimeout bounds the naming-only cluster scan explain runs on the
// §7.3 error path. The scan only makes the message better, so it must never
// make the failure slower than the failure itself: past this, explain falls
// back to the message it would have printed without looking.
const explainScanTimeout = 5 * time.Second

// loadClusterFn / newClientsetFn are the kubeconfig-load + clientset-build
// seams every command that reaches a cluster goes through — resolveClusterTarget
// (data ingest/list/delete), runClusterInfo, and runClusterDoctor. Production
// points them at the real cluster helpers; tests inject a fake ResolvedConfig +
// fake.NewSimpleClientset so the discovery + exit-code contract can be exercised
// without a real kubeconfig or apiserver. Same fn-var seam pattern used across
// this package (listDatasetsFn, doctorRunFn, …).
var (
	loadClusterFn  = cluster.Load
	newClientsetFn = cluster.NewClientset
)

// resolveClusterTargetFn is a test seam over resolveClusterTarget so a command
// test can inject a fully-resolved target (fake clientset + release + PVC)
// without seeding the k8s objects discoverRelease / DiscoverSharedPVC look for.
// Same fn-var seam pattern as loadClusterFn / listDatasetsFn.
var resolveClusterTargetFn = resolveClusterTarget

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
// leadRedirect threads to discoverRelease: pass true when this resolve is the
// command's first output (resources show/set, data list, seal — the redirect
// note then self-leads its one clean leading blank), false when the command has
// already printed something before resolving (data ingest's "Connecting…", data
// delete's warning — the note stays inline, no mid-output blank). See §380.
func resolveClusterTarget(ctx context.Context, p *ui.Printer, opts cluster.KubeconfigOptions, b activeClientBinding, needPVC, leadRedirect bool) (*clusterTarget, error) {
	resolved, err := loadClusterFn(opts)
	if err != nil {
		return nil, &exitError{code: exitLocalEnv, err: fmt.Errorf("loading kubeconfig: %w", err)}
	}
	cs, err := newClientsetFn(resolved)
	if err != nil {
		return nil, &exitError{code: exitLocalEnv, err: err}
	}
	// The cluster-wide fallback scan only engages when the target namespace is
	// the kubeconfig's default — i.e. nobody chose it: not the user (explicit
	// --namespace/--context) and not the active-client binding. A binding miss
	// must NOT silently redirect to some other client (§7.5 — that could be a
	// different machine's client); it keeps the §7.3 "runs elsewhere" message.
	release, nsUsed, err := discoverRelease(ctx, p, cs, resolved.Namespace, b.allowScan(), leadRedirect)
	if err != nil {
		// Only a genuine "namespace has no release" maps to the §7.3
		// "runs elsewhere" rewrite; an API/RBAC list failure or an
		// ambiguous multiple-release match keeps its own message.
		if errors.Is(err, cluster.ErrNoParentRelease) {
			return nil, &exitError{code: exitNoWorkspace, err: &noParentReleaseError{
				err:   err,
				probe: &clusterProbe{cs: cs, serverURL: resolved.ServerURL},
			}}
		}
		return nil, &exitError{code: exitNoWorkspace, err: err}
	}
	// The scan may have retargeted discovery to the namespace that actually
	// hosts the client; everything downstream (PVC discovery, dataset listing,
	// prints) keys on Resolved.Namespace, so it must follow.
	resolved.Namespace = nsUsed
	t := &clusterTarget{Resolved: resolved, Clientset: cs, Release: release}
	if needPVC {
		pvc, err := cluster.DiscoverSharedPVC(ctx, cs, resolved.Namespace)
		if err != nil {
			return nil, &exitError{code: exitNoWorkspace, err: err}
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
//
// leadRedirect says the redirect note, when it fires, is the command's FIRST
// output — so it self-leads a single blank to separate it from the shell prompt
// (§380: gives resources show/set/data list/seal one clean leading blank on the
// multi-client path without a pre-resolve Newline() that would stack into a
// double). Commands that print BEFORE resolving — cluster info's "Kubeconfig"
// section, data ingest's "Connecting…" line, data delete's warning paragraph —
// pass false so the note stays inline and never splits their output with a
// mid-blank.
func discoverRelease(ctx context.Context, p *ui.Printer, cs kubernetes.Interface, namespace string, allowScan, leadRedirect bool) (*cluster.ParentRelease, string, error) {
	release, err := cluster.DiscoverParentRelease(ctx, cs, namespace)
	if err == nil || !allowScan || !errors.Is(err, cluster.ErrNoParentRelease) {
		return release, namespace, err
	}
	found, scanErr := cluster.FindClientNamespaces(ctx, cs)
	if scanErr != nil {
		// The scan itself couldn't run (e.g. RBAC forbids the cluster-wide
		// list — a different problem than "not provisioned"). Keep the
		// original per-namespace discovery error rather than claiming the
		// machine has no client.
		return nil, namespace, err
	}
	if len(found) == 0 {
		// The scan SUCCEEDED and turned up nothing: the cluster the kubeconfig
		// reaches genuinely hosts no tracebloc client. Return a §7.10
		// "this machine isn't provisioned" message rather than the bare
		// per-namespace miss, which read like a namespace hunt. Still wraps
		// ErrNoParentRelease so errors.Is stays true and resolveClusterTarget
		// keeps mapping it to noParentReleaseError / exit 4.
		return nil, namespace, fmt.Errorf(
			"%w on the cluster your kubeconfig points at — if this machine should "+
				"have one, run the installer to provision it; otherwise point at the "+
				"right cluster with --context/--namespace",
			cluster.ErrNoParentRelease)
	}
	if len(found) > 1 {
		return nil, namespace, fmt.Errorf(
			"%w in namespace %q, but tracebloc clients are running in: %s. "+
				"Pass --namespace to pick one.",
			cluster.ErrNoParentRelease, namespace, strings.Join(found, ", "))
	}
	if p != nil {
		// Self-lead with a single blank ONLY when this redirect is the command's
		// opening line (leadRedirect) — resolve-first commands (resources
		// show/set, data list, seal) then get exactly one leading blank on the
		// multi-client path without a pre-resolve Newline() that would stack into
		// a double (§380). Output-first commands (cluster info, data ingest, data
		// delete) pass leadRedirect=false so the note stays inline and doesn't
		// split their already-open output with a mid-blank.
		if leadRedirect {
			p.Newline()
		}
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
// backward compatible for anyone who hasn't run `client create`.
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
//
// DIAGNOSE BEFORE ADVISING (#515). The shipped §7.3 sentence named no way back:
// it offered --namespace without ever saying WHICH namespace, so a user on a
// healthy local install had no supported recovery. explain now spends one
// naming-only cluster scan — the same read discoverRelease already spends
// purely to write a better message — and says what is actually here.
//
// This changes what the CLI SAYS, never what it TARGETS: allowScan() stays
// false, so a binding miss still never silently retargets to some other
// machine's client (§7.5). The scan's result reaches the user as text they must
// act on, which is the whole difference.
func (b activeClientBinding) explain(ctx context.Context, err error) error {
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
	// errors.New, not %w: the rewrite deliberately REPLACES the discovery error
	// rather than wrapping it, so the §7.3 guidance is the whole message and the
	// raw "no release in namespace X" doesn't trail it. That was already true of
	// the fmt.Errorf this replaced — the exit code (exitNoWorkspace, on the
	// *exitError above) is the machine-readable contract here, not the chain.
	// Wrapping would also make the result re-match errors.As(*noParentReleaseError)
	// and so re-explainable, which nothing wants.
	return &exitError{code: exitNoWorkspace,
		err: errors.New(repointMessage(handle, b.namespace, surveyCluster(ctx, npr.probe)))}
}

// clientSurvey is what explain managed to learn about the reached cluster
// before advising.
//
// looked distinguishes "we scanned and the cluster hosts none" from "we could
// not scan at all" (no probe on the error, or the cluster-wide list failed —
// RBAC, a timeout, an unreachable API server). Collapsing the two would let an
// absence of evidence print as evidence of absence: the CLI would tell a user
// with a perfectly healthy client that nothing is running here. When looked is
// false the message says nothing about the cluster's contents at all.
type clientSurvey struct {
	looked     bool
	namespaces []string
	local      bool // the kubeconfig's server is THIS machine (isLocalServerURL)
}

// surveyCluster runs cluster.FindClientNamespaces FOR NAMING ONLY — nothing in
// this path changes the namespace anything targets. A nil probe (synthesised
// error / test double) or a failed scan both return a survey that looked at
// nothing, so explain falls back to the message it printed before #515.
func surveyCluster(ctx context.Context, probe *clusterProbe) clientSurvey {
	if probe == nil || probe.cs == nil {
		return clientSurvey{}
	}
	ctx, cancel := context.WithTimeout(ctx, explainScanTimeout)
	defer cancel()
	found, err := cluster.FindClientNamespaces(ctx, probe.cs)
	if err != nil {
		return clientSurvey{}
	}
	return clientSurvey{looked: true, namespaces: found, local: isLocalServerURL(probe.serverURL)}
}

// repointMessage is the §7.3 error text, branched on what surveyCluster found.
// Pure (no I/O) so every branch is unit-testable as text.
//
//   - exactly one client on a LOCAL cluster — a cluster that IS this machine —
//     name it and offer the repoint. `client create` re-run on a cluster that
//     already hosts a client adopts it: no prompt, no new credential (§7.2).
//   - any client on a remote/shared cluster (or several anywhere) — name the
//     namespaces and offer ONLY --namespace. Never `client create` here: that
//     is the §7.5 boundary, and on a shared cluster the client we found may well
//     be a colleague's.
//   - none, scan clean — say so, and point at the installer, which is then the
//     correct advice rather than a guess.
//   - could not look — the pre-#515 sentence, unchanged. We make no claim.
//
// Each branch is ONE format literal rather than a concatenation, so the whole
// sentence lands in the copy catalog (zz-all-strings harvests literal arguments;
// a `+`-joined message is only ever half-visible there) and can be reviewed as
// the user reads it.
func repointMessage(handle, boundNS string, s clientSurvey) string {
	switch {
	case !s.looked:
		return fmt.Sprintf(
			"active client %q runs on another machine — namespace %q isn't on the cluster your kubeconfig points at; run this command there, or override with --namespace/--context",
			handle, boundNS)
	case len(s.namespaces) == 0:
		return fmt.Sprintf(
			"active client %q runs on another machine — namespace %q isn't on the cluster your kubeconfig points at; run this command there, or override with --namespace/--context.\n\nNo tracebloc client is running on this cluster either — if this machine should have one, set one up: %s",
			handle, boundNS, installer.Cmd)
	case len(s.namespaces) == 1 && s.local:
		return fmt.Sprintf(
			"active client %q runs on another machine — namespace %q isn't on the cluster your kubeconfig points at.\n\nA tracebloc client IS running on this machine, in namespace %q.\n  Point this machine at it:  %s client create\n      (this cluster already runs a client, so it adopts it — no new credential)\n  Or target it just this once:  --namespace %s",
			handle, boundNS, s.namespaces[0], launcher(), s.namespaces[0])
	case len(s.namespaces) == 1:
		return fmt.Sprintf(
			"active client %q runs on another machine — namespace %q isn't on the cluster your kubeconfig points at.\n\nA tracebloc client is running on this cluster, in namespace %q.\n  Target it just this once:  --namespace %s",
			handle, boundNS, s.namespaces[0], s.namespaces[0])
	default:
		return fmt.Sprintf(
			"active client %q runs on another machine — namespace %q isn't on the cluster your kubeconfig points at.\n\ntracebloc clients are running on this cluster, in namespaces: %s.\n  Target one just this once:  --namespace %s",
			handle, boundNS, strings.Join(s.namespaces, ", "), s.namespaces[0])
	}
}
