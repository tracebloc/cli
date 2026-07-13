//go:build integration

package integration

// End-to-end coverage of the top-level `tracebloc delete` offboard teardown
// (cli#140, RFC-0001 §7.10). The offboard orchestration in
// internal/cli/delete.go is unit-tested with fakes; this file exercises the
// two seams the unit suite mocks out, against REAL tooling:
//
//   - TestE2E_RevokeUsesPostNotDelete drives the actual RevokeClient HTTP call
//     over the wire against a recording stub, asserting acceptance (a): the
//     credential is REVOKED via POST /edge-device/<id>/revoke/ — never a hard
//     DELETE of the row (which would cascade the retained training history).
//
//   - TestE2E_DeleteTeardown builds the real `tracebloc` binary and runs
//     `tracebloc delete --yes --force` black-box against a throwaway k3d
//     cluster with a real Helm release installed, asserting acceptances
//     (b) the Helm release is uninstalled, (c) the k3d cluster is deleted,
//     (d) ~/.tracebloc is wiped, and (e) the foreign-`tb` guard (#171) leaves
//     a `tb` alias it did not create in place.
//
// WHY THE REVOKE IS STUBBED, NOT LIVE. The CLI has no base-URL override — the
// backend host is env-mapped (dev/stg/prod), so a black-box binary run cannot
// be pointed at a local backend, and a fake token against a real backend would
// 401 and abort the teardown before it starts. So the black-box run is kept
// fully OFFLINE (all egress routed through a dead proxy), which drives the
// revoke down its documented best-effort transport-failure path (warn +
// continue, `revoked=false`) so the LOCAL teardown (helm/k3d/data/self) runs
// for real. The POST-not-DELETE revoke CONTRACT that a live backend would
// enforce is covered instead by TestE2E_RevokeUsesPostNotDelete, a real HTTP
// round-trip against a recording stub. See scope_deferred: end-to-end revoke
// against a live backend (the actual server-side row-preserving revoke) is the
// one piece CI cannot provide and is left to the pre-prod FR on a real env.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracebloc/cli/internal/api"
)

// e2eClusterName is the k3d cluster name the CLI hard-codes for teardown
// (nodeboot.ClusterName). The throwaway cluster MUST use this name or
// `tracebloc delete` won't find it to delete.
const e2eClusterName = "tracebloc"

// TestE2E_RevokeUsesPostNotDelete proves acceptance (a): offboarding REVOKES
// the machine credential via POST /edge-device/<id>/revoke/ and never issues a
// hard DELETE of the client row (the row is kept as history per §7.10). It is a
// real HTTP round-trip against a recording stub — the exact method + path +
// trailing slash a live backend's DRF route enforces.
func TestE2E_RevokeUsesPostNotDelete(t *testing.T) {
	var (
		mu   sync.Mutex
		reqs []string // "<METHOD> <PATH>" in arrival order
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.Path)
		mu.Unlock()
		// The revoke @action returns 2xx with no meaningful body on success.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &api.Client{BaseURL: srv.URL, Token: "stub-token", HTTP: srv.Client()}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const id = 42
	if err := c.RevokeClient(ctx, id); err != nil {
		t.Fatalf("RevokeClient against stub (200 OK): unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 1 {
		t.Fatalf("expected exactly one backend request, got %d: %v", len(reqs), reqs)
	}
	want := fmt.Sprintf("POST /edge-device/%d/revoke/", id)
	if reqs[0] != want {
		t.Errorf("revoke request = %q, want %q", reqs[0], want)
	}
	for _, req := range reqs {
		if strings.HasPrefix(req, http.MethodDelete+" ") {
			t.Errorf("offboard issued a hard DELETE (%q) — the row must be REVOKED, not destroyed", req)
		}
	}
}

// TestE2E_DeleteTeardown is the black-box offboard e2e. It requires a real k3d
// cluster + helm + docker and is OPT-IN via TB_E2E_K3D=1 (CI sets it) so it
// never creates/deletes a "tracebloc" k3d cluster on a developer's machine — a
// real dev cluster shares that name and must not be clobbered.
func TestE2E_DeleteTeardown(t *testing.T) {
	if os.Getenv("TB_E2E_K3D") != "1" {
		t.Skip("set TB_E2E_K3D=1 to run the k3d offboard e2e (creates + deletes a throwaway k3d cluster named \"tracebloc\")")
	}
	requireTools(t, "go", "k3d", "helm", "docker")

	// Refuse to clobber a pre-existing "tracebloc" cluster we didn't create.
	if clusterExists(t) {
		t.Skipf("a k3d cluster %q already exists — refusing to delete a cluster this test didn't create", e2eClusterName)
	}

	repoRoot := repoRoot(t)
	const ns = "tbe2e"
	const kubeContext = "k3d-" + e2eClusterName

	// 1. Build the real product binary into a throwaway bin dir. The offboard
	//    removes this binary as its last step, so it must be a disposable copy.
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "tracebloc")
	build := exec.Command("go", "build", "-o", bin, "./cmd/tracebloc")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build tracebloc: %v\n%s", err, out)
	}

	// 2. Plant a FOREIGN `tb` next to the binary — a symlink that is NOT our
	//    alias (its target isn't this binary). The #171 guard must leave it be.
	foreignTB := filepath.Join(binDir, "tb")
	if err := os.Symlink("/bin/echo", foreignTB); err != nil {
		t.Fatalf("plant foreign tb: %v", err)
	}

	// 3. Throwaway k3d cluster (the box the CLI tears down). Registered for
	//    cleanup first so a failure mid-test still deletes it.
	t.Cleanup(func() { _ = run(exec.Command("k3d", "cluster", "delete", e2eClusterName)) })
	if out, err := runOut(exec.Command("k3d", "cluster", "create", e2eClusterName, "--wait", "--timeout", "180s")); err != nil {
		t.Fatalf("k3d cluster create: %v\n%s", err, out)
	}

	// 4. Install a REAL, image-free Helm release named for the namespace (the
	//    installer's release-name == namespace convention). A lone ConfigMap
	//    needs no image pull, so `helm install` (no --wait) returns at once.
	chartDir := writeMinimalChart(t)
	if out, err := runOut(exec.Command("helm", "install", ns, chartDir,
		"--namespace", ns, "--create-namespace", "--kube-context", kubeContext)); err != nil {
		t.Fatalf("helm install: %v\n%s", err, out)
	}
	// Prove it's really there before we ask the CLI to remove it.
	if out, err := runOut(exec.Command("helm", "list", "-n", ns, "-q", "--kube-context", kubeContext)); err != nil || !strings.Contains(out, ns) {
		t.Fatalf("precondition: helm release %q not installed (err=%v out=%q)", ns, err, out)
	}

	// 5. Seed a signed-in config in a temp ~/.tracebloc naming the active client
	//    + its namespace, so the offboard has something to tear down.
	cfgDir := t.TempDir()
	writeConfig(t, cfgDir, ns)

	// 6. Run the offboard black-box. Kept fully OFFLINE: all egress goes through
	//    a dead proxy (127.0.0.1:1, always refused) so the revoke fails as a
	//    transport error (best-effort, non-terminal) and NO real backend is
	//    contacted — while NO_PROXY exempts the local k3d API server + docker so
	//    helm/k3d still work. --force skips the online guard (no ListClients
	//    call); --yes skips the typed-name confirm.
	cmd := exec.Command(bin, "delete", "--yes", "--force", "--context", kubeContext)
	cmd.Env = append(os.Environ(),
		"TRACEBLOC_CONFIG_DIR="+cfgDir,
		"HTTP_PROXY=http://127.0.0.1:1",
		"HTTPS_PROXY=http://127.0.0.1:1",
		"ALL_PROXY=http://127.0.0.1:1",
		"NO_PROXY=127.0.0.1,localhost,::1,0.0.0.0",
		"no_proxy=127.0.0.1,localhost,::1,0.0.0.0",
	)
	outBytes, err := cmd.CombinedOutput()
	out := string(outBytes)
	t.Logf("`tracebloc delete` output:\n%s", out)
	if err != nil {
		// runDelete returns nil on the best-effort teardown path (revoke failed,
		// local steps succeeded), so a non-zero exit is a real failure.
		t.Fatalf("`tracebloc delete` exited non-zero: %v", err)
	}

	// (a) revoke path: with no backend reachable, the offboard must take the
	//     honest best-effort branch and SAY the credential may still be live —
	//     never claim a clean revoke it didn't perform.
	if !strings.Contains(out, "server-side revoke didn't complete") {
		t.Errorf("expected the best-effort revoke wording (no backend reachable); got:\n%s", out)
	}

	// (b) helm release uninstalled — the success line is printed only when the
	//     real `helm uninstall` exit-0'd against the live cluster.
	if !strings.Contains(out, "Uninstalled the Helm release "+ns) {
		t.Errorf("expected Helm release %q to be uninstalled; got:\n%s", ns, out)
	}

	// (c) k3d cluster deleted — assert both the CLI's success line and the
	//     authoritative `k3d cluster list`.
	if !strings.Contains(out, fmt.Sprintf("Deleted the local cluster %q", e2eClusterName)) {
		t.Errorf("expected the local cluster %q to be deleted; got:\n%s", e2eClusterName, out)
	}
	if clusterExists(t) {
		t.Errorf("k3d cluster %q still exists after offboard", e2eClusterName)
	}

	// (d) ~/.tracebloc wiped (default path, no --keep-data).
	if _, statErr := os.Stat(cfgDir); !os.IsNotExist(statErr) {
		t.Errorf("config dir %s should be wiped, stat err = %v", cfgDir, statErr)
	}

	// (e) foreign-`tb` guard (#171): a `tb` the installer didn't create is left
	//     in place, and the CLI says so.
	if _, lerr := os.Lstat(foreignTB); lerr != nil {
		t.Errorf("foreign `tb` alias was removed (%v) — the #171 guard must leave a non-tracebloc `tb` alone", lerr)
	}
	if !strings.Contains(out, "isn't tracebloc's `tb` alias") {
		t.Errorf("expected the offboard to report it left the foreign `tb` alias; got:\n%s", out)
	}

	// Sanity: the CLI removed ITSELF (last step) — proves the self-remove ran.
	if _, serr := os.Stat(bin); !os.IsNotExist(serr) {
		t.Errorf("the CLI binary should have removed itself, stat err = %v", serr)
	}
}

// requireTools skips the test unless every named executable is on PATH.
func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%q not on PATH — skipping (needed by the k3d offboard e2e)", tool)
		}
	}
}

// clusterExists reports whether the throwaway k3d cluster is present.
func clusterExists(t *testing.T) bool {
	t.Helper()
	out, err := runOut(exec.Command("k3d", "cluster", "list", "--no-headers"))
	if err != nil {
		t.Fatalf("k3d cluster list: %v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == e2eClusterName {
			return true
		}
	}
	return false
}

// repoRoot resolves the module root from this test file's location
// (test/integration → ../..).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// writeConfig seeds a v2 signed-in config.json under dir, naming the active
// client + its namespace so the offboard has a release to tear down.
func writeConfig(t *testing.T, dir, namespace string) {
	t.Helper()
	cfg := fmt.Sprintf(`{
  "version": 2,
  "current_env": "prod",
  "profiles": {
    "prod": {
      "email": "e2e@tracebloc.io",
      "token": "stub-token",
      "active_client_id": "42",
      "active_client_name": "e2e-client",
      "active_client_namespace": %q
    }
  }
}
`, namespace)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

// writeMinimalChart writes an image-free Helm chart (a single ConfigMap) so
// `helm install` succeeds instantly without pulling any image, giving the
// offboard a real release to uninstall.
func writeMinimalChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"),
		[]byte("apiVersion: v2\nname: tbe2e\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatalf("write Chart.yaml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatalf("mkdir templates: %v", err)
	}
	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: tb-e2e-marker\ndata:\n  hello: world\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "cm.yaml"), []byte(cm), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}
	return dir
}

// run executes a command, discarding output, returning its error.
func run(cmd *exec.Cmd) error {
	_, err := runOut(cmd)
	return err
}

// runOut executes a command and returns its combined output + error.
func runOut(cmd *exec.Cmd) (string, error) {
	out, err := cmd.CombinedOutput()
	return string(out), err
}
