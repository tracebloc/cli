package nodeboot

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner records invocations and returns canned output per command prefix.
type fakeRunner struct {
	calls   []string
	outputs map[string]string // key: joined-command prefix → output
	err     error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	for prefix, out := range f.outputs {
		if strings.HasPrefix(cmd, prefix) {
			return out, f.err
		}
	}
	return "", f.err
}

func withFakeRunner(t *testing.T, f *fakeRunner) {
	t.Helper()
	orig := Runner
	Runner = f.run
	t.Cleanup(func() { Runner = orig })
}

func TestClusterExists(t *testing.T) {
	f := &fakeRunner{outputs: map[string]string{"k3d cluster list": "tracebloc 1/1\nother 1/1"}}
	withFakeRunner(t, f)

	got, err := ClusterExists(context.Background(), "tracebloc")
	if err != nil || !got {
		t.Errorf("ClusterExists(tracebloc) = %v, %v; want true, nil", got, err)
	}
	got, err = ClusterExists(context.Background(), "absent")
	if err != nil || got {
		t.Errorf("ClusterExists(absent) = %v, %v; want false, nil", got, err)
	}
}

func TestTeardownCluster_DeletesWhenPresent(t *testing.T) {
	f := &fakeRunner{outputs: map[string]string{"k3d cluster list": "tracebloc 1/1"}}
	withFakeRunner(t, f)

	if err := TeardownCluster(context.Background(), "tracebloc"); err != nil {
		t.Fatal(err)
	}
	if !contains(f.calls, "k3d cluster delete tracebloc") {
		t.Errorf("expected a delete call, got %v", f.calls)
	}
}

func TestTeardownCluster_NoopWhenAbsent(t *testing.T) {
	f := &fakeRunner{outputs: map[string]string{"k3d cluster list": "othercluster 1/1"}}
	withFakeRunner(t, f)

	if err := TeardownCluster(context.Background(), "tracebloc"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "k3d cluster delete") {
			t.Errorf("must not delete an absent cluster, got %v", f.calls)
		}
	}
}

func TestUninstallChart_MissingReleaseIsNotAnError(t *testing.T) {
	f := &fakeRunner{err: errors.New(`Error: uninstall: Release not loaded: ns1: release: not found`)}
	withFakeRunner(t, f)

	if err := UninstallChart(context.Background(), "ns1"); err != nil {
		t.Errorf("a not-found release should be a no-op, got %v", err)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
