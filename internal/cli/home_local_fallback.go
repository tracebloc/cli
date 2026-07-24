package cli

// Home-screen fallbacks for machines the provisioning pointer never reached
// (#401). Split from home.go to respect its file budget.

import (
	"context"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/tracebloc/cli/internal/cluster"
)

// localEnvFallback answers "is there a secure environment on THIS machine?"
// when the active-client pointer is empty — the state every pre-#388 Windows
// install is in permanently, because only `client create` writes the pointer
// and the Windows installer never ran it. Field case: `doctor` said "Ready to
// run training" while home said "No secure environment on this machine yet".
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
	release, nsUsed, err := discoverRelease(ctx, nil, cs, resolved.Namespace, false)
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

// tbCmdAliasOurs reports whether a tb.cmd shim in dir invokes exe (matched on
// the binary's basename, case-insensitively — .cmd is a Windows artifact).
func tbCmdAliasOurs(dir, exe string) bool {
	b, err := os.ReadFile(filepath.Join(dir, binTB+".cmd"))
	if err != nil {
		return false
	}
	base := strings.TrimSuffix(filepath.Base(exe), ".exe")
	return strings.Contains(strings.ToLower(string(b)), strings.ToLower(base))
}
