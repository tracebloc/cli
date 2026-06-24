package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

// withClientBackend points the client commands at an httptest server (via the
// newAPIClient seam) and writes a signed-in config to a temp dir. It also stubs
// readClusterID to "no cluster" by default, so create tests never touch a real
// kubeconfig/cluster — tests that exercise the anchor override it via stubClusterID.
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

	origCID := readClusterID
	readClusterID = func(context.Context, cluster.KubeconfigOptions) (string, error) {
		return "", errors.New("no cluster reachable (test default)")
	}
	t.Cleanup(func() { readClusterID = origCID })
}

// stubClusterID overrides the cluster-anchor read for a single test.
func stubClusterID(t *testing.T, uid string, err error) {
	t.Helper()
	orig := readClusterID
	readClusterID = func(context.Context, cluster.KubeconfigOptions) (string, error) {
		return uid, err
	}
	t.Cleanup(func() { readClusterID = orig })
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
	if err := runClientCreate(context.Background(), ui.New(&out), nil, clientCreateOpts{name: "my-client", location: "DE", yes: true}); err != nil {
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
	err := runClientCreate(context.Background(), ui.New(&out), nil, clientCreateOpts{name: "my-client", location: "DE", yes: true})
	if err == nil || !strings.Contains(err.Error(), "CLIENT_WRITE") {
		t.Errorf("want permission error, got %v", err)
	}
	if !strings.Contains(out.String(), "ada@co.io") {
		t.Errorf("expected admins shown, got:\n%s", out.String())
	}
}

func TestClientCreate_RequiresLogin(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir()) // no config → not signed in
	err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil, clientCreateOpts{name: "x", location: "DE", yes: true})
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
	if err := runClientCreate(context.Background(), ui.New(&out), pr, clientCreateOpts{}); err != nil {
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
	if err := runClientCreate(context.Background(), ui.New(&out), pr, clientCreateOpts{}); err != nil {
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
	if err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil, clientCreateOpts{name: "My Client", location: "DE", yes: true}); err != nil {
		t.Fatal(err)
	}
	if body.Namespace != "my-client-2" {
		t.Errorf("namespace = %q, want my-client-2 (collision suffix not applied)", body.Namespace)
	}
}

func TestClientCreate_AnchorMint(t *testing.T) {
	var body api.CreateClientRequest
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated) // 201 = minted
			_, _ = w.Write([]byte(`{"id":5,"first_name":"c","username":"u-5","namespace":"c","location":"DE","cluster_id":"uid-1"}`))
		}
	})
	stubClusterID(t, "uid-1", nil)
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), nil, clientCreateOpts{name: "c", location: "DE", yes: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if body.ClusterID != "uid-1" {
		t.Errorf("cluster_id sent = %q, want uid-1 (anchor not wired into the request)", body.ClusterID)
	}
	if !strings.Contains(out.String(), "Machine credential") {
		t.Errorf("mint should print the credential, got:\n%s", out.String())
	}
}

func TestClientCreate_AdoptIdempotent(t *testing.T) {
	posts := 0
	var lastBody api.CreateClientRequest
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost:
			posts++
			_ = json.NewDecoder(r.Body).Decode(&lastBody)
			w.WriteHeader(http.StatusOK) // 200 = adopted an existing client
			_, _ = w.Write([]byte(`{"id":8,"first_name":"existing","username":"u-8","namespace":"existing","location":"DE","cluster_id":"uid-1"}`))
		}
	})
	stubClusterID(t, "uid-1", nil)

	run := func() string {
		var out bytes.Buffer
		if err := runClientCreate(context.Background(), ui.New(&out), nil, clientCreateOpts{name: "c", location: "DE", yes: true}); err != nil {
			t.Fatalf("adopt: %v", err)
		}
		return out.String()
	}
	// Two runs against the same cluster — real re-run idempotency: each POSTs once,
	// both adopt the SAME existing client, neither prints a credential.
	first, second := run(), run()
	if posts != 2 {
		t.Fatalf("posts = %d, want 2 (each run POSTs once)", posts)
	}
	for i, out := range []string{first, second} {
		if !strings.Contains(out, "adopted") {
			t.Errorf("run %d: adopt should say so, got:\n%s", i, out)
		}
		if strings.Contains(out, "Machine credential") {
			t.Errorf("run %d: adopt must NOT print a credential, got:\n%s", i, out)
		}
	}
	// The credential is still SENT on every create (the backend uses it only on a
	// mint, §7.2) even though it's never printed on adopt.
	if lastBody.Password == "" {
		t.Error("password should still be sent in the adopt POST body")
	}
	cfg, _ := config.Load()
	if cfg.ActiveClientID != "8" {
		t.Errorf("active client = %q, want 8 (adopted id)", cfg.ActiveClientID)
	}
}

func TestClientCreate_CredentialFileMint(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":5,"first_name":"c","username":"u-5","namespace":"my-ns","location":"DE","cluster_id":"uid-1"}`))
		}
	})
	stubClusterID(t, "uid-1", nil)
	credPath := filepath.Join(t.TempDir(), "cred.env")
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), nil,
		clientCreateOpts{name: "c", location: "DE", yes: true, credentialFile: credPath}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// never-show: the secret must NOT hit the terminal.
	if strings.Contains(out.String(), "Machine credential") || strings.Contains(out.String(), "password") {
		t.Errorf("credential must not be printed when --credential-file is set, got:\n%s", out.String())
	}
	// the file is 0600 and carries the sourceable credential.
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("credential file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential file mode = %o, want 600", perm)
	}
	kv := parseEnvFile(t, credPath)
	if kv["TRACEBLOC_CLIENT_ID"] != "5" || kv["TB_NAMESPACE"] != "my-ns" || kv["TRACEBLOC_CLIENT_PASSWORD"] == "" {
		t.Errorf("credential file = %v (want id=5, ns=my-ns, non-empty password)", kv)
	}
	// never-show, the real invariant: the minted password VALUE must not appear
	// in stdout under any label (the string checks above are just a proxy).
	if strings.Contains(out.String(), kv["TRACEBLOC_CLIENT_PASSWORD"]) {
		t.Errorf("minted password leaked to the terminal:\n%s", out.String())
	}
}

// TestClientCreate_CredentialFilePreexistingPerms locks in the 0600 guarantee
// when the target already exists with looser perms — os.WriteFile would have
// kept the stale mode and leaked the secret group/other-readable.
func TestClientCreate_CredentialFilePreexistingPerms(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":5,"first_name":"c","username":"u-5","namespace":"my-ns","location":"DE","cluster_id":"uid-1"}`))
		}
	})
	stubClusterID(t, "uid-1", nil)
	credPath := filepath.Join(t.TempDir(), "cred.env")
	// A stale, world-readable file already sits at the target path.
	if err := os.WriteFile(credPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), nil,
		clientCreateOpts{name: "c", location: "DE", yes: true, credentialFile: credPath}); err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("credential file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential file mode = %o over a pre-existing 0644 target, want 600", perm)
	}
}

// TestClientCreate_CredentialFileWriteFailFatal asserts a credential-file write
// failure is fatal — the minted password is the only copy, so a failed write must
// surface an error, never a silent drop. The target's parent is a regular file, so
// the directory create (hence the write) fails deterministically.
func TestClientCreate_CredentialFileWriteFailFatal(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":5,"first_name":"c","username":"u-5","namespace":"my-ns","location":"DE","cluster_id":"uid-1"}`))
		}
	})
	stubClusterID(t, "uid-1", nil)
	notADir := filepath.Join(t.TempDir(), "iam-a-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(notADir, "cred.env") // parent is a file → write fails
	var out bytes.Buffer
	err := runClientCreate(context.Background(), ui.New(&out), nil,
		clientCreateOpts{name: "c", location: "DE", yes: true, credentialFile: credPath})
	if err == nil {
		t.Fatal("expected a fatal error when the credential file can't be written, got nil")
	}
}

func TestClientCreate_CredentialFileAdopt(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK) // adopt
			_, _ = w.Write([]byte(`{"id":8,"first_name":"existing","username":"u-8","namespace":"ex-ns","location":"DE","cluster_id":"uid-1"}`))
		}
	})
	stubClusterID(t, "uid-1", nil)
	credPath := filepath.Join(t.TempDir(), "cred.env")
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), nil,
		clientCreateOpts{name: "c", location: "DE", yes: true, credentialFile: credPath}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	kv := parseEnvFile(t, credPath)
	// adopt emits id + namespace + the ADOPTED marker, but NO password (the
	// existing one stands; it's write-only on the backend).
	if kv["TRACEBLOC_CLIENT_ID"] != "8" || kv["TB_NAMESPACE"] != "ex-ns" || kv["TRACEBLOC_CLIENT_ADOPTED"] != "1" {
		t.Errorf("adopt credential file = %v (want id=8, ns=ex-ns, ADOPTED=1)", kv)
	}
	if _, hasPw := kv["TRACEBLOC_CLIENT_PASSWORD"]; hasPw {
		t.Errorf("adopt must not write a password (none issued), got:\n%v", kv)
	}
}

// parseEnvFile reads a KEY=value env file (skipping # comments) into a map.
func parseEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	kv := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

func TestClientCreate_ClusterConflict(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusConflict) // 409 = bound to another account (R6)
			_, _ = w.Write([]byte(`{"error":"cluster_conflict","cluster_id":"uid-1"}`))
		}
	})
	stubClusterID(t, "uid-1", nil)
	err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil, clientCreateOpts{name: "c", location: "DE", yes: true})
	if err == nil || !strings.Contains(err.Error(), "different tracebloc account") {
		t.Errorf("want a cluster_conflict error, got %v", err)
	}
}

func TestClientCreate_NoClusterAnchorWarns(t *testing.T) {
	var body api.CreateClientRequest
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":3,"first_name":"c","username":"u-3","namespace":"c","location":"DE"}`))
		}
	})
	// readClusterID left at the withClientBackend default (returns an error).
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), nil, clientCreateOpts{name: "c", location: "DE", yes: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if body.ClusterID != "" {
		t.Errorf("cluster_id sent = %q, want empty (no anchor when cluster unreadable)", body.ClusterID)
	}
	if !strings.Contains(out.String(), "without a cluster anchor") {
		t.Errorf("expected a never-silent hint about the missing anchor, got:\n%s", out.String())
	}
	// The no-anchor path must still complete a full mint — the credential is shown.
	if !strings.Contains(out.String(), "Machine credential") {
		t.Errorf("no-anchor fallback should still print the credential, got:\n%s", out.String())
	}
}
