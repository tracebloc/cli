package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
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

// ── cli#517: which poll failures end the sign-in, and which are worth another try ──

// deviceCodeBody is the /device/code reply the poll tests share: a ten-minute
// window (so the expiry copy has a duration to name) and the RFC's 5s interval.
const deviceCodeBody = `{"device_code":"dc","user_code":"X","verification_uri":"https://x/activate","expires_in":600,"interval":5}`

// pollBackend serves /device/code + /userinfo/ and hands every /device/token
// poll to tokenH, which sees the 1-based poll number. Returns a pointer to the
// live poll count so a test can assert the loop STOPPED (or kept going).
func pollBackend(t *testing.T, tokenH func(w http.ResponseWriter, poll int)) *int {
	t.Helper()
	polls := 0
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			_, _ = w.Write([]byte(deviceCodeBody))
		case "/device/token":
			polls++
			tokenH(w, polls)
		case "/userinfo/":
			_, _ = w.Write([]byte(`{"email":"ds@tracebloc.io"}`))
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
		}
	})
	return &polls
}

// TestClassifyPollError_Table pins the retry classification directly, on the
// production function the loop calls (never a re-implementation of it). The
// inputs are written down independently of the matcher: each is the error a
// specific real-world failure produces, not a value read back off the rule.
func TestClassifyPollError_Table(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want pollDisposition
	}{
		// The RFC 8628 §3.5 sentinels, bare and wrapped — PollToken returns them
		// bare today, but a future wrapper must not silently reclassify them.
		{"pending", api.ErrAuthorizationPending, pollAgain},
		{"pending wrapped", fmt.Errorf("poll: %w", api.ErrAuthorizationPending), pollAgain},
		{"slow_down", api.ErrSlowDown, pollSlower},
		{"slow_down wrapped", fmt.Errorf("poll: %w", api.ErrSlowDown), pollSlower},
		{"expired_token", api.ErrExpiredToken, pollStop},
		{"expired_token wrapped", fmt.Errorf("poll: %w", api.ErrExpiredToken), pollStop},
		{"access_denied", api.ErrAccessDenied, pollStop},
		{"access_denied wrapped", fmt.Errorf("poll: %w", api.ErrAccessDenied), pollStop},

		// A server verdict we must respect: retrying an identical request cannot
		// change any of these, and looping on one would hang the installer.
		{"400 unrecognized", &api.APIError{StatusCode: 400, Body: `{"error":"invalid_grant"}`}, pollStop},
		{"401", &api.APIError{StatusCode: 401}, pollStop},
		{"403", &api.APIError{StatusCode: 403}, pollStop},
		{"404 no such endpoint", &api.APIError{StatusCode: 404}, pollStop},
		{"426 upgrade required", &api.UpgradeRequiredError{MinVersion: "1.2.3"}, pollStop},
		{"426 wrapped", fmt.Errorf("poll: %w", &api.UpgradeRequiredError{}), pollStop},
		{"operator cancelled", context.Canceled, pollStop},
		{"operator cancelled wrapped", fmt.Errorf("POST /device/token: %w", context.Canceled), pollStop},

		// Temporary: the server (or a proxy in front of it) is failing, and the
		// human at the browser has done nothing wrong.
		{"500", &api.APIError{StatusCode: 500}, pollRetry},
		{"502 proxy", &api.APIError{StatusCode: 502}, pollRetry},
		{"503 deploying", &api.APIError{StatusCode: 503}, pollRetry},
		{"504 gateway timeout", &api.APIError{StatusCode: 504}, pollRetry},
		{"408 request timeout", &api.APIError{StatusCode: 408}, pollRetry},
		{"429 rate limited", &api.APIError{StatusCode: 429}, pollRetry},

		// Never reached a verdict at all — the class cli#517 exists to keep alive.
		{"dns failure", fmt.Errorf("POST /device/token: %w", &net.OpError{
			Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "api.tracebloc.io"}}), pollRetry},
		{"connection refused", fmt.Errorf("POST /device/token: %w",
			&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}), pollRetry},
		{"http client timeout", fmt.Errorf("POST /device/token: %w", context.DeadlineExceeded), pollRetry},
		{"undecodable body", errors.New(`device-token success response missing token (got "<html>")`), pollRetry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPollError(tc.err); got != tc.want {
				t.Errorf("classifyPollError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyPollError_CoversPollTokenVocabulary derives the input domain from
// the PRODUCER instead of restating it: every error code RFC 8628 §3.5 lets the
// device-token endpoint return is driven through the real api.PollToken, and the
// error it actually produces is classified. A code the client stops mapping (or
// starts mapping differently) shows up here as a changed disposition — which a
// hand-written list of sentinels could not see.
func TestClassifyPollError_CoversPollTokenVocabulary(t *testing.T) {
	want := map[string]pollDisposition{
		"authorization_pending": pollAgain,
		"slow_down":             pollSlower,
		"expired_token":         pollStop,
		"access_denied":         pollStop,
		// Not in §3.5's happy vocabulary, but §3.5 defers to RFC 6749 §5.2 for
		// the rest; those are refusals of the request, not transient conditions.
		"invalid_request": pollStop,
		"invalid_grant":   pollStop,
	}
	for code, wantDisp := range want {
		t.Run(code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
			}))
			t.Cleanup(srv.Close)
			c := &api.Client{BaseURL: srv.URL, HTTP: srv.Client()}
			_, err := c.PollToken(context.Background(), "dc")
			if err == nil {
				t.Fatalf("PollToken returned no error for %q", code)
			}
			if got := classifyPollError(err); got != wantDisp {
				t.Errorf("%q → %v, want %v (err=%v)", code, got, wantDisp, err)
			}
		})
	}
}

// TestLogin_TransientFailureRetriesWithinWindow is the headline cli#517 fix: a
// backend blip mid-poll used to abort the whole sign-in (and with it an
// installer run that had already built a cluster). It must be ridden out.
func TestLogin_TransientFailureRetriesWithinWindow(t *testing.T) {
	polls := pollBackend(t, func(w http.ResponseWriter, poll int) {
		switch {
		case poll <= 3: // three 503s in a row — a backend restart
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_, _ = w.Write([]byte(`{"token":"cat_ok"}`))
		}
	})
	if _, err := runCmd(t, "login"); err != nil {
		t.Fatalf("a transient backend failure must not abort sign-in, got: %v", err)
	}
	if *polls != 4 {
		t.Errorf("polls = %d, want 4 (three 503s ridden out, then the token)", *polls)
	}
	cfg, _ := config.Load()
	if cfg.Current().Token != "cat_ok" {
		t.Errorf("stored token = %q, want cat_ok", cfg.Current().Token)
	}
}

// TestLogin_TerminalErrorStopsImmediately is the other half of the same
// contract: making transient errors retryable must NOT turn a refusal into a
// loop. A 400 the client can't map to a §3.5 sentinel is a refusal — one poll,
// then out.
func TestLogin_TerminalErrorStopsImmediately(t *testing.T) {
	polls := pollBackend(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	_, err := runCmd(t, "login")
	if err == nil {
		t.Fatal("a refused device code must fail the sign-in")
	}
	if *polls != 1 {
		t.Errorf("polls = %d, want 1 — a refusal must not be retried", *polls)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("the refusal must be surfaced verbatim, got: %v", err)
	}
}

// TestLogin_AccessDeniedStopsImmediately: the user said no in the browser.
// Polling past that would ignore an explicit refusal.
func TestLogin_AccessDeniedStopsImmediately(t *testing.T) {
	polls := pollBackend(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	})
	if _, err := runCmd(t, "login"); err == nil {
		t.Fatal("a denied sign-in must fail")
	}
	if *polls != 1 {
		t.Errorf("polls = %d, want 1 — an explicit denial must not be retried", *polls)
	}
}

// TestLogin_TransientFailuresGiveUpAtTheCap: retrying is bounded. A backend
// that never answers must report ITSELF as the cause, not burn the code's whole
// window and then blame the user for being slow.
func TestLogin_TransientFailuresGiveUpAtTheCap(t *testing.T) {
	polls := pollBackend(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err := runCmd(t, "login")
	if err == nil {
		t.Fatal("an unreachable backend must eventually fail the sign-in")
	}
	// Written down independently of the constant: a cap of 1 would satisfy the
	// equality below while restoring exactly the cli#517 behaviour this fixes —
	// one failure, no retry. The loop must ride out at least a few.
	if *polls < 5 {
		t.Errorf("polls = %d — a transient failure must be retried several times before giving up", *polls)
	}
	if *polls != maxPollFailures {
		t.Errorf("polls = %d, want %d (the consecutive-failure cap)", *polls, maxPollFailures)
	}
	if !strings.Contains(err.Error(), "couldn't reach the backend") {
		t.Errorf("the give-up message must name the network as the cause, got: %v", err)
	}
}

// TestLogin_TransientFailureStreakResets: the cap counts CONSECUTIVE failures.
// A blip every few polls during a long human-paced wait must not accumulate
// into a give-up — that would re-introduce cli#517 on a flaky link.
func TestLogin_TransientFailureStreakResets(t *testing.T) {
	polls := pollBackend(t, func(w http.ResponseWriter, poll int) {
		switch {
		case poll >= 3*maxPollFailures:
			_, _ = w.Write([]byte(`{"token":"cat_ok"}`))
		case poll%2 == 0: // every other poll fails — never maxPollFailures in a row
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
		}
	})
	if _, err := runCmd(t, "login"); err != nil {
		t.Fatalf("an intermittent failure must not exhaust the cap, got: %v", err)
	}
	if *polls != 3*maxPollFailures {
		t.Errorf("polls = %d, want %d", *polls, 3*maxPollFailures)
	}
}

// TestLogin_ExpiredNamesTheWindow (cli#517 §4): "the sign-in code expired" with
// no duration reads as an instant failure to a user who stepped away. The
// message must name the window the server advertised.
func TestLogin_ExpiredNamesTheWindow(t *testing.T) {
	pollBackend(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"expired_token"}`))
	})
	_, err := runCmd(t, "login")
	if err == nil {
		t.Fatal("an expired code must fail the sign-in")
	}
	if !strings.Contains(err.Error(), "10 minutes") {
		t.Errorf("the expiry message must name the 600s window as 10 minutes, got: %v", err)
	}
}

// TestSignInWindow renders expires_in as the clause the copy carries. Derived
// from the server's value, never hardcoded, so it can't outlive a TTL change.
func TestSignInWindow(t *testing.T) {
	cases := []struct {
		expiresIn int
		want      string
	}{
		{600, " — sign-in codes are valid for 10 minutes"},
		{60, " — sign-in codes are valid for 1 minute"},
		{300, " — sign-in codes are valid for 5 minutes"},
		{90, " — sign-in codes are valid for 1m30s"},
		{0, ""},  // server didn't say — say nothing rather than guess
		{-1, ""}, // ditto
	}
	for _, tc := range cases {
		if got := signInWindow(tc.expiresIn); got != tc.want {
			t.Errorf("signInWindow(%d) = %q, want %q", tc.expiresIn, got, tc.want)
		}
	}
}

// TestCopyCatalogSeesTheSignInStrings guards the guard: the copy catalog
// harvests string LITERALS passed to errors.New / fmt.Errorf / the Printer, so
// composing a message inside a helper (and handing the helper a variable) drops
// it silently out of the catalog — the completeness backstop goes on passing
// while the copy it exists to inventory is invisible. These sentences must stay
// literal arguments; assembling them here proves they still are.
func TestCopyCatalogSeesTheSignInStrings(t *testing.T) {
	catalog := strings.Join(harvestMessages(t), "\n")
	for _, want := range []string{
		"the sign-in code expired%s",
		"the sign-in code expired before it was approved%s",
		"sign-in was denied in the browser",
		"%w. Run `tracebloc login` to start a new one",
		"— sign-in codes are valid for %s", // the harvest trims leading space
		"couldn't reach the backend to finish signing in",
	} {
		if !strings.Contains(catalog, want) {
			t.Errorf("copy %q is not reachable by the catalog harvest — keep it a literal argument "+
				"of errors.New/fmt.Errorf, not a variable built inside a helper", want)
		}
	}
}

// TestSignInAdvice_ContradictsNobody (cli#517 §3): standalone, the CLI names the
// command that starts a fresh sign-in. Under the installer it must NOT — a bare
// `tracebloc login` there leaves the client mint and the Helm install undone,
// and the installer prints its own, correct next step a line later.
func TestSignInAdvice_ContradictsNobody(t *testing.T) {
	t.Run("standalone names the command", func(t *testing.T) {
		t.Setenv("TRACEBLOC_INSTALLER", "")
		err := terminalSignInError(api.ErrExpiredToken, " — sign-in codes are valid for 10 minutes")
		if !strings.Contains(err.Error(), "tracebloc login") {
			t.Errorf("a hand-run login should say how to retry, got: %v", err)
		}
	})
	t.Run("under the installer names nothing", func(t *testing.T) {
		t.Setenv("TRACEBLOC_INSTALLER", "1")
		err := terminalSignInError(api.ErrExpiredToken, " — sign-in codes are valid for 10 minutes")
		if strings.Contains(err.Error(), "tracebloc login") {
			t.Errorf("under the installer `tracebloc login` is wrong advice, got: %v", err)
		}
		if !strings.Contains(err.Error(), "expired") || !strings.Contains(err.Error(), "10 minutes") {
			t.Errorf("suppressing the advice must not suppress the FACT, got: %v", err)
		}
	})
}

// TestLogin_InstallerContextSuppressesTheCliAdvice pins the same rule through
// the command, not just the helper — the env var has to reach the message a
// user actually sees.
func TestLogin_InstallerContextSuppressesTheCliAdvice(t *testing.T) {
	pollBackend(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"expired_token"}`))
	})
	t.Setenv("TRACEBLOC_INSTALLER", "1")
	_, err := runCmd(t, "login")
	if err == nil {
		t.Fatal("an expired code must fail the sign-in")
	}
	if strings.Contains(err.Error(), "tracebloc login") {
		t.Errorf("under the installer the CLI must not tell the user to re-run login, got: %v", err)
	}
	if !strings.Contains(err.Error(), "10 minutes") {
		t.Errorf("the window must still be named under the installer, got: %v", err)
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

// TestLogout_UnknownEnvSkipsRevoke is the backend#2171 fail-closed contract for
// logout: a session whose current_env is unrecognised must still clear local state
// (logout's primary job), but must NOT build a client and revoke the token — that
// would send the credential to api.BaseURL's prod fallback, the very leak the fix
// prevents. The user is signed out locally and pointed at the dashboard.
func TestLogout_UnknownEnvSkipsRevoke(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	t.Setenv("CLIENT_ENV", "")
	if err := (&config.Config{CurrentEnv: "staging", Profiles: map[string]*config.Profile{
		"staging": {Token: "x", Email: "e@co", ActiveClientID: "7"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}

	var built []string
	orig := newAPIClient
	newAPIClient = func(env string) *api.Client {
		built = append(built, env)
		return &api.Client{BaseURL: api.BaseURL(env)}
	}
	t.Cleanup(func() { newAPIClient = orig })

	out, err := runCmd(t, "logout")
	if err != nil {
		t.Fatalf("logout must succeed for an unknown env (local clear is its primary job): %v", err)
	}
	if len(built) != 0 {
		t.Errorf("logout built a client for %v — an unknown env must never be revoked "+
			"against (it resolves to prod, sending the token there): backend#2171", built)
	}
	cfg, _ := config.Load()
	if cfg.SignedIn() || cfg.Current().ActiveClientID != "" {
		t.Errorf("local state must still be cleared for an unknown env: %+v", cfg)
	}
	if !strings.Contains(out, "Signed out locally") {
		t.Errorf("want the 'Signed out locally' hint naming the skipped revoke, got:\n%s", out)
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

// TestAuthCheck_VerboseUnreachableNotRejected (review #2): a non-401/403 WhoAmI
// failure (e.g. backend 500 / outage) must read as "couldn't verify", not a
// rejected token telling the user to re-login (which wouldn't help).
func TestAuthCheck_VerboseUnreachableNotRejected(t *testing.T) {
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo/" {
			w.WriteHeader(http.StatusInternalServerError) // reachable but erroring — not a rejection
		}
	})
	saveSignedIn(t, "tok") // CurrentEnv=dev
	out, err := runCmd(t, "auth", "status", "--check", "--verbose", "--env", "dev")
	if got := ExitCodeFromError(err); got != 1 {
		t.Fatalf("exit code = %d, want 1", got)
	}
	if !strings.Contains(out, "Couldn't verify your session") {
		t.Errorf("a 500 should read as 'couldn't verify', got:\n%s", out)
	}
	if strings.Contains(out, "was rejected") {
		t.Errorf("a 500 must not be labelled a rejected token: %s", out)
	}
}

// TestLogin_ClearsStaleIdentityOnWhoAmIFailure (review #3): a re-login as a
// different user must not inherit the previous user's identity if the WhoAmI
// confirmation fails — otherwise cli#137 would auto-name the new client after the
// wrong person.
func TestLogin_ClearsStaleIdentityOnWhoAmIFailure(t *testing.T) {
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"WDJB-MJHT","verification_uri":"https://x/activate","expires_in":600,"interval":5}`))
		case "/device/token":
			_, _ = w.Write([]byte(`{"token":"bob_tok"}`))
		case "/userinfo/":
			w.WriteHeader(http.StatusInternalServerError) // confirmation fails
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
		}
	})
	// Pre-existing session for a DIFFERENT user (Alice) on this env.
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "alice_tok", Email: "alice@co", FirstName: "Alice"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}

	if _, err := runCmd(t, "login"); err != nil {
		t.Fatalf("login should still succeed when WhoAmI fails: %v", err)
	}

	cfg, _ := config.Load()
	prof := cfg.Current()
	if prof.Token != "bob_tok" {
		t.Errorf("token = %q, want the new bob_tok", prof.Token)
	}
	if prof.FirstName != "" || prof.Email != "" {
		t.Errorf("stale identity leaked: FirstName=%q Email=%q (want both cleared)", prof.FirstName, prof.Email)
	}
}
