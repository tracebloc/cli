package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
