package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/doctor"
	"github.com/tracebloc/cli/internal/ui"
)

// stubBackend points the newAPIClient seam at an httptest server for one test.
func stubBackend(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	orig := newAPIClient
	newAPIClient = func(string) *api.Client { return &api.Client{BaseURL: srv.URL, HTTP: srv.Client()} }
	t.Cleanup(func() { newAPIClient = orig })
}

// cli#101: `cluster doctor` auth/config/token checks (RFC-0001 §8.5). These pin
// runAuthChecks — the half of doctor that diagnoses a failed *provision*.

func TestRunAuthChecks_NotSignedIn(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	var out bytes.Buffer
	if st := runAuthChecks(context.Background(), ui.New(&out)); st != doctor.StatusFail {
		t.Errorf("not signed in → want Fail, got %v", st)
	}
	if !strings.Contains(out.String(), "Auth & config") || !strings.Contains(out.String(), "not signed in") {
		t.Errorf("missing auth section / not-signed-in line:\n%s", out.String())
	}
}

func TestRunAuthChecks_TokenValid(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", Email: "a@b.io", ActiveClientID: "5"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`))
	})
	var out bytes.Buffer
	if st := runAuthChecks(context.Background(), ui.New(&out)); st != doctor.StatusOK {
		t.Errorf("valid token + active client → want OK, got %v;\n%s", st, out.String())
	}
	if !strings.Contains(out.String(), "token valid") {
		t.Errorf("missing token-valid line:\n%s", out.String())
	}
}

func TestRunAuthChecks_TokenRejected401(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", ActiveClientID: "5"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Invalid token."}`))
	})
	var out bytes.Buffer
	if st := runAuthChecks(context.Background(), ui.New(&out)); st != doctor.StatusFail {
		t.Errorf("token rejected (401) → want Fail, got %v", st)
	}
	if !strings.Contains(out.String(), "rejected the token (401)") {
		t.Errorf("missing 401 line:\n%s", out.String())
	}
}

func TestRunAuthChecks_NoActiveClientWarns(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x"}, // signed in, but no active client selected
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`))
	})
	var out bytes.Buffer
	if st := runAuthChecks(context.Background(), ui.New(&out)); st != doctor.StatusWarn {
		t.Errorf("valid token but no active client → want Warn, got %v;\n%s", st, out.String())
	}
	if !strings.Contains(out.String(), "Active client — none") {
		t.Errorf("missing no-active-client warning:\n%s", out.String())
	}
}

// TestClusterDoctor_KubeconfigFailEscalatesWhenAuthFails pins the Bugbot fix: a
// kubeconfig load failure normally exits 3, but if the auth section ALSO failed
// (here: not signed in) it escalates to 2 so a bad token isn't masked as a
// kubeconfig-only problem.
func TestClusterDoctor_KubeconfigFailEscalatesWhenAuthFails(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir()) // not signed in → auth Fail
	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out), "/nonexistent-kubeconfig-xyz", "", "")
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("kubeconfig-fail + auth-fail → want exit 2, got %v", err)
	}
}

// TestClusterDoctor_KubeconfigFailStays3WhenAuthOK: with auth healthy, a
// kubeconfig failure keeps the documented exit-3 contract.
func TestClusterDoctor_KubeconfigFailStays3WhenAuthOK(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", ActiveClientID: "5"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`)) // WhoAmI ok → auth OK
	})
	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out), "/nonexistent-kubeconfig-xyz", "", "")
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 3 {
		t.Fatalf("kubeconfig-fail + auth-OK → want exit 3 (contract), got %v", err)
	}
}

// TestRunAuthChecks_426IsHardFailure pins the Bugbot fix: a 426 (server enforces
// a newer CLI) from the live token check is a hard "upgrade" failure, not a
// transient "couldn't verify — check your network" warning.
func TestRunAuthChecks_426IsHardFailure(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", ActiveClientID: "5"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired) // 426
		_, _ = w.Write([]byte(`{"error":"upgrade_required","min_version":"0.9.0"}`))
	})
	var out bytes.Buffer
	if st := runAuthChecks(context.Background(), ui.New(&out)); st != doctor.StatusFail {
		t.Errorf("426 from the token check → want Fail (not a transient Warn), got %v", st)
	}
	if !strings.Contains(out.String(), "too old") || !strings.Contains(out.String(), "426") {
		t.Errorf("426 should report a clear 'too old / upgrade' failure, got:\n%s", out.String())
	}
}

// TestClusterDoctor_BindsActiveClientNamespace pins the review fix: with no
// --namespace/--context, doctor must target the active client's cached namespace
// (like `cluster info` and the home screen), not the kubeconfig default — else
// the home screen and doctor can disagree about the same install. The server is
// unroutable, so discovery falls back to the bound namespace; we assert doctor
// reports THAT namespace. Mutation-proven: drop bindActiveClientNamespace and the
// namespace becomes the kubeconfig default, failing this assertion.
func TestClusterDoctor_BindsActiveClientNamespace(t *testing.T) {
	writeActiveClientConfig(t, "munich-radiology", "Munich Radiology")
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`)) // WhoAmI ok → auth healthy
	})
	// Valid kubeconfig at an unroutable TEST-NET address: loads fine, so the
	// namespace resolves from the binding; the later cluster dial just fails.
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://192.0.2.1:1"}
contexts:
- name: ctx
  context: {cluster: c, user: u}
current-context: ctx
users:
- name: u
  user: {}
`
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kc, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	// Bound the context so doctor.Run's dial to the unroutable server can't hang
	// the test (and CI) — we only assert the namespace resolved from the binding,
	// which is printed before any cluster I/O.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	_ = runClusterDoctor(ctx, ui.New(&out), kc, "", "") // no ns/context override
	if !strings.Contains(out.String(), "munich-radiology") {
		t.Fatalf("doctor should target the active client's namespace (munich-radiology), got:\n%s", out.String())
	}
}
