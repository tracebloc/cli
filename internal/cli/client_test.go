package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "tok"},
	}}).Save(); err != nil {
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

	// Default: no client live on the cluster, so the R7 adopt-backfill path is a
	// no-op and create tests never touch a real kubeconfig. Tests that exercise R7
	// override it via stubInClusterClient.
	origLive := readInClusterClient
	readInClusterClient = func(context.Context, cluster.KubeconfigOptions) (*cluster.InClusterClient, error) {
		return nil, nil
	}
	t.Cleanup(func() { readInClusterClient = origLive })
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

// stubInClusterClient overrides the live in-cluster client discovery (R7).
func stubInClusterClient(t *testing.T, lc *cluster.InClusterClient, err error) {
	t.Helper()
	orig := readInClusterClient
	readInClusterClient = func(context.Context, cluster.KubeconfigOptions) (*cluster.InClusterClient, error) {
		return lc, err
	}
	t.Cleanup(func() { readInClusterClient = orig })
}

// signInAs sets the active profile's identity (first name + email) so the cli#137
// auto-name (<firstname>-NN) is deterministic in a test. Call after
// withClientBackend, which creates the profile.
func signInAs(t *testing.T, firstName, email string) {
	t.Helper()
	// Guard against writing to the developer's real ~/.tracebloc: this helper only
	// makes sense once withClientBackend has redirected config to a temp dir.
	if os.Getenv("TRACEBLOC_CONFIG_DIR") == "" {
		t.Fatal("signInAs: TRACEBLOC_CONFIG_DIR is unset — call withClientBackend first")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Current()
	p.FirstName, p.Email = firstName, email
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
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
	if cfg.Current().ActiveClientID != "5" {
		t.Errorf("active client = %q, want 5", cfg.Current().ActiveClientID)
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

// TestClientCreate_R7_AdoptBackfill: a client is live on this cluster with a null
// backend cluster_id (existing fleet). Create must backfill the anchor onto it
// (PATCH) and ADOPT it — never mint a duplicate (cli#131 / RFC-0001 §7.2).
func TestClientCreate_R7_AdoptBackfill(t *testing.T) {
	var patchedCluster string
	postCalled := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			// The live client is in this account, anchor still null.
			_, _ = w.Write([]byte(`[{"id":7,"first_name":"box","username":"uuid-live","namespace":"ns-live","cluster_id":""}]`))
		case r.Method == http.MethodPatch && r.URL.Path == "/edge-device/7/":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			patchedCluster = body["cluster_id"]
			_, _ = w.Write([]byte(`{"id":7,"first_name":"box","username":"uuid-live","namespace":"ns-live","cluster_id":"uid-9"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			postCalled = true
			t.Error("mint POST must NOT be called on the R7 adopt-backfill path")
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	stubClusterID(t, "uid-9", nil)
	stubInClusterClient(t, &cluster.InClusterClient{ClientID: "uuid-live", Namespace: "ns-live"}, nil)

	credFile := filepath.Join(t.TempDir(), "cred.env")
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), nil,
		clientCreateOpts{name: "box", location: "DE", yes: true, credentialFile: credFile}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if postCalled {
		t.Fatal("minted instead of adopting")
	}
	if patchedCluster != "uid-9" {
		t.Errorf("backfilled cluster_id = %q, want uid-9", patchedCluster)
	}
	cfg, _ := config.Load()
	if cfg.Current().ActiveClientID != "7" {
		t.Errorf("active client = %q, want 7 (the adopted live client)", cfg.Current().ActiveClientID)
	}
	cred, _ := os.ReadFile(credFile)
	if !strings.Contains(string(cred), "TRACEBLOC_CLIENT_ADOPTED=1") ||
		!strings.Contains(string(cred), "TRACEBLOC_CLIENT_ID=uuid-live") ||
		strings.Contains(string(cred), "TRACEBLOC_CLIENT_PASSWORD") {
		t.Errorf("adopt credential file wrong (want id+ADOPTED, no password):\n%s", cred)
	}
}

// TestClientCreate_R7_AlreadyAnchored: the live client already carries this
// cluster's anchor → adopt directly, no PATCH, no mint.
func TestClientCreate_R7_AlreadyAnchored(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[{"id":7,"first_name":"box","username":"uuid-live","namespace":"ns-live","cluster_id":"uid-9"}]`))
		default:
			t.Errorf("unexpected %s %s (no PATCH/POST expected)", r.Method, r.URL.Path)
		}
	})
	stubClusterID(t, "uid-9", nil)
	stubInClusterClient(t, &cluster.InClusterClient{ClientID: "uuid-live", Namespace: "ns-live"}, nil)

	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), nil,
		clientCreateOpts{name: "box", location: "DE", yes: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	cfg, _ := config.Load()
	if cfg.Current().ActiveClientID != "7" {
		t.Errorf("active client = %q, want 7", cfg.Current().ActiveClientID)
	}
}

// TestClientCreate_R7_CrossAccountRefuse: a client is live here but it isn't in
// the signed-in account — refuse rather than mint over it or silently adopt.
func TestClientCreate_R7_CrossAccountRefuse(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[]`)) // signed-in account owns no such client
		default:
			t.Errorf("unexpected %s %s (must refuse before PATCH/POST)", r.Method, r.URL.Path)
		}
	})
	stubClusterID(t, "uid-9", nil)
	stubInClusterClient(t, &cluster.InClusterClient{ClientID: "uuid-foreign", Namespace: "ns"}, nil)

	err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil,
		clientCreateOpts{name: "box", location: "DE", yes: true})
	if err == nil || !strings.Contains(err.Error(), "different tracebloc account") {
		t.Errorf("want cross-account refusal, got %v", err)
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
	if cfg.Current().ActiveClientID != "7" {
		t.Errorf("active = %q, want 7", cfg.Current().ActiveClientID)
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
			_, _ = w.Write([]byte(`{"id":9,"first_name":"lab-01","username":"u-9","namespace":"lab-01"}`))
		}
	})
	signInAs(t, "Lab", "lab@example.com") // auto-name base "lab"
	// No name/location prompts anymore (cli#137): the name is auto-derived and
	// location is optional, so an interactive run only reaches the confirm.
	confirmYes := true
	pr := &fakePrompter{confirm: &confirmYes}
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), pr, clientCreateOpts{}); err != nil {
		t.Fatalf("interactive create: %v", err)
	}
	if !posted {
		t.Fatal("expected a POST after the user confirmed")
	}
	if body.Name != "lab-01" || body.Namespace != "lab-01" {
		t.Errorf("auto-named create body = %+v, want name/namespace lab-01", body)
	}
	if body.Location != "" {
		t.Errorf("location = %q, want empty (no --location given, none sent)", body.Location)
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
	signInAs(t, "Lab", "lab@example.com")
	confirmNo := false
	pr := &fakePrompter{confirm: &confirmNo}
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
	// The printed "client id" is what the installer's Client ID prompt consumes —
	// it must be the UUID username (u-5), the same value written to the credential
	// file, not the numeric dashboard id. Assert the username is shown as the id.
	if !strings.Contains(out.String(), "client id") || !strings.Contains(out.String(), "u-5") {
		t.Errorf("mint should print the username (u-5) as the client id, got:\n%s", out.String())
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
	if cfg.Current().ActiveClientID != "8" {
		t.Errorf("active client = %q, want 8 (adopted id)", cfg.Current().ActiveClientID)
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
	// TRACEBLOC_CLIENT_ID must be the UUID *username* (here "u-5"), NOT the numeric
	// dashboard id (5): it becomes the pod's CLIENT_ID, which controller.py sends to
	// api-token-auth as the login username. The backend authenticates an EdgeDevice
	// by its username, so writing the id crash-loops the client on "Unable to log in".
	if kv["TRACEBLOC_CLIENT_ID"] != "u-5" || kv["TB_NAMESPACE"] != "my-ns" || kv["TRACEBLOC_CLIENT_PASSWORD"] == "" {
		t.Errorf("credential file = %v (want id=u-5 [the username, not id 5], ns=my-ns, non-empty password)", kv)
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
	// adopt emits the username + namespace + the ADOPTED marker, but NO password
	// (the existing one stands; it's write-only on the backend). Same invariant as
	// the mint path: TRACEBLOC_CLIENT_ID is the UUID username ("u-8"), not id 8 —
	// it's the login username the adopted client reconnects with.
	if kv["TRACEBLOC_CLIENT_ID"] != "u-8" || kv["TB_NAMESPACE"] != "ex-ns" || kv["TRACEBLOC_CLIENT_ADOPTED"] != "1" {
		t.Errorf("adopt credential file = %v (want id=u-8 [the username, not id 8], ns=ex-ns, ADOPTED=1)", kv)
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

// TestClientCreate_ReRunReviewShowsAdoptedNamespace pins that on an idempotent
// re-run, the client already anchored to this cluster is excluded from collision
// detection — so the review shows the namespace that's actually adopted
// (lab-one), not a bumped lab-one-2.
func TestClientCreate_ReRunReviewShowsAdoptedNamespace(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			// An existing client already anchored to THIS cluster (uid-1).
			_, _ = w.Write([]byte(`[{"id":1,"first_name":"Lab One","username":"u-1","namespace":"lab-one","location":"DE","cluster_id":"uid-1"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			w.WriteHeader(http.StatusOK) // adopt
			_, _ = w.Write([]byte(`{"id":1,"first_name":"Lab One","username":"u-1","namespace":"lab-one","location":"DE","cluster_id":"uid-1"}`))
		}
	})
	stubClusterID(t, "uid-1", nil)
	confirmYes := true
	pr := &fakePrompter{confirm: &confirmYes}
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), pr,
		clientCreateOpts{name: "Lab One", location: "DE"}); err != nil {
		t.Fatalf("re-run create: %v", err)
	}
	if strings.Contains(out.String(), "lab-one-2") {
		t.Errorf("review showed a bumped namespace — the cluster's own client wasn't excluded from collision detection:\n%s", out.String())
	}
}

// TestClientCreate_AutoNameNoLocation is the cli#137 headline acceptance case:
// a non-interactive create with NO name and NO location flags still succeeds —
// the name is auto-generated from the signed-in identity and no location is sent.
func TestClientCreate_AutoNameNoLocation(t *testing.T) {
	var body api.CreateClientRequest
	rawBody := ""
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[]`)) // no existing clients on the account
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			b, _ := io.ReadAll(r.Body)
			rawBody = string(b)
			_ = json.Unmarshal(b, &body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":5,"first_name":"lukas-01","username":"u-5","namespace":"lukas-01"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	signInAs(t, "Lukas", "lukas@tracebloc.io")

	// pr == nil → non-interactive (the installer path). No flags at all.
	if err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil,
		clientCreateOpts{yes: true}); err != nil {
		t.Fatalf("zero-flag non-interactive create should succeed, got: %v", err)
	}
	if body.Name != "lukas-01" || body.Namespace != "lukas-01" {
		t.Errorf("auto-name = %+v, want name/namespace lukas-01", body)
	}
	// "no location sent" must mean the key is absent from the JSON (omitempty),
	// not just an empty string — the backend distinguishes unset from blank.
	if strings.Contains(rawBody, "location") {
		t.Errorf("request body carried a location key, want it omitted: %s", rawBody)
	}
}

// TestClientCreate_AutoNameNumbering: a second machine on the same account with
// the same first name numbers up (lukas-02), rather than stacking a slug -2 bump.
func TestClientCreate_AutoNameNumbering(t *testing.T) {
	var body api.CreateClientRequest
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			// lukas-01 already exists on the account.
			_, _ = w.Write([]byte(`[{"id":1,"first_name":"lukas-01","username":"u-1","namespace":"lukas-01"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":2,"first_name":"lukas-02","username":"u-2","namespace":"lukas-02"}`))
		}
	})
	signInAs(t, "Lukas", "lukas@tracebloc.io")
	if err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil,
		clientCreateOpts{yes: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if body.Name != "lukas-02" || body.Namespace != "lukas-02" {
		t.Errorf("second machine = %+v, want lukas-02 (numbered, not a slug -2 bump)", body)
	}
}

// TestClientCreate_AutoNameEmailFallback: with no first name on the profile, the
// auto-name base falls back to the email local-part.
func TestClientCreate_AutoNameEmailFallback(t *testing.T) {
	var body api.CreateClientRequest
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":7,"first_name":"jane-doe-01","username":"u-7","namespace":"jane-doe-01"}`))
		}
	})
	signInAs(t, "", "jane.doe@tracebloc.io") // no first name → local-part "jane.doe" → slug "jane-doe"
	if err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil,
		clientCreateOpts{yes: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if body.Name != "jane-doe-01" {
		t.Errorf("auto-name = %q, want jane-doe-01 (email local-part fallback)", body.Name)
	}
}

// TestClientCreate_FlagsStillHonored: explicit --name/--location are passed
// through verbatim and suppress the auto-name.
func TestClientCreate_FlagsStillHonored(t *testing.T) {
	var body api.CreateClientRequest
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":4,"first_name":"lab-one","username":"u-4","namespace":"lab-one","location":"US"}`))
		}
	})
	signInAs(t, "Lukas", "lukas@tracebloc.io") // would auto-name lukas-01 if not overridden
	if err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil,
		clientCreateOpts{name: "Lab One", location: "US", yes: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if body.Name != "Lab One" || body.Location != "US" {
		t.Errorf("create body = %+v, want name 'Lab One' location US (flags verbatim)", body)
	}
}

// TestClientCreate_NonInteractiveNeedsConsent (review #1): a bare non-interactive
// run (no TTY, no --yes, no --credential-file) must NOT silently mint and print
// the credential to stdout — it fails closed with guidance before any POST.
func TestClientCreate_NonInteractiveNeedsConsent(t *testing.T) {
	posted := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			posted = true
		}
	})
	signInAs(t, "Lukas", "lukas@tracebloc.io")
	// pr == nil (non-interactive), yes == false, no credential file.
	err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil, clientCreateOpts{})
	if code := ExitCodeFromError(err); code != 1 {
		t.Fatalf("want exit 1, got %d (err=%v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "refusing to provision non-interactively") {
		t.Errorf("want a consent-required error, got: %v", err)
	}
	if posted {
		t.Error("no client should be minted without --yes/--credential-file")
	}
}

// TestClientCreate_NonInteractiveWithYes: --yes alone is sufficient consent for a
// non-interactive mint (the confirm can't run, but the user opted in).
func TestClientCreate_NonInteractiveWithYes(t *testing.T) {
	posted := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			posted = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":5,"first_name":"lukas-01","username":"u-5","namespace":"lukas-01"}`))
		}
	})
	signInAs(t, "Lukas", "lukas@tracebloc.io")
	if err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil, clientCreateOpts{yes: true}); err != nil {
		t.Fatalf("--yes should authorize a non-interactive mint, got: %v", err)
	}
	if !posted {
		t.Error("expected a mint with --yes")
	}
}

// TestClientCreate_AutoNameFailsClosedOnListError (review #2): if the account's
// client list can't be read, auto-naming would number against an empty set and
// mint a deterministic duplicate. It must fail closed instead — never POST.
func TestClientCreate_AutoNameFailsClosedOnListError(t *testing.T) {
	posted := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			w.WriteHeader(http.StatusBadGateway) // transient list failure
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			posted = true
		}
	})
	signInAs(t, "Lukas", "lukas@tracebloc.io")
	err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil, clientCreateOpts{yes: true})
	if code := ExitCodeFromError(err); code != 1 {
		t.Fatalf("want exit 1 on list failure, got %d (err=%v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "unique client name") {
		t.Errorf("want a 'couldn't pick a unique name' error, got: %v", err)
	}
	if posted {
		t.Error("must not mint (a duplicate) when the client list is unreadable")
	}
}

// TestClientCreate_AutoNameFailsClosedOnListError still lets an explicit --name
// through a list blip (the list is only best-effort for slug-collision avoidance).
func TestClientCreate_ExplicitNameToleratesListError(t *testing.T) {
	posted := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			w.WriteHeader(http.StatusBadGateway)
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			posted = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":5,"first_name":"lab","username":"u-5","namespace":"lab"}`))
		}
	})
	signInAs(t, "Lukas", "lukas@tracebloc.io")
	if err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil,
		clientCreateOpts{name: "lab", yes: true}); err != nil {
		t.Fatalf("explicit --name should tolerate a list blip, got: %v", err)
	}
	if !posted {
		t.Error("expected a mint with an explicit --name despite the list error")
	}
}

// TestClientCreate_AutoNameCapsAt63 (review #4): a very long first name must still
// produce name == namespace within the 63-char DNS label cap — no slug -NN bump.
func TestClientCreate_AutoNameCapsAt63(t *testing.T) {
	var body api.CreateClientRequest
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":5,"first_name":"x","username":"u-5","namespace":"x"}`))
		}
	})
	signInAs(t, strings.Repeat("a", 70), "long@tracebloc.io") // slugifies to 63 a's
	if err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil, clientCreateOpts{yes: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if body.Name != body.Namespace {
		t.Errorf("name != namespace for a long first name: %q vs %q", body.Name, body.Namespace)
	}
	if len(body.Name) > 63 {
		t.Errorf("name exceeds the 63-char DNS label cap: %d chars (%q)", len(body.Name), body.Name)
	}
	if !strings.HasSuffix(body.Name, "-01") {
		t.Errorf("expected a -01 suffix, got %q", body.Name)
	}
}

// TestClientCreate_AutoNameSurfacesUpgradeRequired (Bugbot #144-A): a 426 while
// listing clients for the auto-name must surface as an upgrade signal, not a
// "retry" reachability error — retrying an outdated CLI never succeeds.
func TestClientCreate_AutoNameSurfacesUpgradeRequired(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/edge-device/" {
			w.WriteHeader(http.StatusUpgradeRequired) // 426
			_, _ = w.Write([]byte(`{"error":"upgrade_required","min_version":"1.2.3"}`))
		}
	})
	signInAs(t, "Lukas", "lukas@tracebloc.io")
	err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil, clientCreateOpts{yes: true})
	if code := ExitCodeFromError(err); code != 1 {
		t.Fatalf("want exit 1, got %d (err=%v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("want an upgrade-required message, got: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "couldn't reach the backend") {
		t.Errorf("a 426 must not be framed as a transient reachability error: %v", err)
	}
}

// TestClientCreate_AutoNameReservesSluggedNames (Bugbot #144-B): a legacy client
// whose display name slugifies to the same handle (e.g. "Lukas 01" → lukas-01),
// even with a blank namespace, must reserve that handle so auto-naming skips it.
func TestClientCreate_AutoNameReservesSluggedNames(t *testing.T) {
	var body api.CreateClientRequest
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			// Legacy client: raw display name, no namespace stored.
			_, _ = w.Write([]byte(`[{"id":1,"first_name":"Lukas 01","username":"u-1","namespace":""}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/edge-device/":
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":2,"first_name":"lukas-02","username":"u-2","namespace":"lukas-02"}`))
		}
	})
	signInAs(t, "Lukas", "lukas@tracebloc.io")
	if err := runClientCreate(context.Background(), ui.New(&bytes.Buffer{}), nil, clientCreateOpts{yes: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if body.Name != "lukas-02" {
		t.Errorf("auto-name = %q, want lukas-02 (lukas-01 reserved by legacy \"Lukas 01\")", body.Name)
	}
}
