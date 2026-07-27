package helm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// sealRunner swaps Runner for a scripted fake and records every invocation, so
// the seal-check plumbing is exercised without helm or a cluster — the same
// double upgrade_test.go uses.
type sealRunner struct {
	calls [][]string
	out   string
	err   error
}

func (f *sealRunner) install(t *testing.T) {
	t.Helper()
	orig := Runner
	Runner = func(_ context.Context, name string, args ...string) (string, error) {
		f.calls = append(f.calls, append([]string{name}, args...))
		return f.out, f.err
	}
	t.Cleanup(func() { Runner = orig })
}

// hooksYAML mirrors what `helm get hooks` prints for the client chart
// (docs/SEAL-CHECK.md contract): two conformance Jobs carrying the seal
// labels (one also with the CLI's optional hint annotation), a non-runnable
// aux test hook (the storage check's ServiceAccount, negative hook-weight), a
// non-test hook that must be filtered out, and a legacy `test-success` Pod
// that must still count as a test.
const hooksYAML = `---
# Source: client/templates/egress-enforcement-check.yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: acme-egress-enforcement-check
  namespace: acme
  labels:
    app.kubernetes.io/instance: acme
    tracebloc.io/seal-check: "true"
    tracebloc.io/seal-check-name: egress-enforcement
  annotations:
    "helm.sh/hook": test
    "helm.sh/hook-delete-policy": before-hook-creation,hook-succeeded
    tracebloc.io/seal-hint: ensure the CNI enforces egress NetworkPolicy, then re-run
spec:
  backoffLimit: 0
---
# Source: client/templates/egress-reachability-check.yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: acme-egress-reachability-check
  namespace: acme
  labels:
    app.kubernetes.io/instance: acme
    tracebloc.io/seal-check: "true"
    tracebloc.io/seal-check-name: backend-reachability
  annotations:
    "helm.sh/hook": test
spec:
  backoffLimit: 0
---
# Source: client/templates/storage-assertions-check.yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: acme-storage-assertions-check
  annotations:
    "helm.sh/hook": test
    "helm.sh/hook-weight": "-6"
---
# Source: client/templates/some-migration.yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: acme-pre-upgrade-migrate
  annotations:
    "helm.sh/hook": pre-upgrade
---
apiVersion: v1
kind: Pod
metadata:
  name: acme-legacy-probe
  annotations:
    "helm.sh/hook": "test-success, something-else"
`

func TestListTestHooks_ParsesAndFilters(t *testing.T) {
	f := &sealRunner{out: hooksYAML}
	f.install(t)

	hooks, err := ListTestHooks(context.Background(), TestTarget{
		Release: "acme", Namespace: "acme",
		Kubeconfig: "/tmp/kc", KubeContext: "kind-acme",
	})
	if err != nil {
		t.Fatalf("ListTestHooks: %v", err)
	}

	// argv: get hooks on the right release/namespace, pinned to the resolved
	// cluster — never the ambient context.
	want := []string{"helm", "get", "hooks", "acme", "--namespace", "acme",
		"--kubeconfig", "/tmp/kc", "--kube-context", "kind-acme"}
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != strings.Join(want, " ") {
		t.Fatalf("helm argv = %v, want %v", f.calls, want)
	}

	if len(hooks) != 4 {
		t.Fatalf("got %d hooks (%+v), want 4 (the pre-upgrade hook filtered out)", len(hooks), hooks)
	}
	enf := hooks[0]
	if enf.Name != "acme-egress-enforcement-check" || enf.Kind != "Job" || !enf.SealCheck || !enf.Runnable() {
		t.Errorf("enforcement hook parsed wrong: %+v", enf)
	}
	if enf.SealName != "egress-enforcement" ||
		enf.SealHint != "ensure the CNI enforces egress NetworkPolicy, then re-run" {
		t.Errorf("seal name label / hint annotation parsed wrong: %+v", enf)
	}
	reach := hooks[1]
	if reach.SealName != "backend-reachability" || !reach.SealCheck || reach.SealHint != "" {
		t.Errorf("reachability hook parsed wrong: %+v", reach)
	}
	sa := hooks[2]
	if sa.Kind != "ServiceAccount" || sa.SealCheck || sa.Runnable() {
		t.Errorf("aux ServiceAccount hook parsed wrong (must be non-runnable, unlabelled): %+v", sa)
	}
	legacy := hooks[3]
	if legacy.Name != "acme-legacy-probe" || legacy.Kind != "Pod" || !legacy.Runnable() {
		t.Errorf("legacy test-success hook parsed wrong: %+v", legacy)
	}
}

// A release with no hooks prints nothing — that's an empty suite, not an error
// (the CLI turns it into the honest "unknown" verdict).
func TestListTestHooks_NoHooks(t *testing.T) {
	f := &sealRunner{out: ""}
	f.install(t)
	hooks, err := ListTestHooks(context.Background(), TestTarget{Release: "acme", Namespace: "acme"})
	if err != nil || len(hooks) != 0 {
		t.Fatalf("got %v, %v — want empty, nil", hooks, err)
	}
}

// A helm failure surfaces helm's own output (the actionable part), wrapped.
func TestListTestHooks_HelmError(t *testing.T) {
	f := &sealRunner{out: "Error: release: not found\n", err: errors.New("exit status 1")}
	f.install(t)
	_, err := ListTestHooks(context.Background(), TestTarget{Release: "ghost", Namespace: "ghost"})
	if err == nil || !strings.Contains(err.Error(), "release: not found") {
		t.Fatalf("want the helm output in the error, got: %v", err)
	}
}

// A manifest that doesn't parse must fail closed — dropping it could silently
// remove a conformance check from the verdict.
func TestListTestHooks_BadYAMLFailsClosed(t *testing.T) {
	f := &sealRunner{out: "kind: Job\nmetadata: {name: x\n"}
	f.install(t)
	_, err := ListTestHooks(context.Background(), TestTarget{Release: "acme", Namespace: "acme"})
	if err == nil || !strings.Contains(err.Error(), "parsing the release's hook manifests") {
		t.Fatalf("want a parse failure, got: %v", err)
	}
}

func TestRunTest_Argv(t *testing.T) {
	f := &sealRunner{out: "NAME: acme\n"}
	f.install(t)
	// One check plus an aux plumbing hook: both must land in ONE --filter
	// (comma-OR), so helm creates the check's dependencies but runs no other check.
	out, err := RunTest(context.Background(), TestTarget{
		Release: "acme", Namespace: "acme", Kubeconfig: "/tmp/kc", KubeContext: "kind-acme",
	}, []string{"acme-storage-assertions-check", "acme-storage-assertions-sa"}, 2*time.Minute)
	if err != nil || out != "NAME: acme\n" {
		t.Fatalf("RunTest = %q, %v", out, err)
	}
	want := []string{"helm", "test", "acme", "--namespace", "acme",
		"--filter", "name=acme-storage-assertions-check,name=acme-storage-assertions-sa",
		"--timeout", "2m0s",
		"--kubeconfig", "/tmp/kc", "--kube-context", "kind-acme"}
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != strings.Join(want, " ") {
		t.Fatalf("helm argv = %v, want %v", f.calls, want)
	}
}

// The raw error + output pass through unwrapped: the caller owns the per-check
// verdict (exit code) and the failure-detail extraction, and needs both intact.
func TestRunTest_PassesThroughFailure(t *testing.T) {
	f := &sealRunner{
		out: "Error: 1 error occurred:\n\t* job failed: BackoffLimitExceeded\n",
		err: errors.New("exit status 1"),
	}
	f.install(t)
	out, err := RunTest(context.Background(), TestTarget{Release: "acme", Namespace: "acme"}, []string{"acme-x"}, 0)
	if err == nil || err.Error() != "exit status 1" {
		t.Fatalf("want the raw runner error, got: %v", err)
	}
	if !strings.Contains(out, "BackoffLimitExceeded") {
		t.Fatalf("want the raw combined output, got: %q", out)
	}
	// timeout == 0 → no --timeout flag (helm's own default stands).
	if got := strings.Join(f.calls[0], " "); strings.Contains(got, "--timeout") {
		t.Fatalf("timeout 0 must not add --timeout: %v", got)
	}
}
