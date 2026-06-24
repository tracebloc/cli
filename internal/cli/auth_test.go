package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/config"
)

// withTestBackend points the login command at an httptest server (via the
// newAPIClient seam), makes polling instant (pollAfter seam), and isolates the
// on-disk config to a temp dir. All are restored on cleanup.
func withTestBackend(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())

	origClient, origAfter := newAPIClient, pollAfter
	newAPIClient = func(string) *api.Client {
		return &api.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	}
	pollAfter = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	t.Cleanup(func() { newAPIClient = origClient; pollAfter = origAfter })
}

func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd(BuildInfo{Version: "test"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestLogin_FullFlow(t *testing.T) {
	var polls int
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"WDJB-MJHT","verification_uri":"https://x/activate","expires_in":600,"interval":5}`))
		case "/device/token":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"token":"cat_abc"}`))
		case "/userinfo/":
			if got := r.Header.Get("Authorization"); got != "Bearer cat_abc" {
				t.Errorf("userinfo auth header = %q, want %q", got, "Bearer cat_abc")
			}
			_, _ = w.Write([]byte(`{"email":"ds@tracebloc.io","account":"Acme"}`))
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
		}
	})

	out, err := runCmd(t, "login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if polls != 2 {
		t.Errorf("expected 2 polls (pending then token), got %d", polls)
	}
	cfg, _ := config.Load()
	if cfg.Token != "cat_abc" {
		t.Errorf("stored token = %q, want cat_abc", cfg.Token)
	}
	if cfg.Email != "ds@tracebloc.io" {
		t.Errorf("stored email = %q, want ds@tracebloc.io", cfg.Email)
	}
	if !strings.Contains(out, "ds@tracebloc.io") {
		t.Errorf("expected output to show the account, got:\n%s", out)
	}
}

// TestLogin_SlowDownBacksOffByFive pins RFC 8628 §3.5: on `slow_down` the poll
// interval must increase by 5 seconds, not 1. Captures the durations handed to
// the pollAfter seam.
func TestLogin_SlowDownBacksOffByFive(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"X","verification_uri":"https://x/activate","expires_in":600,"interval":5}`))
		case "/device/token":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"slow_down"}`))
				return
			}
			_, _ = w.Write([]byte(`{"token":"cat_ok"}`))
		case "/userinfo/":
			_, _ = w.Write([]byte(`{"email":"e@co","account":"A"}`))
		}
	}))
	t.Cleanup(srv.Close)

	origClient, origAfter := newAPIClient, pollAfter
	newAPIClient = func(string) *api.Client { return &api.Client{BaseURL: srv.URL, HTTP: srv.Client()} }
	var waits []time.Duration
	pollAfter = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	t.Cleanup(func() { newAPIClient = origClient; pollAfter = origAfter })

	if _, err := runCmd(t, "login"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if len(waits) < 2 {
		t.Fatalf("expected >=2 polls, got waits=%v", waits)
	}
	if waits[0] != 5*time.Second {
		t.Errorf("first poll wait = %v, want 5s (server interval)", waits[0])
	}
	if waits[1] != 10*time.Second {
		t.Errorf("post-slow_down wait = %v, want 10s (interval+5 per RFC 8628), not 6s", waits[1])
	}
}

func TestLogin_BackendUnsupported(t *testing.T) {
	withTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := runCmd(t, "login")
	if err == nil || !strings.Contains(err.Error(), "doesn't support browser login") {
		t.Errorf("want unsupported-backend error, got %v", err)
	}
	cfg, _ := config.Load()
	if cfg.SignedIn() {
		t.Error("must not store a token when the backend has no device endpoints")
	}
}

func TestLogin_Denied(t *testing.T) {
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"X","interval":5}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"access_denied"}`))
		}
	})
	_, err := runCmd(t, "login")
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("want access-denied error, got %v", err)
	}
}

func TestLogout(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{Token: "x", Email: "e@co", ActiveClientID: "7"}).Save(); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, "logout")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if cfg.SignedIn() {
		t.Error("expected to be signed out")
	}
	// The active-client pointer is account-scoped — it must not survive logout,
	// or it bleeds into the next account's session.
	if cfg.ActiveClientID != "" {
		t.Errorf("active_client_id = %q after logout, want cleared", cfg.ActiveClientID)
	}
	if !strings.Contains(out, "Signed out") {
		t.Errorf("got:\n%s", out)
	}
}

func TestAuthStatus_SignedIn(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{Env: "dev", Token: "x", Email: "ds@co"}).Save(); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, "auth", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"signed in", "ds@co", "dev"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q, got:\n%s", want, out)
		}
	}
}

func TestAuthStatus_NotSignedIn(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	out, err := runCmd(t, "auth", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Not signed in") {
		t.Errorf("got:\n%s", out)
	}
}
