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

// withDoctorRun seams doctorRunFn to return a fixed set of granular check
// results, so runClusterDoctor's roll-up + verdict can be exercised with a
// controlled mix (the per-check logic is tested in internal/doctor).
func withDoctorRun(t *testing.T, results []doctor.Result) {
	t.Helper()
	orig := doctorRunFn
	t.Cleanup(func() { doctorRunFn = orig })
	doctorRunFn = func(context.Context, kubernetes.Interface, doctor.Options) []doctor.Result {
		return results
	}
}

// runDoctorClusterHalf drives runClusterDoctor with the identity half stubbed
// HEALTHY (signed in + a 200 WhoAmI) and the cluster reached through the seams
// with doctor.Run returning `results`, into a non-verbose printer.
func runDoctorClusterHalf(t *testing.T, results []doctor.Result) (string, error) {
	t.Helper()
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", Email: "a@b.io", ActiveClientID: "5"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`)) // WhoAmI ok → session healthy
	})
	withClusterSeams(t, fake.NewSimpleClientset()) // cs only flows to doctorRunFn, which ignores it
	withDoctorRun(t, results)
	var buf bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&buf, ui.WithColor(false)), "", "", "", false)
	return buf.String(), err
}

// All healthy → the two plain lines + the "ready to run training" verdict, exit 0.
// The resolved namespace is printed as the secure-environment name (the seam
// resolves it to "default").
func TestRunClusterDoctor_AllHealthy(t *testing.T) {
	out, err := runDoctorClusterHalf(t, []doctor.Result{
		{Name: "Cluster reachable", Status: doctor.StatusOK, Detail: "reachable"},
		{Name: "Backend egress (from this machine)", Status: doctor.StatusOK, Detail: "reachable"},
	})
	if err != nil {
		t.Fatalf("all healthy → want nil error (exit 0), got %v", err)
	}
	for _, want := range []string{
		"Signed in as a@b.io",
		`Secure environment "default"`,
		"Connected to tracebloc",
		"Ready to run training",
		"Everything looks good",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("healthy view missing %q:\n%s", want, out)
		}
	}
	// No Kubernetes vocabulary leaks into the default view.
	for _, banned := range []string{"Kubeconfig", "context", "PVC", "kubectl", "requests-proxy", "Pending"} {
		if strings.Contains(out, banned) {
			t.Errorf("leaked k8s term %q in default view:\n%s", banned, out)
		}
	}
}

// A failing cluster check rolls up into a plain ✖ line + concrete remedy, drives
// the verdict to Fail (exit 2, silent error), and points at the support bundle.
func TestRunClusterDoctor_FailRollsUp(t *testing.T) {
	out, err := runDoctorClusterHalf(t, []doctor.Result{
		{Name: "Cluster reachable", Status: doctor.StatusOK, Detail: "reachable"},
		{Name: "Dataset volume (PVC)", Status: doctor.StatusFail, Detail: "PVC not bound"},
	})
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("a failing check → want exit 2, got %v", err)
	}
	if ee.err != nil {
		t.Errorf("Fail verdict must be a silent exitError (lines already printed), got err=%v", ee.err)
	}
	for _, want := range []string{"Not ready", "dataset storage", "--diagnose"} {
		if !strings.Contains(out, want) {
			t.Errorf("failed view missing %q:\n%s", want, out)
		}
	}
	// The raw k8s detail ("PVC not bound") must NOT appear in the default view.
	if strings.Contains(out, "PVC not bound") {
		t.Errorf("raw k8s detail leaked into the default view:\n%s", out)
	}
}

// A Warn-level granular check (e.g. a proxy note) is not user-actionable and must
// NOT fail the verdict — it degrades to --verbose. Default view stays healthy.
func TestRunClusterDoctor_WarnsDontFail(t *testing.T) {
	out, err := runDoctorClusterHalf(t, []doctor.Result{
		{Name: "Cluster reachable", Status: doctor.StatusOK, Detail: "reachable"},
		{Name: "Proxy configuration", Status: doctor.StatusWarn, Detail: "not fully wired"},
	})
	if err != nil {
		t.Fatalf("warnings-only → want nil error (exit 0), got %v", err)
	}
	if !strings.Contains(out, "Everything looks good") {
		t.Errorf("want the healthy verdict (warns are verbose-only):\n%s", out)
	}
}

// --verbose surfaces the Kubernetes detail (kubeconfig + granular checks) under
// the plain summary — the one place that vocabulary is allowed.
func TestRunClusterDoctor_VerboseShowsDetails(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", Email: "a@b.io", ActiveClientID: "5"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`))
	})
	withClusterSeams(t, fake.NewSimpleClientset())
	withDoctorRun(t, []doctor.Result{
		{Name: "Cluster reachable", Status: doctor.StatusOK, Detail: "reachable"},
		{Name: "Service Bus egress (requests-proxy)", Status: doctor.StatusOK, Detail: "ready"},
	})
	var buf bytes.Buffer
	if err := runClusterDoctor(context.Background(), ui.New(&buf, ui.WithColor(false), ui.WithVerbose(true)), "", "", "", false); err != nil {
		t.Fatalf("verbose healthy → want nil error, got %v", err)
	}
	for _, want := range []string{"Details (for support)", "Service Bus egress (requests-proxy)"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("verbose view missing %q:\n%s", want, buf.String())
		}
	}
}

// The clientset-build arm: loadClusterFn succeeds but newClientsetFn fails. With
// the session healthy, this is a local-environment problem (exit 3) framed
// plainly (no raw Kubernetes error in the default view).
func TestRunClusterDoctor_ClientsetError(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", Email: "a@b.io", ActiveClientID: "5"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`))
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
	err := runClusterDoctor(context.Background(), ui.New(&buf, ui.WithColor(false)), "", "", "", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 3 {
		t.Fatalf("clientset build error + healthy session → want exit 3, got %v", err)
	}
	if !strings.Contains(buf.String(), "Couldn't connect to your secure environment") {
		t.Errorf("want the plain connect-failure message:\n%s", buf.String())
	}
}
