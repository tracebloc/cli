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

// imageReference is the ghcr namespace the installer pulls tracebloc images from.
// PruneImages is SCOPED to this reference so an offboard reclaims only tracebloc's
// images — never a blanket `docker system prune` that would evict images other
// workloads on the host depend on (RFC-0001 §7.10 "never a blanket prune").
const imageReference = "ghcr.io/tracebloc/*"

// Runner executes an external command and returns its combined output. A package
// var so tests can substitute a fake without spawning real k3d/helm/docker.
var Runner = func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
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
func UninstallChart(ctx context.Context, namespace string) error {
	_, err := run(ctx, "helm", "uninstall", namespace, "--namespace", namespace)
	// Match helm's release-not-found wording specifically ("... release: not
	// found"), not a bare "not found" — an unrelated failure whose output happens
	// to contain "not found" (e.g. "Kubernetes cluster unreachable: ... not
	// found") must surface, not be swallowed as an idempotent no-op.
	if err != nil && strings.Contains(err.Error(), "release: not found") {
		return nil
	}
	return err
}

// PruneImages reclaims the tracebloc container images pulled during install —
// `docker images --filter=reference="ghcr.io/tracebloc/*" -q | docker rmi`. It is
// SCOPED to the tracebloc image reference and best-effort by design (RFC-0001
// §7.10): reclaiming disk is a nice-to-have on offboard, not a hard step, so a
// docker failure or an image still in use by a container is not fatal. It is
// NEVER a blanket `docker system prune`, which would evict images other workloads
// on the host depend on.
//
// No matching images (nothing to reclaim) is a clean no-op, not an error.
func PruneImages(ctx context.Context) error {
	out, err := run(ctx, "docker", "images", "--filter=reference="+imageReference, "-q")
	if err != nil {
		return err
	}
	// De-duplicate the image IDs: a single image tagged multiple times
	// (ghcr.io/tracebloc/x:1 and :latest) lists its ID once per tag, and passing
	// the same ID twice to `docker rmi` makes the second reference error.
	ids := dedupeLines(out)
	if len(ids) == 0 {
		return nil // nothing tracebloc-owned to reclaim
	}
	_, err = run(ctx, "docker", append([]string{"rmi"}, ids...)...)
	return err
}

// dedupeLines splits combined `docker images -q` output into unique, non-empty,
// order-preserving lines (image IDs).
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
