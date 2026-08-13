package nodeboot

import (
	"context"
	"errors"
	"path"
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

// Fixtures for the reclaim tests are image names a PRODUCER actually publishes
// under the org's ghcr namespace, not names invented to match the code under test.
// A fixture copied from the constant it is meant to guard can only ever confirm
// self-consistency — which is how the constant's scope went unexamined (backend#1861).
const (
	// The GPU node image the Windows GPU installer pulls into the host docker daemon
	// (client's scripts/install-k8s.ps1) and docker/k3s-cuda/build.sh publishes. The
	// TAG here is illustrative — the k3s/CUDA pins live in the client repo, and this
	// assertion is about the repository, not the pin — but the repository is exact.
	prodGPUNodeImage = "ghcr.io/tracebloc/k3s-cuda:v1.29.4-k3s1-cuda-12.4.1-base-ubuntu22.04"
	// The ingestor, the org's other published ghcr container package. It normally runs
	// as an in-cluster Job rather than landing in the host daemon, but if it ever does,
	// it is in scope.
	prodIngestorImage = "ghcr.io/tracebloc/ingestor:0.8"
	// A chart image, as the host daemon would print it: Docker Hub images are stored
	// under their SHORT name, so this is what a `tracebloc/*` filter would match — and
	// what imageReference must NOT reach.
	prodChartImage = "tracebloc/jobs-manager:prod"
)

func TestPruneImages(t *testing.T) {
	// Removal is by REFERENCE (repo:tag), not image ID (-q): an ID shared across
	// repos refuses `docker rmi <id>` ("must be forced"). Removing by reference
	// untags only our refs and never needs a force.
	const listCmd = `docker images --filter=reference=ghcr.io/tracebloc/* --format {{.Repository}}:{{.Tag}}`

	t.Run("removes tracebloc images by reference, scoped + deduped", func(t *testing.T) {
		f := newFakeRunner()
		// A reference listed twice must be passed to rmi once.
		f.on(listCmd, prodGPUNodeImage+"\n"+prodIngestorImage+"\n"+prodGPUNodeImage+"\n", nil)
		f.install(t)

		n, err := PruneImages(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("removed count = %d, want 2 (the deduped reference count)", n)
		}
		want := []string{
			listCmd,
			"docker rmi " + prodGPUNodeImage + " " + prodIngestorImage,
		}
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("calls = %v, want %v", f.calls, want)
		}
		// SCOPED contract: never a blanket prune, and never a force (which could
		// evict an image a non-tracebloc workload shares — the whole reason we
		// switched from -q/ID to by-reference).
		for _, c := range f.calls {
			if strings.Contains(c, "system prune") {
				t.Fatalf("PruneImages must never run `docker system prune`; got %q", c)
			}
			if strings.Contains(c, "rmi") && (strings.Contains(c, " -f") || strings.Contains(c, "--force")) {
				t.Fatalf("PruneImages must never force-remove; got %q", c)
			}
		}
	})

	t.Run("dangling <none> references are skipped", func(t *testing.T) {
		f := newFakeRunner()
		f.on(listCmd, prodGPUNodeImage+"\n<none>:<none>\n"+prodIngestorImage+"\n", nil)
		f.install(t)

		n, err := PruneImages(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// The skipped <none> must not be counted as reclaimed either.
		if n != 2 {
			t.Fatalf("removed count = %d, want 2 (<none> is not reclaimed)", n)
		}
		want := []string{
			listCmd,
			"docker rmi " + prodGPUNodeImage + " " + prodIngestorImage,
		}
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("calls = %v, want %v (must skip <none>)", f.calls, want)
		}
	})

	t.Run("no matching images reports 0 removed, not a silent success", func(t *testing.T) {
		f := newFakeRunner()
		f.on(listCmd, "\n  \n", nil)
		f.install(t)

		// The ordinary outcome on every non-Windows-GPU host. It must be reportable as
		// "nothing reclaimed" — a count of 0 with no error — so `tracebloc delete`
		// cannot tell the operator it reclaimed disk it never touched (backend#1861).
		n, err := PruneImages(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 0 {
			t.Fatalf("removed count = %d, want 0", n)
		}
		want := []string{listCmd} // no rmi when there's nothing to remove
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("calls = %v, want %v", f.calls, want)
		}
	})

	t.Run("best-effort: rmi failure surfaces to the caller, and claims nothing removed", func(t *testing.T) {
		f := newFakeRunner()
		f.on(listCmd, prodGPUNodeImage, nil)
		f.on("docker rmi "+prodGPUNodeImage, "image is being used by running container", errors.New("exit 1"))
		f.install(t)

		// PruneImages returns the error; the CALLER (tracebloc delete) treats it as
		// best-effort and only notes it — that policy lives in the command, not here.
		n, err := PruneImages(context.Background())
		if err == nil {
			t.Fatal("want the rmi error surfaced, got nil")
		}
		if n != 0 {
			t.Fatalf("removed count = %d, want 0: the rmi that would have reclaimed it failed", n)
		}
	})

	t.Run("listing failure surfaces", func(t *testing.T) {
		f := newFakeRunner()
		f.on(listCmd, "", errors.New("docker daemon not running"))
		f.install(t)

		if _, err := PruneImages(context.Background()); err == nil {
			t.Fatal("want the docker images error surfaced, got nil")
		}
	})
}

// TestImageReferenceScope guards the reclaim's SCOPE against producer facts rather
// than against itself. TestPruneImages can only check the shape of the commands —
// its fake Runner is keyed on the exact command string, so it agrees with whatever
// imageReference happens to say. That is why the scope went unexamined until
// backend#1861.
//
// The two directions below are the decision, encoded: the pattern must cover the
// GPU node image the installer really pulls into the host docker daemon, and must
// NOT reach Docker Hub's tracebloc namespace, where the chart images (which the host
// daemon never holds — containerd inside the k3d node pulls them) and a developer's
// own locally built images live. Widening it to `tracebloc/*` reddens this test;
// read imageReference's comment before changing either side.
//
// Scope only: this asserts which namespace the pattern selects, and deliberately does
// not re-implement docker's reference matcher. That `docker images
// --filter=reference=docker.io/tracebloc/*` matches nothing while `tracebloc/*`
// matches Hub images — because the daemon stores Hub images under their short name —
// was confirmed against a real daemon by hand, and is recorded in imageReference.
func TestImageReferenceScope(t *testing.T) {
	t.Run("covers the images published under the org's ghcr namespace", func(t *testing.T) {
		for _, ref := range []string{prodGPUNodeImage, prodIngestorImage} {
			ok, err := path.Match(imageReference, ref)
			if err != nil {
				t.Fatalf("bad pattern %q: %v", imageReference, err)
			}
			if !ok {
				t.Errorf("imageReference %q does not cover %q — the reclaim would miss an image the installer pulls into the host daemon", imageReference, ref)
			}
		}
	})

	t.Run("does not reach Docker Hub's tracebloc namespace", func(t *testing.T) {
		// Widening to reach these buys nothing (the host daemon never holds a chart
		// image) and costs a developer their locally built images.
		for _, ref := range []string{
			prodChartImage,
			"tracebloc/resource-monitor:prod",
			"tracebloc/mysql-client:prod",
			"tracebloc/client-image_classification-cpu:local",
		} {
			ok, err := path.Match(imageReference, ref)
			if err != nil {
				t.Fatalf("bad pattern %q: %v", imageReference, err)
			}
			if ok {
				t.Errorf("imageReference %q reaches %q — an offboard must not remove Docker Hub tracebloc images the installer never pulled", imageReference, ref)
			}
		}
	})
}
