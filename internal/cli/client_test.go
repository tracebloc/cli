package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

// withClientBackend points the client commands at an httptest server (via the
// newAPIClient seam) and writes a signed-in config to a temp dir.
func withClientBackend(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{Env: "dev", Token: "tok"}).Save(); err != nil {
		t.Fatal(err)
	}
	orig := newAPIClient
	newAPIClient = func(string) *api.Client {
		return &api.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	}
	t.Cleanup(func() { newAPIClient = orig })
}

func TestClientCreate_Success(t *testing.T) {
	var body api.CreateClientRequest
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[]`)) // no existing clients
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("auth header = %q, want Bearer tok", got)
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":5,"first_name":"my-client","username":"u-123","namespace":"my-client","location":"DE"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), nil, "my-client", "DE", true); err != nil {
		t.Fatalf("create: %v", err)
	}
	if body.Namespace != "my-client" || body.Location != "DE" || body.Password == "" {
		t.Errorf("create body = %+v", body)
	}
	cfg, _ := config.Load()
	if cfg.ActiveClientID != "5" {
		t.Errorf("active client = %q, want 5", cfg.ActiveClientID)
	}
	if !strings.Contains(out.String(), "u-123") {
		t.Errorf("output missing username:\n%s", out.String())
	}
}

func TestClientCreate_AskAnAdmin(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/edge-device/" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/edge-device/" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"detail":"no permission"}`))
		case r.URL.Path == "/edge-device/admins/":
			_, _ = w.Write([]byte(`[{"name":"Ada","email":"ada@co.io"}]`))
		}
	})
	var out bytes.Buffer
	err := runClientCreate(context.Background(), ui.New(&out), nil, "my-client", "DE", true)
	if err == nil || !strings.Contains(err.Error(), "CLIENT_WRITE") {
		t.Errorf("want permission error, got %v", err)
	}
	if !strings.Contains(out.String(), "ada@co.io") {
		t.Errorf("expected admins shown, got:\n%s", out.String())
	}
}

func TestClientCreate_RequiresLogin(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir()) // no config → not signed in
	err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil, "x", "DE", true)
	if err == nil || !strings.Contains(err.Error(), "login") {
		t.Errorf("want not-signed-in error, got %v", err)
	}
}

func TestClientList(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"first_name":"alpha","namespace":"alpha","location":"DE"},{"id":2,"first_name":"beta","namespace":"beta","location":"US"}]`))
	})
	var out bytes.Buffer
	if err := runClientList(context.Background(), ui.New(&out)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alpha", "beta"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("list missing %q:\n%s", want, out.String())
		}
	}
}

func TestClientUse(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":7,"first_name":"gamma","namespace":"gamma"}]`))
	})
	if err := runClientUse(context.Background(), ui.New(&bytes.Buffer{}), "7"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if cfg.ActiveClientID != "7" {
		t.Errorf("active = %q, want 7", cfg.ActiveClientID)
	}
	if err := runClientUse(context.Background(), ui.New(&bytes.Buffer{}), "99"); err == nil {
		t.Error("expected an error for an unknown client id")
	}
}

func TestClientCreate_Interactive(t *testing.T) {
	var body api.CreateClientRequest
	posted := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			posted = true
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":9,"first_name":"Lab One","username":"u-9","namespace":"lab-one","location":"DE"}`))
		}
	})
	confirmYes := true
	pr := &fakePrompter{
		answers: map[string]string{
			"Client name":             "Lab One",
			"Location zone (e.g. DE)": "DE",
		},
		confirm: &confirmYes,
	}
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), pr, "", "", false); err != nil {
		t.Fatalf("interactive create: %v", err)
	}
	if !posted {
		t.Fatal("expected a POST after the user confirmed")
	}
	if body.Name != "Lab One" || body.Namespace != "lab-one" || body.Location != "DE" {
		t.Errorf("create body = %+v", body)
	}
	if !strings.Contains(out.String(), "Review") {
		t.Errorf("expected a review section before the confirm, got:\n%s", out.String())
	}
}

func TestClientCreate_InteractiveCancel(t *testing.T) {
	posted := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
		}
		_, _ = w.Write([]byte(`[]`))
	})
	confirmNo := false
	pr := &fakePrompter{
		answers: map[string]string{
			"Client name":             "Lab Two",
			"Location zone (e.g. DE)": "US",
		},
		confirm: &confirmNo,
	}
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), pr, "", "", false); err != nil {
		t.Fatalf("declining the confirm should be a clean exit, got: %v", err)
	}
	if posted {
		t.Error("no client should be created when the user declines the confirm")
	}
	if !strings.Contains(out.String(), "Cancelled") {
		t.Errorf("expected a Cancelled note, got:\n%s", out.String())
	}
}

func TestClientList_Paginated(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"count":2,"next":null,"results":[{"id":2,"first_name":"beta","namespace":"beta"}]}`))
			return
		}
		// page 1: an absolute `next` link, like real DRF pagination
		_, _ = fmt.Fprintf(w, `{"count":2,"next":"http://%s/edge-device/?page=2","results":[{"id":1,"first_name":"alpha","namespace":"alpha"}]}`, r.Host)
	})
	var out bytes.Buffer
	if err := runClientList(context.Background(), ui.New(&out)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alpha", "beta"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("paginated list missing %q (next not followed?):\n%s", want, out.String())
		}
	}
}

func TestClientCreate_CollisionSuffix(t *testing.T) {
	var body api.CreateClientRequest
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			// an existing client already holds "my-client"
			_, _ = w.Write([]byte(`[{"id":1,"first_name":"My Client","namespace":"my-client"}]`))
		case r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":2,"first_name":"My Client","username":"u-2","namespace":"my-client-2","location":"DE"}`))
		}
	})
	if err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil, "My Client", "DE", true); err != nil {
		t.Fatal(err)
	}
	if body.Namespace != "my-client-2" {
		t.Errorf("namespace = %q, want my-client-2 (collision suffix not applied)", body.Namespace)
	}
}
