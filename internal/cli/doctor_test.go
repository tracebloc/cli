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

// signedInConfig writes a signed-in config with an active client for the tests
// that need to get past the identity gate.
func signedInConfig(t *testing.T) {
	t.Helper()
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", Email: "a@b.io", ActiveClientID: "5"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
}

// ── Identity / session gate (runs before any cluster I/O) ──

func TestDoctor_NotSignedIn(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("not signed in → want exit 2, got %v", err)
	}
	if !strings.Contains(out.String(), "Not signed in") || !strings.Contains(out.String(), "login") {
		t.Errorf("want a plain 'Not signed in — run ... login', got:\n%s", out.String())
	}
}

func TestDoctor_SessionExpired401(t *testing.T) {
	signedInConfig(t)
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Invalid token."}`))
	})
	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("401 → want exit 2, got %v", err)
	}
	if !strings.Contains(out.String(), "session expired") || !strings.Contains(out.String(), "login") {
		t.Errorf("want 'Your session expired — run ... login', got:\n%s", out.String())
	}
}

func TestDoctor_OutOfDate426(t *testing.T) {
	signedInConfig(t)
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired) // 426
		_, _ = w.Write([]byte(`{"error":"upgrade_required","min_version":"0.9.0"}`))
	})
	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("426 → want exit 2, got %v", err)
	}
	if !strings.Contains(out.String(), "out of date") || !strings.Contains(out.String(), "tracebloc.io/i.sh") {
		t.Errorf("want 'This CLI is out of date — update it: <installer>', got:\n%s", out.String())
	}
}

// A bad kubeconfig, with auth healthy, is a local-environment problem (exit 3)
// framed as "no secure environment here yet" — not a Kubernetes error dump.
func TestDoctor_KubeconfigFailIsLocalEnv(t *testing.T) {
	signedInConfig(t)
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`)) // WhoAmI ok
	})
	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "/nonexistent-kubeconfig-xyz", "", "", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 3 {
		t.Fatalf("kubeconfig-fail + auth-OK → want exit 3, got %v", err)
	}
	if !strings.Contains(out.String(), "No secure environment") {
		t.Errorf("want the plain no-environment message, got:\n%s", out.String())
	}
}

// With no --namespace/--context, doctor targets the active client's cached
// namespace (like `cluster info` + the home screen) and prints it as the
// secure-environment name. Mutation-proven: drop bindActiveClientNamespace and
// the printed name becomes the kubeconfig default.
func TestDoctor_BindsActiveClientNamespace(t *testing.T) {
	writeActiveClientConfig(t, "munich-radiology", "Munich Radiology")
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`)) // WhoAmI ok
	})
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	_ = runClusterDoctor(ctx, ui.New(&out, ui.WithColor(false)), kc, "", "", false)
	if !strings.Contains(out.String(), "munich-radiology") {
		t.Fatalf("doctor should name the active client's environment (munich-radiology), got:\n%s", out.String())
	}
}

// ── summarizeDoctor: the roll-up of the granular checks into two plain lines ──

func TestSummarizeDoctor(t *testing.T) {
	res := func(name string, s doctor.Status) doctor.Result { return doctor.Result{Name: name, Status: s} }
	allOK := []doctor.Result{
		res("Cluster reachable", doctor.StatusOK),
		res("Backend egress (from this machine)", doctor.StatusOK),
		res("Service Bus egress (requests-proxy)", doctor.StatusOK),
		res("Pod health", doctor.StatusOK),
		res("Dataset volume (PVC)", doctor.StatusOK),
		res("Node capacity", doctor.StatusOK),
	}
	with := func(base []doctor.Result, name string, s doctor.Status) []doctor.Result {
		out := make([]doctor.Result, len(base))
		copy(out, base)
		for i := range out {
			if out[i].Name == name {
				out[i].Status = s
			}
		}
		return out
	}

	t.Run("all healthy → both OK", func(t *testing.T) {
		c, r := summarizeDoctor(allOK, tokenOK)
		if c.status != doctor.StatusOK || r.status != doctor.StatusOK {
			t.Fatalf("want both OK, got connected=%v ready=%v", c.status, r.status)
		}
		if c.text != "Connected to tracebloc" || r.text != "Ready to run training" {
			t.Errorf("unexpected healthy text: %q / %q", c.text, r.text)
		}
	})

	t.Run("unreachable → connected Fail, ready can't-check", func(t *testing.T) {
		c, r := summarizeDoctor(with(allOK, "Cluster reachable", doctor.StatusFail), tokenOK)
		if c.status != doctor.StatusFail {
			t.Errorf("connected should Fail when unreachable, got %v", c.status)
		}
		if r.status != doctor.StatusUnknown {
			t.Errorf("ready should be Unknown (can't check) when unreachable, got %v", r.status)
		}
		if !strings.Contains(r.text, "can't check") {
			t.Errorf("ready text should say can't check, got %q", r.text)
		}
	})

	t.Run("token unreachable → connected Fail", func(t *testing.T) {
		c, _ := summarizeDoctor(allOK, tokenUnreachable)
		if c.status != doctor.StatusFail || !strings.Contains(c.text, "can't reach tracebloc") {
			t.Errorf("token-unreachable → want connected Fail 'can't reach tracebloc', got %v %q", c.status, c.text)
		}
	})

	t.Run("results egress down → connected Fail (experiments stall)", func(t *testing.T) {
		c, _ := summarizeDoctor(with(allOK, "Service Bus egress (requests-proxy)", doctor.StatusFail), tokenOK)
		if c.status != doctor.StatusFail || !strings.Contains(c.text, "results can't reach") {
			t.Errorf("want connected Fail on results-egress down, got %v %q", c.status, c.text)
		}
	})

	t.Run("no compute → ready Fail", func(t *testing.T) {
		_, r := summarizeDoctor(with(allOK, "Node capacity", doctor.StatusFail), tokenOK)
		if r.status != doctor.StatusFail || !strings.Contains(r.remedy, "Docker Desktop") {
			t.Errorf("want ready Fail with a raise-allocation remedy, got %v remedy=%q", r.status, r.remedy)
		}
	})

	t.Run("component down → ready Fail (reinstall/support)", func(t *testing.T) {
		_, r := summarizeDoctor(with(allOK, "Pod health", doctor.StatusFail), tokenOK)
		if r.status != doctor.StatusFail || !strings.Contains(r.remedy, "tracebloc.io/i.sh") {
			t.Errorf("want ready Fail with a reinstall remedy, got %v remedy=%q", r.status, r.remedy)
		}
	})

	// A reachable cluster with no tracebloc installed must NOT be reported as
	// "isn't answering" with a kubectl remedy — it's a reinstall (Bugbot #365).
	t.Run("no environment installed → connected Fail (installer, not kubectl)", func(t *testing.T) {
		noEnv := with(allOK, "Cluster reachable", doctor.StatusFail)
		for i := range noEnv {
			if noEnv[i].Name == "Cluster reachable" {
				noEnv[i].Reach = doctor.ReachNoEnv
			}
		}
		c, r := summarizeDoctor(noEnv, tokenOK)
		if c.status != doctor.StatusFail || !strings.Contains(c.text, "No secure environment") {
			t.Errorf("no-env → want connected Fail 'No secure environment', got %v %q", c.status, c.text)
		}
		if !strings.Contains(c.remedy, "tracebloc.io/i.sh") {
			t.Errorf("no-env remedy should be the installer, got %q", c.remedy)
		}
		if strings.Contains(c.remedy, "kubectl") {
			t.Errorf("no-env remedy must not leak kubectl, got %q", c.remedy)
		}
		if r.status != doctor.StatusUnknown {
			t.Errorf("no-env → ready should be can't-check (Unknown), got %v", r.status)
		}
	})

	// A failing image-pull check means training images can't be fetched — that
	// is not-ready, and must not be silently dropped from the rollup (Bugbot #365).
	t.Run("images can't be pulled → ready Fail", func(t *testing.T) {
		withPull := append(append([]doctor.Result{}, allOK...), res("Image pull secret", doctor.StatusFail))
		_, r := summarizeDoctor(withPull, tokenOK)
		if r.status != doctor.StatusFail || !strings.Contains(r.text, "images can't be pulled") {
			t.Errorf("image-pull down → want ready Fail 'images can't be pulled', got %v %q", r.status, r.text)
		}
		if !strings.Contains(r.remedy, "--diagnose") {
			t.Errorf("image-pull remedy should point at --diagnose, got %q", r.remedy)
		}
	})

	// A backend that ANSWERED with an error (5xx/403/decode) is a tracebloc-side
	// problem — it must not be blamed on the user's network with a proxy remedy
	// (Bugbot #365).
	t.Run("backend answered with an error → connected Fail (support, not network)", func(t *testing.T) {
		c, r := summarizeDoctor(allOK, tokenServerErr)
		if c.status != doctor.StatusFail || !strings.Contains(c.text, "server error") {
			t.Errorf("server-err → want connected Fail 'server error', got %v %q", c.status, c.text)
		}
		if strings.Contains(c.remedy, "PROXY") || strings.Contains(c.remedy, "network") {
			t.Errorf("server-err remedy must not blame the network, got %q", c.remedy)
		}
		if !strings.Contains(c.remedy, "--diagnose") {
			t.Errorf("server-err remedy should point at support/--diagnose, got %q", c.remedy)
		}
		if r.status != doctor.StatusUnknown {
			t.Errorf("server-err → ready should be can't-check (Unknown), got %v", r.status)
		}
	})

	// Disconnected but the local cluster is healthy: Ready must NOT show a green
	// ✔ next to a Connected ✖ — training can't complete while disconnected
	// (Bugbot #365).
	t.Run("disconnected but cluster healthy → ready not a false check", func(t *testing.T) {
		c, r := summarizeDoctor(with(allOK, "Service Bus egress (requests-proxy)", doctor.StatusFail), tokenOK)
		if c.status != doctor.StatusFail {
			t.Fatalf("precondition: want connected Fail (service bus down), got %v", c.status)
		}
		if r.status == doctor.StatusOK {
			t.Errorf("ready must not be a green check while disconnected, got OK %q", r.text)
		}
	})

	// The "Backend egress (from this machine)" probe is indicative-not-definitive;
	// a miss must NOT contradict a successful WhoAmI by claiming the network is
	// down. With a healthy session it stays a --verbose diagnostic (Bugbot #365).
	t.Run("backend-egress miss + healthy session → connected stays OK", func(t *testing.T) {
		c, _ := summarizeDoctor(with(allOK, "Backend egress (from this machine)", doctor.StatusFail), tokenOK)
		if c.status != doctor.StatusOK {
			t.Errorf("indicative backend-egress miss + healthy WhoAmI → want connected OK, got %v %q", c.status, c.text)
		}
		if strings.Contains(c.text, "can't reach tracebloc from here") {
			t.Errorf("must not blame the network after a successful WhoAmI, got %q", c.text)
		}
	})
}
