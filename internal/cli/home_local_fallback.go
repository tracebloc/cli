package cli

// The home screen's environment probe, and the fallbacks it leans on when the
// provisioning pointer can't be trusted — never reached this machine at all
// (#401) or names a namespace that isn't on the reached cluster (#515). Split
// from home.go to respect its file budget; realProbeEnv moved here under #515
// because it is now mostly a decision about WHICH fallback to take, and reads
// better next to them than next to the renderer.

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/tracebloc/cli/internal/cluster"
)

// localEnvFallback answers "is there a secure environment on THIS machine?"
// when the active-client pointer cannot be trusted:
//
//   - it is EMPTY (#401) — the state every pre-#388 Windows install is in
//     permanently, because only `client create` writes the pointer and the
//     Windows installer never ran it. Field case: `doctor` said "Ready to run
//     training" while home said "No secure environment on this machine yet".
//   - it is SET BUT WRONG (#515) — it names a namespace that isn't on the
//     cluster this kubeconfig reaches (an orphaned record left by a cluster
//     recreation, a pointer written on another machine). #401 covered only the
//     empty case, so a wrong pointer went on recommending a reinstall over a
//     healthy install.
//
// Both are the same question, and the answer must not come from the pointer:
// this reloads the kubeconfig with NO binding applied, so it probes the
// namespace the kubeconfig itself selects — which is the client's own namespace
// on any installer-provisioned machine (install-client-helm.sh runs
// `kubectl config set-context --current --namespace <ns>`).
//
// The ownership gate in realProbeEnv exists so a status screen never greets a
// SHARED cluster's unrelated client as yours (§7.5). This fallback keeps that
// guarantee by adopting a discovered release only when the kubeconfig's server
// is LOCAL (loopback / host.docker.internal / a k3d wildcard bind) — a cluster
// that is this machine by definition, so whatever tracebloc release runs there
// is this machine's environment. Remote/shared clusters return the same honest
// no-release the gate always produced, and every error degrades to no-release
// (never "offline" — an unprovisioned machine with no reachable local cluster
// most likely has no environment at all).
func localEnvFallback(ctx context.Context) envProbe {
	resolved, err := loadClusterFn(cluster.KubeconfigOptions{})
	if err != nil {
		return envProbe{local: localNoRelease}
	}
	if !isLocalServerURL(resolved.ServerURL) {
		return envProbe{local: localNoRelease}
	}
	resolved.RestConfig.Timeout = homeProbeTimeout
	cs, err := newClientsetFn(resolved)
	if err != nil {
		return envProbe{local: localNoRelease}
	}
	// Namespace-only discovery — never the cluster-wide scan, mirroring the
	// gate's no-silent-retarget rule even on a local cluster.
	release, nsUsed, err := discoverRelease(ctx, nil, cs, resolved.Namespace, false, false)
	if err != nil {
		return envProbe{local: localNoRelease}
	}
	ep := envProbe{name: release.ReleaseName}
	if jobsManagerReady(ctx, cs, nsUsed, release) {
		ep.local = localLive
	} else {
		ep.local = localDegraded
	}
	if ep.local == localLive {
		if c, ok := machineCapacity(ctx, cs); ok {
			ep.compute, ep.hasCompute = c, true
		}
	}
	return ep
}

// localEnvNamespace reports the namespace the KUBECONFIG itself selects, and
// whether that reading is usable — i.e. the kubeconfig loads and the cluster it
// reaches is LOCAL (the #401 carve-out: a cluster that is this machine by
// definition, so whatever tracebloc release runs there is this machine's).
//
// It is the doctor-side half of localEnvFallback (#515), for the one caller that
// already holds a clientset and only needs the namespace to re-probe. Like the
// fallback it applies NO active-client binding — that pointer is precisely the
// thing under suspicion — and it never scans: the installer points the
// kubeconfig context at the client's namespace
// (install-client-helm.sh: `kubectl config set-context --current --namespace`),
// so reading it is enough and no cluster-wide list is spent. On a remote or
// shared cluster it returns false, so the ownership gate holds exactly as it
// does on the home screen.
func localEnvNamespace(opts cluster.KubeconfigOptions) (string, bool) {
	resolved, err := loadClusterFn(opts)
	if err != nil || resolved == nil {
		return "", false
	}
	if !isLocalServerURL(resolved.ServerURL) || resolved.Namespace == "" {
		return "", false
	}
	return resolved.Namespace, true
}

// isLocalServerURL reports whether a kubeconfig server URL points at THIS
// machine. Covers loopback names/addresses, the wildcard binds k3d writes when
// no host is pinned, and Docker Desktop's host alias (the same signals
// doctor's reachability remedy keys on).
func isLocalServerURL(serverURL string) bool {
	u, err := url.Parse(serverURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch strings.ToLower(host) {
	case "localhost", "host.docker.internal", "0.0.0.0", "::":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

// tbAliasAvailable reports whether a real tracebloc-owned `tb` alias sits next
// to this binary, so examples/remedies can echo the short name. On unix the
// installer symlinks `tb` → tracebloc and delete.go's aliasStatus judges
// ownership exactly as offboarding does. On Windows symlinks need admin, so
// install.ps1 writes a `tb.cmd` shim instead — a regular file the symlink test
// can never accept (#401): ours = the shim invokes this binary.
func tbAliasAvailable() bool {
	exe, err := osExecutable()
	if err != nil {
		return false
	}
	dir := filepath.Dir(exe)
	tb := filepath.Join(dir, binTB)
	if tb != exe {
		if _, ours := aliasStatus(tb, exe); ours {
			return true
		}
	}
	return tbCmdAliasOurs(dir, exe)
}

// tbCmdAliasOurs reports whether a tb.cmd shim in dir invokes THIS binary by
// its full path (install.ps1 writes an absolute target — the same ownership
// bar aliasStatus applies to symlinks). Matching just the basename would claim
// any third-party shim that merely mentions "tracebloc", or one invoking a
// different tracebloc at another path (Bugbot). Case-insensitive: .cmd is a
// Windows artifact and NTFS paths are case-insensitive.
func tbCmdAliasOurs(dir, exe string) bool {
	b, err := os.ReadFile(filepath.Join(dir, binTB+".cmd")) // #nosec G304 -- fixed name next to os.Executable(): inspects the install dir's own tb.cmd shim; whoever controls that dir already controls the binary.
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(b)), strings.ToLower(filepath.Clean(exe)))
}

// realProbeEnv is the bounded cluster probe. It reuses the exact namespace
// binding + discovery the data/cluster commands use, so the home screen reports
// the very environment those commands would target. Best-effort throughout: any
// failure degrades to unreachable/no-release, never an error.
func realProbeEnv(ctx context.Context) envProbe {
	ctx, cancel := context.WithTimeout(ctx, homeProbeTimeout)
	defer cancel()

	// The name for a discovered release is set below; the unreachable / no-release
	// returns leave it empty and let resolveHomeModel fill the remembered name, so
	// the "provisioned ⇒ named offline" fallback lives in exactly one place.
	opts := cluster.KubeconfigOptions{}
	binding := bindActiveClientNamespace(&opts)
	// OWNERSHIP GATE: no active-client binding ⇒ nothing was ever provisioned
	// for this profile, so no release the kubeconfig can reach is honestly
	// YOURS. Without the binding, discovery would fall back to the kubeconfig's
	// default namespace and then the cluster-wide scan — either can surface an
	// UNRELATED client (a shared cluster, a colleague's install), which this
	// screen would then greet as "your secure environment". The data commands
	// run that scan behind a visible retarget note and an explicit user action;
	// a status screen has neither, and §7.5's rule (a miss must never silently
	// retarget to some other client) applies doubly here. Report no-release —
	// resolveHomeModel renders the honest no-env screen (or a named offline via
	// the remembered-name fallback) — and skip the cluster I/O entirely, which
	// also keeps the common unprovisioned re-entry instant.
	if !binding.applied {
		// #401: an empty pointer isn't proof of "no environment" — the Windows
		// installer never writes it. localEnvFallback adopts a release only on
		// a LOCAL (loopback/k3d) cluster, so the shared-cluster guarantee above
		// is preserved; everything else still reads as no-release.
		return localEnvFallback(ctx)
	}
	resolved, err := loadClusterFn(opts)
	if err != nil {
		return envProbe{local: localUnreachable}
	}
	// Bound every API call so an unreachable API server can't hang the home
	// screen (mirrors cluster.ClusterID's time-boxed best-effort read).
	resolved.RestConfig.Timeout = homeProbeTimeout
	cs, err := newClientsetFn(resolved)
	if err != nil {
		return envProbe{local: localUnreachable}
	}

	release, nsUsed, err := discoverRelease(ctx, nil, cs, resolved.Namespace, binding.allowScan(), false)
	if err != nil {
		if errors.Is(err, cluster.ErrNoParentRelease) {
			// Cluster reachable, but this release isn't in the resolved context.
			// #515: a WRONG pointer is no more proof of "no environment" than the
			// empty one #401 covered — the binding above overrode the kubeconfig's
			// own namespace with a stale/foreign one, so this miss says nothing
			// about what runs here. Re-ask through the same local-only fallback:
			// it adopts a release ONLY when the kubeconfig's server is this
			// machine, so the shared-cluster guarantee is untouched, and every
			// other outcome is localNoRelease — exactly what this branch returned
			// before. Provisioned ⇒ resolveHomeModel turns that into a named
			// "offline".
			ep := localEnvFallback(ctx)
			// Mark it: the release the fallback found is not the client the
			// pointer names, so the heartbeat keyed on that pointer describes
			// someone else. resolveHomeModel refuses to render Online off this.
			ep.pointerStale = ep.local != localNoRelease
			return ep
		}
		// A list/RBAC/connect failure: we couldn't confirm what's here. Treat it
		// as unreachable (→ offline if provisioned, else no-env).
		return envProbe{local: localUnreachable}
	}

	ep := envProbe{name: release.ReleaseName}
	if jobsManagerReady(ctx, cs, nsUsed, release) {
		ep.local = localLive
	} else {
		ep.local = localDegraded
	}
	// Compute is only surfaced on the Online line, and only worth reading when the
	// environment is actually up.
	if ep.local == localLive {
		if c, ok := machineCapacity(ctx, cs); ok {
			ep.compute, ep.hasCompute = c, true
		}
	}
	return ep
}
