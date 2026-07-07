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
	if cfg.Current().Token != "cat_abc" {
		t.Errorf("stored token = %q, want cat_abc", cfg.Current().Token)
	}
	if cfg.Current().Email != "ds@tracebloc.io" {
		t.Errorf("stored email = %q, want ds@tracebloc.io", cfg.Current().Email)
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
	// logout now revokes server-side (cli#112) — route it at a stub, not prod.
	var revoked bool
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/revoke" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return
		}
		revoked = true
		if got := r.Header.Get("Authorization"); got != "Bearer x" {
			t.Errorf("revoke auth header = %q, want %q", got, "Bearer x")
		}
		w.WriteHeader(http.StatusNoContent) // 204, like the real endpoint
	})
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", Email: "e@co", ActiveClientID: "7"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, "logout")
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Error("logout did not call POST /auth/revoke")
	}
	cfg, _ := config.Load()
	if cfg.SignedIn() {
		t.Error("expected to be signed out")
	}
	// The active-client pointer is account-scoped — it must not survive logout,
	// or it bleeds into the next account's session.
	if cfg.Current().ActiveClientID != "" {
		t.Errorf("active_client_id = %q after logout, want cleared", cfg.Current().ActiveClientID)
	}
	if !strings.Contains(out, "Signed out") {
		t.Errorf("got:\n%s", out)
	}
}

// TestLogout_RevokeFailureStillClearsLocal pins the cli#112 contract: when the
// server-side revoke fails (offline / already-revoked / 5xx), logout must still
// succeed and clear local state — never leave the user unable to log out locally.
func TestLogout_RevokeFailureStillClearsLocal(t *testing.T) {
	withTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // revoke fails
	})
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", Email: "e@co", ActiveClientID: "7"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, "logout")
	if err != nil {
		t.Fatalf("logout must succeed even when revoke fails: %v", err)
	}
	cfg, _ := config.Load()
	if cfg.SignedIn() || cfg.Current().ActiveClientID != "" {
		t.Errorf("local state must be cleared even when revoke fails: %+v", cfg)
	}
	if !strings.Contains(out, "Signed out") {
		t.Errorf("got:\n%s", out)
	}
}

// TestLogout_RevokesAgainstSessionEnv pins that logout revokes against the
// session's own env (the current profile's env), not a hardcoded prod — so the
// token is killed on the host it was issued for (cli#112 / Bugbot, carried to v2).
func TestLogout_RevokesAgainstSessionEnv(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "stg", Profiles: map[string]*config.Profile{
		"stg": {Token: "x"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	var gotEnv string
	orig := newAPIClient
	newAPIClient = func(env string) *api.Client {
		gotEnv = env
		return &api.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	}
	t.Cleanup(func() { newAPIClient = orig })

	if _, err := runCmd(t, "logout"); err != nil {
		t.Fatal(err)
	}
	if gotEnv != "stg" {
		t.Errorf("logout revoked against env %q, want the session env %q (not prod)", gotEnv, "stg")
	}
}

func TestAuthStatus_SignedIn(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", Email: "ds@co"},
	}}).Save(); err != nil {
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

// saveSignedIn writes a signed-in profile for the "dev" env into the isolated
// config dir set up by a prior withTestBackend/t.Setenv.
func saveSignedIn(t *testing.T, token string) {
	t.Helper()
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: token, Email: "ds@co"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
}

// TestAuthCheck_SignedInValid_Exit0: `auth status --check` exits 0 and is silent
// when a token is present and the backend accepts it.
func TestAuthCheck_SignedInValid_Exit0(t *testing.T) {
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo/" {
			_, _ = w.Write([]byte(`{"email":"ds@co","account":"Acme"}`))
			return
		}
		t.Errorf("unexpected request path %s", r.URL.Path)
	})
	saveSignedIn(t, "tok") // CurrentEnv=dev
	out, err := runCmd(t, "auth", "status", "--check", "--env", "dev")
	if err != nil {
		t.Fatalf("--check should exit 0 when signed in + token valid, got: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("--check should be silent by default, got:\n%s", out)
	}
}

// TestAuthCheck_NotSignedIn_Exit1: silent exit 1 when there's no token.
func TestAuthCheck_NotSignedIn_Exit1(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	out, err := runCmd(t, "auth", "status", "--check")
	if got := ExitCodeFromError(err); got != 1 {
		t.Fatalf("exit code = %d, want 1 (err=%v)", got, err)
	}
	if !IsSilentError(err) {
		t.Errorf("--check exit 1 should be silent (nil-inner exitError), err=%v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("--check should print nothing, got:\n%s", out)
	}
}

// TestAuthCheck_TokenRejected_Exit1: a stored token the backend rejects (401)
// exits 1 — not a false "signed in".
func TestAuthCheck_TokenRejected_Exit1(t *testing.T) {
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo/" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	})
	saveSignedIn(t, "stale") // CurrentEnv=dev
	_, err := runCmd(t, "auth", "status", "--check", "--env", "dev")
	if got := ExitCodeFromError(err); got != 1 {
		t.Fatalf("exit code = %d, want 1 on a rejected token", got)
	}
}

// TestAuthCheck_VerboseNarrates: --check --verbose prints the verdict.
func TestAuthCheck_VerboseNarrates(t *testing.T) {
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo/" {
			_, _ = w.Write([]byte(`{"email":"ds@co","account":"Acme"}`))
		}
	})
	saveSignedIn(t, "tok") // CurrentEnv=dev
	out, err := runCmd(t, "auth", "status", "--check", "--verbose", "--env", "dev")
	if err != nil {
		t.Fatalf("--check --verbose signed-in should exit 0, got: %v", err)
	}
	if !strings.Contains(out, "ds@co") {
		t.Errorf("--verbose should narrate the account, got:\n%s", out)
	}
}

// TestAuthCheck_UpgradeRequiredSurfaces (Bugbot #146-C): a 426 from WhoAmI must
// surface the upgrade instruction (even without --verbose), not be reported like
// a rejected token telling the user to re-login.
func TestAuthCheck_UpgradeRequiredSurfaces(t *testing.T) {
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo/" {
			w.WriteHeader(http.StatusUpgradeRequired) // 426
			_, _ = w.Write([]byte(`{"error":"upgrade_required","min_version":"1.2.3"}`))
		}
	})
	saveSignedIn(t, "tok")                                           // CurrentEnv=dev
	_, err := runCmd(t, "auth", "status", "--check", "--env", "dev") // no --verbose
	if got := ExitCodeFromError(err); got != 1 {
		t.Fatalf("exit code = %d, want 1", got)
	}
	if IsSilentError(err) || err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("a 426 must surface the upgrade message (non-silent), got: %v", err)
	}
}

// TestAuthCheck_EnvMismatch_Exit1 (Lukas #1): a valid session for one env must NOT
// pass --check for a DIFFERENT target env — otherwise the installer skips the
// login that switches env and provisions into the wrong account. Exit 1 without
// even probing the backend (no /userinfo/ call).
func TestAuthCheck_EnvMismatch_Exit1(t *testing.T) {
	probed := false
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo/" {
			probed = true
			_, _ = w.Write([]byte(`{"email":"ds@co","account":"Acme"}`))
		}
	})
	saveSignedIn(t, "tok") // CurrentEnv=dev, valid dev session
	// The installer targets prod this run; the dev session must not satisfy it.
	_, err := runCmd(t, "auth", "status", "--check", "--env", "prod")
	if got := ExitCodeFromError(err); got != 1 {
		t.Fatalf("exit code = %d, want 1 on an env mismatch", got)
	}
	if probed {
		t.Error("must not probe the backend when the signed-in env differs from the target")
	}
}
