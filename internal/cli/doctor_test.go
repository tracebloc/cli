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
	"github.com/tracebloc/cli/internal/cluster"
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

// A 401 hard stop with --diagnose must NOT write a bundle: the fix is `login`
// (no bundle needed), and one written here would falsely record "session:
// confirmed" for an expired session. The defer is registered after the session
// probe precisely so 401/426 return first (Bugbot #365).
func TestDoctor_DiagnoseNotWrittenOnExpiredSession(t *testing.T) {
	signedInConfig(t)
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Invalid token."}`))
	})
	tmp := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	var out bytes.Buffer
	err = runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", true)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("401 + --diagnose → want exit 2, got %v", err)
	}
	if strings.Contains(out.String(), "Wrote a support bundle") {
		t.Errorf("must not write a bundle on an expired session, got:\n%s", out.String())
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tracebloc-doctor-") {
			t.Errorf("a bundle file was written on 401 (%s) — should be none", e.Name())
		}
	}
}

// A session fault (here a transport error → tokenUnreachable) that coincides with
// a local-env failure must surface as exit 2 ("a problem was found"), matching the
// full-probe path — not be masked as the local-env exit 3 (Bugbot #365).
func TestDoctor_SessionFaultDominatesEarlyExit(t *testing.T) {
	signedInConfig(t)
	// Point the API client at a closed port so WhoAmI is a transport error →
	// tokenUnreachable (not an APIError).
	origAPI := newAPIClient
	t.Cleanup(func() { newAPIClient = origAPI })
	newAPIClient = func(string) *api.Client {
		return &api.Client{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: 2 * time.Second}}
	}
	// No environment on this machine.
	origLoad := loadClusterFn
	t.Cleanup(func() { loadClusterFn = origLoad })
	loadClusterFn = func(cluster.KubeconfigOptions) (*cluster.ResolvedConfig, error) {
		return nil, errors.New("no kubeconfig here")
	}

	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("session fault + no-env → want exit 2 (problem found), got %v", err)
	}
	if !strings.Contains(out.String(), "No secure environment") {
		t.Errorf("want the no-environment line, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Can't reach tracebloc from here") &&
		!strings.Contains(out.String(), "didn't confirm your session") {
		t.Errorf("want the session fault surfaced alongside no-environment, got:\n%s", out.String())
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
	withDetail := func(base []doctor.Result, name string, s doctor.Status, detail string) []doctor.Result {
		out := with(base, name, s)
		for i := range out {
			if out[i].Name == name {
				out[i].Detail = detail
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

	t.Run("node capacity can't-check → ready can't-check, never green", func(t *testing.T) {
		// Both can't-check Warn details (nodes unlistable, RESOURCE_REQUESTS
		// unreadable) must roll up to an honest Unknown — a ✔ here would assert
		// readiness without a capacity probe (Bugbot).
		for _, detail := range []string{
			"could not list nodes: nodes is forbidden",
			"couldn't read RESOURCE_REQUESTS from jobs-manager — skipping node-fit",
		} {
			_, r := summarizeDoctor(withDetail(allOK, "Node capacity", doctor.StatusWarn, detail), tokenOK)
			if r.status != doctor.StatusUnknown {
				t.Errorf("%q: ready should be Unknown, got %v", detail, r.status)
			}
			if !strings.Contains(r.text, "couldn't check free compute") {
				t.Errorf("%q: ready text should say couldn't check free compute, got %q", detail, r.text)
			}
		}
	})

	t.Run("node capacity GPU-soft warn → still ready", func(t *testing.T) {
		_, r := summarizeDoctor(withDetail(allOK, "Node capacity", doctor.StatusWarn,
			"no single Ready node satisfies cpu+memory AND nvidia.com/gpu — GPU jobs rely on the CPU fallback (needs cpu=2, memory=8Gi)"), tokenOK)
		if r.status != doctor.StatusOK {
			t.Errorf("GPU-soft warn must stay Ready (CPU fallback), got %v", r.status)
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
		// GOOS-independent invariant: every variant carries the resize fix
		// (#400 — the old string pinned "Docker Desktop", wrong on WSL2/linux).
		if r.status != doctor.StatusFail || !strings.Contains(r.remedy, "resources set max") {
			t.Errorf("want ready Fail with a resize remedy, got %v remedy=%q", r.status, r.remedy)
		}
	})

	// computeRemedy (#400): the compute remedy must match the host's actual
	// memory lever — Docker Desktop's Resources slider does not exist on the
	// WSL2 backend, and bare Linux has no Docker Desktop at all.
	t.Run("computeRemedy per GOOS", func(t *testing.T) {
		win := computeRemedy("windows")
		if !strings.Contains(win, ".wslconfig") || !strings.Contains(win, "Hyper-V") {
			t.Fatalf("windows remedy must name both levers: %q", win)
		}
		mac := computeRemedy("darwin")
		if !strings.Contains(mac, "Docker Desktop → Settings → Resources → Advanced") {
			t.Fatalf("darwin remedy keeps the slider: %q", mac)
		}
		lin := computeRemedy("linux")
		if strings.Contains(lin, "Docker Desktop") || strings.Contains(lin, "wslconfig") {
			t.Fatalf("linux remedy must not name Docker Desktop/WSL: %q", lin)
		}
		for _, r := range []string{win, mac, lin} {
			if !strings.Contains(r, "resources set max") {
				t.Fatalf("every remedy carries the resize fix: %q", r)
			}
		}
	})

	t.Run("component down → ready Fail (reinstall/support)", func(t *testing.T) {
		_, r := summarizeDoctor(with(allOK, "Pod health", doctor.StatusFail), tokenOK)
		if r.status != doctor.StatusFail || !strings.Contains(r.remedy, "tracebloc.io/i.sh") {
			t.Errorf("want ready Fail with a reinstall remedy, got %v remedy=%q", r.status, r.remedy)
		}
	})

	// Pod health has TWO StatusWarn sources (checkPods): pods stuck Pending, and a
	// failure to list pods at all (e.g. RBAC). They must roll up differently.
	warnPods := func(detail string) []doctor.Result {
		out := with(allOK, "Pod health", doctor.StatusWarn)
		for i := range out {
			if out[i].Name == "Pod health" {
				out[i].Detail = detail
			}
		}
		return out
	}

	// Pods stuck Pending past the grace window: training can't schedule, so the
	// rollup must NOT report ✔ "Ready to run training" — that false green was the
	// original Bugbot finding.
	t.Run("pods stuck pending (warn) → ready Fail, not a false green", func(t *testing.T) {
		_, r := summarizeDoctor(warnPods("Pending > 5m0s: [trainer-x]"), tokenOK)
		if r.status != doctor.StatusFail {
			t.Fatalf("stuck-pending pods must roll up to not-ready, got %v %q", r.status, r.text)
		}
		if !strings.Contains(r.text, "Not ready") {
			t.Errorf("want a Not-ready readiness line for stuck-pending pods, got %q", r.text)
		}
	})

	// The other Pod-health Warn source, "could not list pods" (RBAC), is a
	// can't-check — it must NOT get the stuck-pending/compute (Docker Desktop)
	// remedy (Bugbot follow-up).
	t.Run("pod-health warn = could not list pods (RBAC) → can't-check, not stuck-pending", func(t *testing.T) {
		_, r := summarizeDoctor(warnPods("could not list pods: pods is forbidden"), tokenOK)
		if r.status == doctor.StatusFail {
			t.Errorf("a can't-list-pods warn must not be a hard not-ready, got %v %q", r.status, r.text)
		}
		if strings.Contains(r.remedy, "Docker Desktop") {
			t.Errorf("must not give the stuck-pending/compute remedy for a read failure, got remedy=%q", r.remedy)
		}
	})

	// A reachable cluster with no tracebloc installed must NOT be reported as
	// "isn't answering" with a kubectl remedy — it's a reinstall (Bugbot #365).
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

// ── doctorVerdict: the closing "everything looks good" / problem / partial call ──

func TestDoctorVerdict(t *testing.T) {
	ok, warn, fail, unknown := doctor.StatusOK, doctor.StatusWarn, doctor.StatusFail, doctor.StatusUnknown
	cases := []struct {
		name             string
		connected, ready doctor.Status
		wantFail         bool
		wantAllGood      bool
	}{
		{"both OK → everything good", ok, ok, false, true},
		{"ready Fail → problem", ok, fail, true, false},
		{"connected Fail → problem", fail, ok, true, false},
		// The Bugbot case: connected but readiness couldn't be checked (RBAC →
		// Unknown). Not a hard failure, but NOT "everything looks good".
		{"connected + ready can't-check → neither", ok, unknown, false, false},
		// Not-connected already Fails via connected, regardless of ready=Unknown.
		{"disconnected + ready unknown → problem", fail, unknown, true, false},
		{"a warn that isn't Fail → not everything-good", ok, warn, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotFail, gotAllGood := doctorVerdict(tc.connected, tc.ready)
			if gotFail != tc.wantFail || gotAllGood != tc.wantAllGood {
				t.Errorf("doctorVerdict(%v,%v) = fail=%v allGood=%v, want fail=%v allGood=%v",
					tc.connected, tc.ready, gotFail, gotAllGood, tc.wantFail, tc.wantAllGood)
			}
		})
	}
}
