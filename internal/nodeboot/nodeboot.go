// Package nodeboot orchestrates the node-level bootstrap that `client create`
// now owns (prototype): standing up a local k3d cluster and installing the
// tracebloc/client Helm chart, plus the inverse teardown for `client delete`.
//
// PROTOTYPE SCOPE. This shells out to the same proven tools the bash installer
// uses (k3d, helm) rather than reimplementing cluster bootstrap natively, and
// covers the k3d-on-Linux path only. It deliberately does NOT reproduce the
// installer's GPU detection, proxy plumbing, values generation, or reboot
// policy (scripts/lib/* in tracebloc/client) — those remain the installer's job
// until this direction is formalized (reverses RFC-0001 §6.4's installer/CLI
// split; tracked as a prototype, not the design of record).
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

// Runner executes an external command and returns its combined output. A
// package var so tests can substitute a fake without spawning real processes.
var Runner = func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// run is the internal helper: runs a command, wrapping a failure with the tool
// name + its output so the caller surfaces an actionable error.
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

// TeardownCluster deletes the k3d cluster. A missing cluster is not an error
// (the delete is idempotent — the end state is "gone" either way).
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

// UninstallChart removes the client's Helm release. A missing release is not an
// error (idempotent teardown).
func UninstallChart(ctx context.Context, namespace string) error {
	_, err := run(ctx, "helm", "uninstall", namespace, "--namespace", namespace)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}
