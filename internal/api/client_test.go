package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func TestBaseURL(t *testing.T) {
	cases := map[string]string{
		"dev":   "https://dev-api.tracebloc.io",
		"stg":   "https://stg-api.tracebloc.io",
		"prod":  "https://api.tracebloc.io",
		"":      "https://api.tracebloc.io",
		"DEV":   "https://dev-api.tracebloc.io", // case-insensitive
		"weird": "https://api.tracebloc.io",     // unknown -> prod
	}
	for env, want := range cases {
		if got := BaseURL(env); got != want {
			t.Errorf("BaseURL(%q) = %q, want %q", env, got, want)
		}
	}
}

func TestResolveEnv(t *testing.T) {
	t.Setenv("CLIENT_ENV", "stg")
	if got := ResolveEnv("dev"); got != "dev" {
		t.Errorf("explicit should win: got %q", got)
	}
	if got := ResolveEnv(""); got != "stg" {
		t.Errorf("CLIENT_ENV should be used: got %q", got)
	}
	t.Setenv("CLIENT_ENV", "")
	if got := ResolveEnv(""); got != "prod" {
		t.Errorf("default should be prod: got %q", got)
	}
}

func TestRequestDeviceCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/device/code" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"WDJB-MJHT","verification_uri":"https://x/activate","expires_in":600,"interval":5}`))
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	resp, err := c.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp.UserCode != "WDJB-MJHT" || resp.Interval != 5 || resp.DeviceCode != "dc" {
		t.Errorf("got %+v", resp)
	}
}

func TestPollTokenSequence(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		switch n {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"slow_down"}`))
		default:
			_, _ = w.Write([]byte(`{"token":"usertoken123"}`))
		}
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	ctx := context.Background()
	if _, err := c.PollToken(ctx, "dc"); !errors.Is(err, ErrAuthorizationPending) {
		t.Errorf("poll 1: want pending, got %v", err)
	}
	if _, err := c.PollToken(ctx, "dc"); !errors.Is(err, ErrSlowDown) {
		t.Errorf("poll 2: want slow_down, got %v", err)
	}
	tok, err := c.PollToken(ctx, "dc")
	if err != nil || tok != "usertoken123" {
		t.Errorf("poll 3: want token, got %q / %v", tok, err)
	}
}

func TestPollTokenDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	if _, err := c.PollToken(context.Background(), "dc"); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("want access_denied, got %v", err)
	}
}

func TestWhoAmI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/userinfo/" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer usertoken123" {
			t.Errorf("auth header = %q, want %q", got, "Bearer usertoken123")
		}
		_, _ = w.Write([]byte(`{"email":"ds@tracebloc.io","type":"DS","account":"Acme"}`))
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	c.Token = "usertoken123"
	id, err := c.WhoAmI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "ds@tracebloc.io" || id.Account != "Acme" {
		t.Errorf("got %+v", id)
	}
}

func TestWhoAmIUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Invalid token."}`))
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	c.Token = "bad"
	var ae *APIError
	if _, err := c.WhoAmI(context.Background()); !errors.As(err, &ae) || ae.StatusCode != http.StatusUnauthorized {
		t.Errorf("want APIError 401, got %v", err)
	}
}

func TestCreateClientMintAndAdopt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int
		wantAdopted bool
	}{
		{"mint", http.StatusCreated, false},
		{"adopt", http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent CreateClientRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/edge-device/" || r.Method != http.MethodPost {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewDecoder(r.Body).Decode(&sent)
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(`{"id":5,"first_name":"c","namespace":"c","cluster_id":"uid-1"}`))
			}))
			defer srv.Close()
			c := New("prod")
			c.BaseURL = srv.URL
			pc, adopted, err := c.CreateClient(context.Background(),
				CreateClientRequest{Name: "c", Namespace: "c", Password: "pw", ClusterID: "uid-1"})
			if err != nil {
				t.Fatal(err)
			}
			if adopted != tc.wantAdopted {
				t.Errorf("adopted = %v, want %v", adopted, tc.wantAdopted)
			}
			if sent.ClusterID != "uid-1" {
				t.Errorf("cluster_id sent = %q, want uid-1", sent.ClusterID)
			}
			if pc.ClusterID != "uid-1" {
				t.Errorf("cluster_id parsed = %q, want uid-1", pc.ClusterID)
			}
		})
	}
}

func TestCreateClientConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"cluster_conflict","cluster_id":"uid-1"}`))
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	var ae *APIError
	_, _, err := c.CreateClient(context.Background(), CreateClientRequest{ClusterID: "uid-1"})
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusConflict {
		t.Errorf("want APIError 409, got %v", err)
	}
}

// TestListClients_FollowsPagination guards that DRF pagination is still
// followed end-to-end after the nextPath refactor (page 1 → page 2 → done).
func TestListClients_FollowsPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"next":"","results":[{"id":2,"first_name":"b","namespace":"b"}]}`))
			return
		}
		// DRF emits an absolute `next`; nextPath keeps only path+query.
		_, _ = w.Write([]byte(`{"next":"http://x/edge-device/?page=2","results":[{"id":1,"first_name":"a","namespace":"a"}]}`))
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	got, err := c.ListClients(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("want 2 clients [1,2], got %+v", got)
	}
}

// TestListClients_UnparseableNextLink_IsError pins the Bugbot fix (v0.4.0 RC):
// a non-empty `next` the server sends that url.Parse rejects must be a hard
// error, never a silent truncation to the pages seen so far — otherwise list /
// `use` / namespace-collision checks would miss clients without any error.
func TestListClients_UnparseableNextLink_IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// `next` carries a control byte (\u007f) → url.Parse fails.
		_, _ = w.Write([]byte(`{"next":"http://x/\u007f","results":[{"id":1,"first_name":"a","namespace":"a"}]}`))
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	if _, err := c.ListClients(context.Background()); err == nil {
		t.Fatal("expected an error on an unparseable next link, got nil (silent truncation)")
	}
}

// TestListClients_BareArrayUnpaginated covers the unpaginated deployment shape
// (a bare JSON array) — still returned as-is after the bare-decode was guarded
// to the first page.
func TestListClients_BareArrayUnpaginated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"first_name":"a","namespace":"a"},{"id":2,"first_name":"b","namespace":"b"}]`))
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	got, err := c.ListClients(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("want 2 clients [1,2] from a bare array, got %+v", got)
	}
}

// ── cli#98: User-Agent version header + 426 Upgrade Required (RFC-0001 §14 R11) ──

// TestUserAgentHeaderSent proves the transport wrapper puts the configured
// version on the wire for a real request (here a GET via WhoAmI).
func TestUserAgentHeaderSent(t *testing.T) {
	old := userAgent
	defer func() { userAgent = old }()
	SetUserAgent("7.7.7")

	want := "tracebloc-cli/7.7.7 (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"email":"x","type":"y","account":"z"}`))
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	c.Token = "t"
	if _, err := c.WhoAmI(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
}

// TestUserAgentDevFallback: a build that never called SetUserAgent (or with an
// empty version) reports "dev" — which the backend can't parse, so it fails open.
func TestUserAgentDevFallback(t *testing.T) {
	old := userAgent
	defer func() { userAgent = old }()

	want := "tracebloc-cli/dev (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
	userAgent = ""
	if got := currentUserAgent(); got != want {
		t.Errorf("unset → currentUserAgent() = %q, want %q", got, want)
	}
	SetUserAgent("")
	if got := currentUserAgent(); got != want {
		t.Errorf(`SetUserAgent("") → %q, want %q`, got, want)
	}
}

// TestUpgradeRequired426 pins the central 426 handling: a 426 from any endpoint
// (GET or POST) surfaces as a typed *UpgradeRequiredError carrying min_version,
// not a raw *APIError — so every command degrades to the same upgrade message.
func TestUpgradeRequired426(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired) // 426
		_, _ = w.Write([]byte(`{"error":"upgrade_required","min_version":"0.4.0"}`))
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	c.Token = "t"

	var ue *UpgradeRequiredError
	if _, err := c.WhoAmI(context.Background()); !errors.As(err, &ue) || ue.MinVersion != "0.4.0" {
		t.Errorf("GET on 426: want *UpgradeRequiredError{0.4.0}, got %v", err)
	}
	ue = nil
	if _, err := c.RequestDeviceCode(context.Background()); !errors.As(err, &ue) || ue.MinVersion != "0.4.0" {
		t.Errorf("POST on 426: want *UpgradeRequiredError{0.4.0}, got %v", err)
	}
}

// TestUpgradeRequiredErrorMessage: the message is actionable (names the floor)
// and stays sensible when the server didn't send a min_version.
func TestUpgradeRequiredErrorMessage(t *testing.T) {
	msg := (&UpgradeRequiredError{MinVersion: "0.4.0"}).Error()
	if !strings.Contains(msg, "0.4.0") || !strings.Contains(msg, "too old") {
		t.Errorf("message not actionable: %q", msg)
	}
	if got := (&UpgradeRequiredError{}).Error(); strings.Contains(got, ">=") {
		t.Errorf("empty min_version should not print a bare '>=': %q", got)
	}
}

// TestUpgradeRequired426_UnparseableBody: a 426 whose body doesn't parse still
// yields an *UpgradeRequiredError (the status is the contract), with no min_version.
func TestUpgradeRequired426_UnparseableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	c := New("prod")
	c.BaseURL = srv.URL
	var ue *UpgradeRequiredError
	if _, err := c.RequestDeviceCode(context.Background()); !errors.As(err, &ue) {
		t.Fatalf("want *UpgradeRequiredError even on unparseable body, got %v", err)
	}
	if ue.MinVersion != "" {
		t.Errorf("MinVersion = %q, want empty", ue.MinVersion)
	}
}
