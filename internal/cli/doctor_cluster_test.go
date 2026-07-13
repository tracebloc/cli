package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/doctor"
	"github.com/tracebloc/cli/internal/ui"
)

// withDoctorRun seams doctorRunFn to return a fixed set of check results, so
// runClusterDoctor's render loop + verdict switch can be exercised with a
// controlled OK/Warn/Fail mix (the per-check logic is tested in internal/doctor).
func withDoctorRun(t *testing.T, results []doctor.Result) {
	t.Helper()
	orig := doctorRunFn
	t.Cleanup(func() { doctorRunFn = orig })
	doctorRunFn = func(context.Context, kubernetes.Interface, doctor.Options) []doctor.Result {
		return results
	}
}

// runDoctorClusterHalf drives runClusterDoctor with the auth half stubbed HEALTHY
// (signed in + active client + a 200 WhoAmI), so the overall verdict reflects the
// cluster checks rather than the auth section, and the cluster reached through the
// seams with doctor.Run returning `results`. Before this PR the cluster half — the
// ✓/⚠/✖ render loop and the verdict switch — was reachable only via a real
// kubeconfig pointing at an unroutable server (dial failure), never with a
// controlled result mix.
func runDoctorClusterHalf(t *testing.T, results []doctor.Result) (string, error) {
	t.Helper()
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", Email: "a@b.io", ActiveClientID: "5"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`)) // WhoAmI ok → auth OK
	})
	withClusterSeams(t, fake.NewSimpleClientset()) // cs only flows to doctorRunFn, which ignores it
	withDoctorRun(t, results)
	var buf bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&buf, ui.WithColor(false)), "", "", "")
	return buf.String(), err
}

// All three render arms fire (✓ detail, ⚠ + remedy hint, ✖ + remedy hint) and a
// single failing check drives the overall verdict to Fail → exit 2 with a silent
// error (the per-check lines already explained it) + the support-bundle hint.
func TestRunClusterDoctor_RendersAllStatusesAndFails(t *testing.T) {
	out, err := runDoctorClusterHalf(t, []doctor.Result{
		{Name: "Cluster reachable", Status: doctor.StatusOK, Detail: "api answered"},
		{Name: "Pod health", Status: doctor.StatusWarn, Detail: "1 pod restarted", Remedy: "check pod logs"},
		{Name: "Dataset volume", Status: doctor.StatusFail, Detail: "PVC not bound", Remedy: "provision the shared PVC"},
	})
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("a failing check → want exit 2, got %v", err)
	}
	if ee.err != nil {
		t.Errorf("the Fail verdict must be a silent exitError (per-check lines already printed), got err=%v", ee.err)
	}
	for _, want := range []string{
		"Cluster reachable", "api answered", // OK arm
		"Pod health", "1 pod restarted", "check pod logs", // Warn arm + remedy hint
		"Dataset volume", "PVC not bound", "provision the shared PVC", // Fail arm + remedy hint
		"Problems found", "support bundle", // Fail verdict + its hint
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Warnings but no failure → overall Warn → exit 0 (nil error) with the
// "completed with warnings" verdict.
func TestRunClusterDoctor_WarnVerdict(t *testing.T) {
	out, err := runDoctorClusterHalf(t, []doctor.Result{
		{Name: "Cluster reachable", Status: doctor.StatusOK, Detail: "ok"},
		{Name: "Proxy configuration", Status: doctor.StatusWarn, Detail: "not fully wired", Remedy: "set the proxy env"},
	})
	if err != nil {
		t.Fatalf("warnings-only → want nil error (exit 0), got %v", err)
	}
	if !strings.Contains(out, "Completed with warnings") {
		t.Errorf("want the warnings verdict:\n%s", out)
	}
}

// All checks pass (and auth is healthy) → overall OK → exit 0 with the all-healthy
// verdict. Also pins that the resolved namespace is printed in the Kubeconfig
// section (the seam resolves it to "default").
func TestRunClusterDoctor_AllHealthy(t *testing.T) {
	out, err := runDoctorClusterHalf(t, []doctor.Result{
		{Name: "Cluster reachable", Status: doctor.StatusOK, Detail: "api answered, client installed"},
		{Name: "Backend egress", Status: doctor.StatusOK, Detail: "reachable"},
	})
	if err != nil {
		t.Fatalf("all healthy → want nil error (exit 0), got %v", err)
	}
	if !strings.Contains(out, "All checks passed") {
		t.Errorf("want the all-healthy verdict:\n%s", out)
	}
	if !strings.Contains(out, "default") {
		t.Errorf("want the resolved namespace printed in the Kubeconfig section:\n%s", out)
	}
}

// The clientset-build arm: loadClusterFn succeeds but newClientsetFn fails. With
// the auth half healthy, this keeps the documented exit-3 contract (a kubeconfig-
// class failure) and surfaces the underlying error.
func TestRunClusterDoctor_ClientsetError(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", Email: "a@b.io", ActiveClientID: "5"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`)) // auth OK
	})
	origLoad, origCS := loadClusterFn, newClientsetFn
	t.Cleanup(func() { loadClusterFn, newClientsetFn = origLoad, origCS })
	loadClusterFn = func(cluster.KubeconfigOptions) (*cluster.ResolvedConfig, error) {
		return &cluster.ResolvedConfig{Namespace: "default", Context: "test-ctx"}, nil
	}
	newClientsetFn = func(*cluster.ResolvedConfig) (kubernetes.Interface, error) {
		return nil, errors.New("bad rest config")
	}
	var buf bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&buf, ui.WithColor(false)), "", "", "")
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 3 {
		t.Fatalf("clientset build error + auth-OK → want exit 3, got %v", err)
	}
	if !strings.Contains(buf.String(), "bad rest config") {
		t.Errorf("want the clientset error surfaced in the Cluster section:\n%s", buf.String())
	}
}
