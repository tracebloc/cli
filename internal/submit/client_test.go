package submit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPSubmitter_HappyPath: jobs-manager returns 201 with the
// canonical body shape; client decodes correctly + surfaces all
// three response fields.
func TestHTTPSubmitter_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pin the wire format jobs-manager actually expects.
		if r.Method != http.MethodPost {
			t.Errorf("got method %s, want POST", r.Method)
		}
		if r.URL.Path != SubmitPath {
			t.Errorf("got path %s, want %s", r.URL.Path, SubmitPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fake-token-deadbeef" {
			t.Errorf("Authorization = %q, want Bearer fake-token-deadbeef", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		body, _ := io.ReadAll(r.Body)
		// Light shape check — body_test.go pins the full JSON shape.
		if !strings.Contains(string(body), `"ingest_config"`) {
			t.Errorf("body missing ingest_config: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"job_name":  "ingestor-abc123",
			"namespace": "tracebloc",
			"replay":    false,
		})
	}))
	defer srv.Close()

	s := NewHTTPSubmitter(srv.URL, "fake-token-deadbeef")
	req, _ := BuildRequest("yaml-content", "key1", "")

	resp, err := s.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if resp.JobName != "ingestor-abc123" {
		t.Errorf("JobName = %q, want ingestor-abc123", resp.JobName)
	}
	if resp.Namespace != "tracebloc" {
		t.Errorf("Namespace = %q, want tracebloc", resp.Namespace)
	}
	if resp.Replay {
		t.Errorf("Replay = true, want false")
	}
}

// TestHTTPSubmitter_ReplayResponse: replay=true is also a success
// path. The orchestrator distinguishes the two via the response
// flag, not via HTTP status — both come back 2xx.
func TestHTTPSubmitter_ReplayResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // jobs-manager returns 200 for replays per source
		_ = json.NewEncoder(w).Encode(map[string]any{
			"job_name":  "existing-job",
			"namespace": "tracebloc",
			"replay":    true,
		})
	}))
	defer srv.Close()

	s := NewHTTPSubmitter(srv.URL, "tok")
	req, _ := BuildRequest("yaml", "same-key", "")
	resp, err := s.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit on replay: %v", err)
	}
	if !resp.Replay {
		t.Errorf("Replay = false, want true")
	}
}

// TestHTTPSubmitter_4xxSurfacesBody: 4xx from jobs-manager
// surfaces the verbatim body so the customer sees jobs-manager's
// actual diagnostic (typically {"detail": "..."}), not just
// "HTTP 422".
func TestHTTPSubmitter_4xxSurfacesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"ingest_config schema rejected: missing required field 'intent'"}`))
	}))
	defer srv.Close()

	s := NewHTTPSubmitter(srv.URL, "tok")
	req, _ := BuildRequest("yaml", "k", "")
	_, err := s.Submit(context.Background(), req)
	if err == nil {
		t.Fatal("Submit returned nil on 4xx")
	}
	if !IsSubmitError(err) {
		t.Errorf("err is not *SubmitError: %T", err)
	}
	for _, want := range []string{"HTTP 422", "missing required field 'intent'"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestHTTPSubmitter_401IsAuthError: distinguishes the auth case
// (401/403) from generic 4xx for the orchestrator's exit-code
// mapping. Used by the CLI to return "your SA token doesn't
// work" vs "your spec was rejected."
func TestHTTPSubmitter_401IsAuthError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"detail":"token expired"}`))
			}))
			defer srv.Close()

			s := NewHTTPSubmitter(srv.URL, "tok")
			req, _ := BuildRequest("yaml", "k", "")
			_, err := s.Submit(context.Background(), req)
			if err == nil {
				t.Fatal("Submit returned nil on auth error")
			}
			if !IsAuthError(err) {
				t.Errorf("IsAuthError(%v) = false, want true (status=%d)", err, status)
			}
		})
	}
}

// TestHTTPSubmitter_5xxNotAuthError: 5xx is server-side trouble
// (kube-apiserver flake, jobs-manager bug), NOT an auth issue.
// IsAuthError should return false so the orchestrator routes to
// the right exit-code bucket.
func TestHTTPSubmitter_5xxNotAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"internal"}`))
	}))
	defer srv.Close()

	s := NewHTTPSubmitter(srv.URL, "tok")
	req, _ := BuildRequest("yaml", "k", "")
	_, err := s.Submit(context.Background(), req)
	if err == nil {
		t.Fatal("Submit returned nil on 500")
	}
	if IsAuthError(err) {
		t.Errorf("IsAuthError(500) = true, want false")
	}
	if !IsSubmitError(err) {
		t.Errorf("err is not *SubmitError: %T", err)
	}
}

// TestHTTPSubmitter_2xxMissingJobName: a malformed 2xx response
// (server bug or version drift) without job_name is a hard error,
// not a silent success — the orchestrator wouldn't know what to
// watch.
func TestHTTPSubmitter_2xxMissingJobName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"namespace":"tracebloc","replay":false}`))
	}))
	defer srv.Close()

	s := NewHTTPSubmitter(srv.URL, "tok")
	req, _ := BuildRequest("yaml", "k", "")
	_, err := s.Submit(context.Background(), req)
	if err == nil {
		t.Fatal("Submit returned nil on missing job_name")
	}
	if !strings.Contains(err.Error(), "missing job_name") {
		t.Errorf("error missing 'missing job_name' framing: %v", err)
	}
}

// TestHTTPSubmitter_NetworkError: unreachable server (closed
// httptest.Server) surfaces as a wrapped net error with the
// endpoint in the message so customers know what was attempted.
func TestHTTPSubmitter_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	endpoint := srv.URL
	srv.Close() // kill the server

	s := NewHTTPSubmitter(endpoint, "tok")
	req, _ := BuildRequest("yaml", "k", "")
	_, err := s.Submit(context.Background(), req)
	if err == nil {
		t.Fatal("Submit returned nil on unreachable server")
	}
	if !strings.Contains(err.Error(), endpoint) {
		t.Errorf("error missing endpoint URL %q: %v", endpoint, err)
	}
}

// TestHTTPSubmitter_RespectsContext: ctx cancellation aborts the
// in-flight POST. Critical for the SIGINT path (main.go's
// signal.NotifyContext).
func TestHTTPSubmitter_RespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wait longer than the test's ctx allows.
		<-r.Context().Done()
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	s := NewHTTPSubmitter(srv.URL, "tok")
	req, _ := BuildRequest("yaml", "k", "")
	_, err := s.Submit(ctx, req)
	if err == nil {
		t.Fatal("Submit returned nil on cancelled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error doesn't wrap context.Canceled: %v", err)
	}
}
