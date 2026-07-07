package nodeboot

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeRunner records the commands it's asked to run and returns scripted output
// keyed by the "name arg0 arg1 …" join, so a test can assert both the exact
// commands issued (order + args) and drive per-command output/errors — no real
// k3d/helm/docker ever runs.
type fakeRunner struct {
	// responses maps a full command line ("k3d cluster list --no-headers") to its
	// canned (output, error). A command with no entry returns ("", nil).
	responses map[string]struct {
		out string
		err error
	}
	calls []string // every command line, in the order it was invoked
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{responses: map[string]struct {
		out string
		err error
	}{}}
}

func (f *fakeRunner) on(cmdline, out string, err error) {
	f.responses[cmdline] = struct {
		out string
		err error
	}{out, err}
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, line)
	r := f.responses[line]
	return r.out, r.err
}

// install swaps the package Runner for the fake for the duration of the test.
func (f *fakeRunner) install(t *testing.T) {
	t.Helper()
	orig := Runner
	Runner = f.run
	t.Cleanup(func() { Runner = orig })
}

func TestClusterExists(t *testing.T) {
	tests := []struct {
		name    string
		listOut string
		listErr error
		target  string
		want    bool
		wantErr bool
	}{
		{name: "present", listOut: "tracebloc 1/1 0/0\nother 1/1 0/0", target: "tracebloc", want: true},
		{name: "absent", listOut: "other 1/1 0/0", target: "tracebloc", want: false},
		{name: "no clusters at all", listOut: "", target: "tracebloc", want: false},
		{name: "list fails", listErr: errors.New("boom"), target: "tracebloc", wantErr: true},
		// A substring match must NOT count: "tracebloc-old" is a different cluster.
		{name: "substring is not a match", listOut: "tracebloc-old 1/1 0/0", target: "tracebloc", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRunner()
			f.on("k3d cluster list --no-headers", tc.listOut, tc.listErr)
			f.install(t)

			got, err := ClusterExists(context.Background(), tc.target)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ClusterExists = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTeardownCluster(t *testing.T) {
	t.Run("deletes when present", func(t *testing.T) {
		f := newFakeRunner()
		f.on("k3d cluster list --no-headers", "tracebloc 1/1 0/0", nil)
		f.install(t)

		if err := TeardownCluster(context.Background(), "tracebloc"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"k3d cluster list --no-headers", "k3d cluster delete tracebloc"}
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("calls = %v, want %v", f.calls, want)
		}
	})

	t.Run("missing cluster is a no-op, not an error", func(t *testing.T) {
		f := newFakeRunner()
		f.on("k3d cluster list --no-headers", "other 1/1 0/0", nil)
		f.install(t)

		if err := TeardownCluster(context.Background(), "tracebloc"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Only the list ran — no delete on an absent cluster.
		want := []string{"k3d cluster list --no-headers"}
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("calls = %v, want %v", f.calls, want)
		}
	})

	t.Run("delete failure surfaces", func(t *testing.T) {
		f := newFakeRunner()
		f.on("k3d cluster list --no-headers", "tracebloc 1/1 0/0", nil)
		f.on("k3d cluster delete tracebloc", "cannot delete", errors.New("exit 1"))
		f.install(t)

		if err := TeardownCluster(context.Background(), "tracebloc"); err == nil {
			t.Fatal("want error from k3d cluster delete, got nil")
		}
	})
}

func TestUninstallChart(t *testing.T) {
	t.Run("uninstalls the release", func(t *testing.T) {
		f := newFakeRunner()
		f.install(t)

		if err := UninstallChart(context.Background(), "munich-radiology", "", ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"helm uninstall munich-radiology --namespace munich-radiology"}
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("calls = %v, want %v", f.calls, want)
		}
	})

	t.Run("swallows not-found", func(t *testing.T) {
		f := newFakeRunner()
		f.on("helm uninstall gone --namespace gone", "Error: uninstall: Release not loaded: gone: release: not found", errors.New("exit 1"))
		f.install(t)

		if err := UninstallChart(context.Background(), "gone", "", ""); err != nil {
			t.Fatalf("a missing release must be swallowed, got: %v", err)
		}
	})

	t.Run("surfaces a real failure", func(t *testing.T) {
		f := newFakeRunner()
		f.on("helm uninstall ns --namespace ns", "Error: connection refused", errors.New("exit 1"))
		f.install(t)

		if err := UninstallChart(context.Background(), "ns", "", ""); err == nil {
			t.Fatal("want error for a non-not-found helm failure, got nil")
		}
	})

	t.Run("a non-release 'not found' still surfaces", func(t *testing.T) {
		// Only helm's "release: not found" is the idempotent no-op. An unrelated
		// failure whose output merely contains "not found" must NOT be swallowed,
		// or the offboard reports a phantom uninstall while the release lingers.
		f := newFakeRunner()
		f.on("helm uninstall ns --namespace ns",
			`Error: Kubernetes cluster unreachable: namespace "kube-system" not found`, errors.New("exit 1"))
		f.install(t)

		if err := UninstallChart(context.Background(), "ns", "", ""); err == nil {
			t.Fatal("a cluster-unreachable 'not found' must surface, got nil")
		}
	})

	t.Run("kubeconfig + context are passed to helm", func(t *testing.T) {
		f := newFakeRunner()
		f.install(t)

		if err := UninstallChart(context.Background(), "ns", "/tmp/kc.yaml", "k3d-tracebloc"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"helm uninstall ns --namespace ns --kubeconfig /tmp/kc.yaml --kube-context k3d-tracebloc"}
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("calls = %v, want %v", f.calls, want)
		}
	})
}

func TestPruneImages(t *testing.T) {
	t.Run("removes tracebloc images, scoped + deduped", func(t *testing.T) {
		f := newFakeRunner()
		// Two tags of the same image list the ID twice; PruneImages must pass it once.
		f.on(`docker images --filter=reference=ghcr.io/tracebloc/* -q`, "aaa111\nbbb222\naaa111\n", nil)
		f.install(t)

		if err := PruneImages(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{
			`docker images --filter=reference=ghcr.io/tracebloc/* -q`,
			"docker rmi aaa111 bbb222",
		}
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("calls = %v, want %v", f.calls, want)
		}
		// Guard the SCOPED contract: an offboard must never blanket-prune.
		for _, c := range f.calls {
			if strings.Contains(c, "system prune") {
				t.Fatalf("PruneImages must never run `docker system prune`; got %q", c)
			}
		}
	})

	t.Run("no matching images is a clean no-op", func(t *testing.T) {
		f := newFakeRunner()
		f.on(`docker images --filter=reference=ghcr.io/tracebloc/* -q`, "\n  \n", nil)
		f.install(t)

		if err := PruneImages(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// No rmi when there's nothing to remove.
		want := []string{`docker images --filter=reference=ghcr.io/tracebloc/* -q`}
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("calls = %v, want %v", f.calls, want)
		}
	})

	t.Run("best-effort: rmi failure surfaces to the caller", func(t *testing.T) {
		f := newFakeRunner()
		f.on(`docker images --filter=reference=ghcr.io/tracebloc/* -q`, "aaa111", nil)
		f.on("docker rmi aaa111", "image is being used by running container", errors.New("exit 1"))
		f.install(t)

		// PruneImages returns the error; the CALLER (tracebloc delete) treats it as
		// best-effort and only warns — that policy lives in the command, not here.
		if err := PruneImages(context.Background()); err == nil {
			t.Fatal("want the rmi error surfaced, got nil")
		}
	})

	t.Run("listing failure surfaces", func(t *testing.T) {
		f := newFakeRunner()
		f.on(`docker images --filter=reference=ghcr.io/tracebloc/* -q`, "", errors.New("docker daemon not running"))
		f.install(t)

		if err := PruneImages(context.Background()); err == nil {
			t.Fatal("want the docker images error surfaced, got nil")
		}
	})
}
