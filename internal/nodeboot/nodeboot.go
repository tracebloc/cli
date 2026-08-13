// Package nodeboot orchestrates the node-level teardown that `tracebloc delete`
// (offboarding, RFC-0001 §7.10) drives: uninstalling the tracebloc Helm release,
// deleting the local k3d cluster, and reclaiming the tracebloc container images.
//
// SCOPE. This shells out to the same proven tools the bash installer uses (k3d,
// helm, docker) rather than reimplementing anything natively. It is the inverse
// of the installer's node bootstrap; it deliberately does NOT touch shared
// system software (Docker/Homebrew/kubectl/k3d/helm/NVIDIA), never runs a blanket
// `docker system prune`, and never reboots — those are "left in place" per §7.10.
//
// It grew out of the cli#136 prototype (which owned k3d/helm on Linux only);
// PruneImages is the addition beyond that prototype.
package nodeboot

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ClusterName mirrors the bash installer's default k3d cluster name
// (scripts/lib/common.sh) so the CLI-driven teardown targets the same cluster.
const ClusterName = "tracebloc"

// imageReference scopes the offboard's image reclaim.
//
// `docker images` reads the HOST docker daemon, and the only tracebloc-namespaced
// image the installer ever puts there is the GPU node image the Windows GPU path
// pulls — `docker pull ghcr.io/tracebloc/k3s-cuda:<k3s>-cuda-<cuda-base>`, see the
// client repo's scripts/install-k8s.ps1 (which also builds it locally under the
// same name when no prebuilt one is reachable).
//
// The chart's own images are deliberately NOT in scope, and widening the pattern
// to reach them would be wrong twice over:
//
//   - They never enter the host daemon. Every chart image (jobs-manager,
//     pods-monitor, resource-monitor, mysql-client), the ingestor Job, and the
//     spawned training images are pulled by containerd INSIDE the k3d node, so
//     `docker images` on the host does not list them — nothing in the installer
//     does a host `docker pull` or `k3d image import` for them. Their disk is
//     reclaimed by `k3d cluster delete`, which runs before this step and takes the
//     node container's image store with it.
//   - Docker Hub images are stored under their SHORT name, so `tracebloc/*` is the
//     only filter form that would match them — and that also matches a developer's
//     locally built `tracebloc/…` images, which the installer never pulled and the
//     operator never asked us to delete.
//
// So the reclaim stays SCOPED to this reference rather than being a blanket
// `docker system prune` that would evict images other workloads on the host depend
// on (RFC-0001 §7.10 "never a blanket prune").
//
// Known gap: a mirror / air-gapped install re-homes the node image onto the
// operator's own registry host, which this pattern cannot name, so on that path the
// node image is left behind.
const imageReference = "ghcr.io/tracebloc/*"

// Runner executes an external command and returns its combined output. A package
// var so tests can substitute a fake without spawning real k3d/helm/docker.
var Runner = func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() // #nosec G204 -- test seam: callers pass literal tool names (k3d/helm/docker) with argv from package consts and the operator's own cluster/release names; no shell.
	return string(out), err
}

// run is the internal helper: runs a command through Runner, wrapping a failure
// with the tool name + its output so the caller surfaces an actionable error.
func run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := Runner(ctx, name, args...)
	if err != nil {
		return out, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return out, nil
}

// ClusterExists reports whether a k3d cluster named `name` is already present.
func ClusterExists(ctx context.Context, name string) (bool, error) {
	out, err := run(ctx, "k3d", "cluster", "list", "--no-headers")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == name {
			return true, nil
		}
	}
	return false, nil
}

// TeardownCluster deletes the k3d cluster. `k3d cluster delete` also prunes the
// cluster's entry from the kubeconfig, so the stale context doesn't linger. A
// missing cluster is not an error — the delete is idempotent (the end state is
// "gone" either way).
func TeardownCluster(ctx context.Context, name string) error {
	exists, err := ClusterExists(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err = run(ctx, "k3d", "cluster", "delete", name)
	return err
}

// UninstallChart removes the client's Helm release (release name = namespace, the
// installer's convention). A missing release is not an error (idempotent teardown).
//
// kubeconfig and kubeContext target the release's cluster: helm otherwise acts on
// the ambient $KUBECONFIG + current-context, so an operator whose current context
// isn't the tracebloc cluster could uninstall the wrong release. Both are appended
// only when non-empty, preserving the default-context behavior.
func UninstallChart(ctx context.Context, namespace, kubeconfig, kubeContext string) error {
	args := []string{"uninstall", namespace, "--namespace", namespace}
	if kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
	}
	if kubeContext != "" {
		args = append(args, "--kube-context", kubeContext)
	}
	_, err := run(ctx, "helm", args...)
	// Match helm's release-not-found wording specifically ("... release: not
	// found"), not a bare "not found" — an unrelated failure whose output happens
	// to contain "not found" (e.g. "Kubernetes cluster unreachable: ... not
	// found") must surface, not be swallowed as an idempotent no-op.
	if err != nil && strings.Contains(err.Error(), "release: not found") {
		return nil
	}
	return err
}

// PruneImages reclaims the tracebloc images the installer left in the host docker
// daemon, by reference — `docker images --filter=reference="ghcr.io/tracebloc/*"
// --format {{.Repository}}:{{.Tag}} | docker rmi` (by repo:tag, NOT image ID: a
// shared ID refuses `docker rmi <id>` — see the body). It is SCOPED to
// imageReference (read the rationale there before widening it) and best-effort by
// design (RFC-0001 §7.10): reclaiming disk is a nice-to-have on offboard, not a
// hard step, so a docker failure or an image still in use by a container is not
// fatal. It is NEVER a blanket `docker system prune`, which would evict images
// other workloads on the host depend on.
//
// It returns how many references it removed, so the caller can tell a real reclaim
// from a no-op instead of reporting reclaimed disk either way. No matching images
// is a clean no-op — (0, nil), not an error — and that is the ordinary outcome:
// only a Windows GPU host has a tracebloc image in the host daemon to begin with.
// A removal failure returns (0, err): nothing is claimed reclaimed when the `rmi`
// that would have reclaimed it did not succeed.
func PruneImages(ctx context.Context) (int, error) {
	// List by REFERENCE (repo:tag), not image ID (-q). An image ID shared across
	// multiple repositories refuses `docker rmi <id>` ("must be forced — image is
	// referenced in multiple repositories"). Removing by reference untags only OUR
	// tracebloc references — Docker deletes the underlying image only when nothing
	// else references it — so we never need a force that could evict an image a
	// non-tracebloc workload shares (cli delete image-reclaim bug).
	out, err := run(ctx, "docker", "images", "--filter=reference="+imageReference,
		"--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return 0, err
	}
	// Dedupe and drop dangling refs: an image that lost its tag prints "<none>"
	// for the repo or tag, which can't be removed by reference (`docker image
	// prune` reclaims those). Best-effort, so skipping them is fine.
	var refs []string
	for _, ref := range dedupeLines(out) {
		if strings.Contains(ref, "<none>") {
			continue
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return 0, nil // nothing tracebloc-owned in the host daemon to reclaim
	}
	if _, err = run(ctx, "docker", append([]string{"rmi"}, refs...)...); err != nil {
		return 0, err
	}
	return len(refs), nil
}

// dedupeLines splits combined `docker images` output into unique, non-empty,
// order-preserving lines (image IDs or repo:tag references).
func dedupeLines(out string) []string {
	seen := map[string]struct{}{}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}
