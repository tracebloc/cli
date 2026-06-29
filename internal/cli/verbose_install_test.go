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

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

// loginStub wires a minimal happy-path device flow via the auth_test seam.
func loginStub(t *testing.T) {
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"X","verification_uri":"https://x/activate","interval":5}`))
		case "/device/token":
			_, _ = w.Write([]byte(`{"token":"cat_v"}`))
		case "/userinfo/":
			_, _ = w.Write([]byte(`{"email":"e@co","account":"A"}`))
		}
	})
}

// cli#101 (RFC-0001 §8.5): --verbose streams the device-flow detail; the default
// output stays quiet.

func TestLogin_VerboseStreamsDetail(t *testing.T) {
	loginStub(t)
	out, err := runCmd(t, "--verbose", "login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(out, "requesting a device code") {
		t.Errorf("--verbose should stream the device-flow detail, got:\n%s", out)
	}
}

func TestLogin_QuietByDefault(t *testing.T) {
	loginStub(t)
	out, err := runCmd(t, "login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if strings.Contains(out, "requesting a device code") {
		t.Errorf("default output should stay quiet (no verbose detail), got:\n%s", out)
	}
}

// TestClientCreate_FailurePrintsResumeAndWritesInstallLog pins the §8.5 failure
// path: a failed provision prints the (idempotent) resume command + the doctor
// pointer, and every run leaves an install-<ts>.log on disk.
func TestClientCreate_FailurePrintsResumeAndWritesInstallLog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "tok"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	// List succeeds; the provision POST 500s → create fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	origClient := newAPIClient
	newAPIClient = func(string) *api.Client { return &api.Client{BaseURL: srv.URL, HTTP: srv.Client()} }
	t.Cleanup(func() { newAPIClient = origClient })
	origCID := readClusterID
	readClusterID = func(context.Context, cluster.KubeconfigOptions) (string, error) {
		return "", errors.New("no cluster (test)")
	}
	t.Cleanup(func() { readClusterID = origCID })

	var out bytes.Buffer
	err := runClientCreate(context.Background(), ui.New(&out), nil,
		clientCreateOpts{name: "My Client", location: "DE", yes: true})
	if err == nil {
		t.Fatal("expected the provision to fail (POST 500)")
	}
	// Resume hint: the idempotent re-run command (name with a space gets quoted)
	// + the doctor pointer.
	if !strings.Contains(out.String(), "tracebloc client create --name 'My Client' --location DE") {
		t.Errorf("missing / incorrect resume command:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "cluster doctor") {
		t.Errorf("missing `cluster doctor` pointer:\n%s", out.String())
	}
	// An install-<ts>.log is written, recording the failure.
	logs, _ := filepath.Glob(filepath.Join(dir, "install-*.log"))
	if len(logs) == 0 {
		t.Fatal("no install-*.log written")
	}
	raw, _ := os.ReadFile(logs[0])
	if !strings.Contains(string(raw), "FAILED") {
		t.Errorf("install log should record the failure:\n%s", raw)
	}
}

// TestClientCreate_CancelLogsCancelledNotDone pins the Bugbot fix: declining the
// confirm prompt is a user abort, not a successful provision — the install log
// must record "cancelled", never "done".
func TestClientCreate_CancelLogsCancelledNotDone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "tok"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	origClient := newAPIClient
	newAPIClient = func(string) *api.Client { return &api.Client{BaseURL: srv.URL, HTTP: srv.Client()} }
	t.Cleanup(func() { newAPIClient = origClient })
	origCID := readClusterID
	readClusterID = func(context.Context, cluster.KubeconfigOptions) (string, error) {
		return "", errors.New("no cluster (test)")
	}
	t.Cleanup(func() { readClusterID = origCID })

	confirmNo := false
	pr := &fakePrompter{answers: map[string]string{}, confirm: &confirmNo}
	var out bytes.Buffer
	if err := runClientCreate(context.Background(), ui.New(&out), pr,
		clientCreateOpts{name: "Lab", location: "DE"}); err != nil {
		t.Fatalf("declining the confirm should be a clean exit, got: %v", err)
	}
	if posted {
		t.Error("no client should be POSTed when the user declines")
	}
	logs, _ := filepath.Glob(filepath.Join(dir, "install-*.log"))
	if len(logs) == 0 {
		t.Fatal("no install-*.log written")
	}
	raw, _ := os.ReadFile(logs[0])
	if !strings.Contains(string(raw), "cancelled") {
		t.Errorf("install log should record the cancel, got:\n%s", raw)
	}
	if strings.Contains(string(raw), "done") {
		t.Errorf("a cancelled run must NOT be logged as 'done':\n%s", raw)
	}
}

// TestClientCreate_ResumeCommandIncludesPromptedValues pins the Bugbot fix: when
// name/location come from interactive prompts (not flags), a failed provision's
// resume command must still include them — opts alone would omit them.
func TestClientCreate_ResumeCommandIncludesPromptedValues(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "tok"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	// list ok; the provision POST 500s after the user confirms.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	origClient := newAPIClient
	newAPIClient = func(string) *api.Client { return &api.Client{BaseURL: srv.URL, HTTP: srv.Client()} }
	t.Cleanup(func() { newAPIClient = origClient })
	origCID := readClusterID
	readClusterID = func(context.Context, cluster.KubeconfigOptions) (string, error) {
		return "", errors.New("no cluster (test)")
	}
	t.Cleanup(func() { readClusterID = origCID })

	confirmYes := true
	pr := &fakePrompter{answers: map[string]string{
		"Client name":             "Prompted Lab",
		"Location zone (e.g. DE)": "FR",
	}, confirm: &confirmYes}
	var out bytes.Buffer
	// No name/location flags — both come from the prompts.
	if err := runClientCreate(context.Background(), ui.New(&out), pr, clientCreateOpts{}); err == nil {
		t.Fatal("expected the provision to fail (POST 500)")
	}
	if !strings.Contains(out.String(), "--name 'Prompted Lab' --location FR") {
		t.Errorf("resume command should carry the PROMPTED name + location, got:\n%s", out.String())
	}
}
