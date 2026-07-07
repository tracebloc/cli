package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/nodeboot"
	"github.com/tracebloc/cli/internal/ui"
)

// typedNamePrompter is the confirm double for `tracebloc delete`: Input returns a
// fixed string (the name the user "typed"), so a test drives the typed-name
// confirmation without a terminal. reply == the active client name → confirmed;
// anything else → the offboard cancels.
type typedNamePrompter struct{ reply string }

func (p typedNamePrompter) Input(_, _, _ string, _ func(string) error) (string, error) {
	return p.reply, nil
}
func (typedNamePrompter) Select(_, _ string, _ []string, def string) (string, error) {
	return def, nil
}
func (typedNamePrompter) Confirm(_ string, def bool) (bool, error) { return def, nil }

// fakeNodeboot records the offboard teardown steps in call order and installs
// itself over the delete.go seams (no real k3d/helm/docker). Each step can be
// scripted to fail so a test exercises the best-effort warn paths.
type fakeNodeboot struct {
	calls         []string
	uninstallErr  error
	teardownErr   error
	pruneErr      error
	removedPaths  []string
	removeErr     map[string]error // path → error to return from osRemoveAll
	executable    string
	executableErr error
	// Kubeconfig/context the uninstall seam was handed — so a test can prove the
	// `tracebloc delete` --kubeconfig/--context flags actually reach helm.
	uninstallKubeconfig string
	uninstallContext    string
}

func (f *fakeNodeboot) install(t *testing.T) {
	t.Helper()
	origU, origT, origP := uninstallChart, teardownCluster, pruneImages
	origExe, origRm := osExecutable, osRemoveAll
	uninstallChart = func(_ context.Context, ns, kubeconfig, kubeContext string) error {
		f.calls = append(f.calls, "uninstall:"+ns)
		f.uninstallKubeconfig, f.uninstallContext = kubeconfig, kubeContext
		return f.uninstallErr
	}
	teardownCluster = func(_ context.Context, name string) error {
		f.calls = append(f.calls, "teardown:"+name)
		return f.teardownErr
	}
	pruneImages = func(_ context.Context) error {
		f.calls = append(f.calls, "prune")
		return f.pruneErr
	}
	osExecutable = func() (string, error) {
		if f.executableErr != nil {
			return "", f.executableErr
		}
		return f.executable, nil
	}
	osRemoveAll = func(path string) error {
		f.calls = append(f.calls, "rm:"+path)
		f.removedPaths = append(f.removedPaths, path)
		if f.removeErr != nil {
			return f.removeErr[path]
		}
		return nil
	}
	t.Cleanup(func() {
		uninstallChart, teardownCluster, pruneImages = origU, origT, origP
		osExecutable, osRemoveAll = origExe, origRm
	})
}

// setActiveForDelete writes a signed-in profile with an active client (id + name
// + namespace) into the temp config dir withClientBackend created, so a delete
// test has something to offboard.
func setActiveForDelete(t *testing.T, id, name, ns string) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Current()
	p.ActiveClientID, p.ActiveClientName, p.ActiveClientNamespace = id, name, ns
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

// (a) No --yes on a non-interactive invocation (prompter nil) → refuse, and do
// NOT touch the backend or run any teardown step.
func TestDelete_NonInteractive_NoYes_Refuses(t *testing.T) {
	backendHit := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke") {
			backendHit = true
		}
		// A status lookup (GET) may still fire from the guard — allow it, return empty.
		_, _ = w.Write([]byte(`[]`))
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
	fn.install(t)

	var out bytes.Buffer
	err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{})
	if err == nil {
		t.Fatal("want refusal without --yes on a non-terminal, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to offboard without confirmation") {
		t.Errorf("unexpected error: %v", err)
	}
	if backendHit {
		t.Error("revoke must NOT be called when the offboard is refused")
	}
	if len(fn.calls) != 0 {
		t.Errorf("no teardown step should run on refusal, got: %v", fn.calls)
	}
}

// (b) --yes runs the full sequence: revoke POSTed to /edge-device/<id>/revoke,
// nodeboot fakes called in order, ~/.tracebloc removed, binary removed.
func TestDelete_Yes_FullSequence(t *testing.T) {
	revokePath := ""
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			// Guard's status lookup: report the client OFFLINE so it doesn't block.
			_, _ = w.Write([]byte(`[{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":0}]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke"):
			revokePath = r.URL.Path
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("auth header = %q, want Bearer tok", got)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")

	// HOST_DATA_DIR = the temp config dir withClientBackend set. Confirm it exists
	// so a successful offboard actually removes something.
	dataDir := os.Getenv("TRACEBLOC_CONFIG_DIR")
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("expected temp config dir to exist: %v", err)
	}
	exe := filepath.Join(t.TempDir(), "bin", "tracebloc")
	fn := &fakeNodeboot{executable: exe}
	fn.install(t)

	var out bytes.Buffer
	if err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true}); err != nil {
		t.Fatalf("offboard: %v", err)
	}

	if revokePath != "/edge-device/5/revoke/" {
		t.Errorf("revoke POST path = %q, want /edge-device/5/revoke/", revokePath)
	}

	// nodeboot steps in order: uninstall → teardown → prune, then the data + binary
	// removals. Assert the teardown ordering explicitly.
	order := []string{
		"uninstall:gpu-box-01",
		"teardown:" + nodeboot.ClusterName,
		"prune",
	}
	for i, want := range order {
		if i >= len(fn.calls) || fn.calls[i] != want {
			t.Fatalf("call %d = %q, want %q (all calls: %v)", i, safeIdx(fn.calls, i), want, fn.calls)
		}
	}

	// ~/.tracebloc (the temp data dir) must have been removed…
	assertRemoved(t, fn, dataDir)
	// …and the binary + its `tb` sibling.
	assertRemoved(t, fn, exe)
	assertRemoved(t, fn, filepath.Join(filepath.Dir(exe), "tb"))
}

// (c) --keep-data spares ~/.tracebloc but still uninstalls + removes the binary.
func TestDelete_KeepData_SparesDataDir(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":0}]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke"):
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	dataDir := os.Getenv("TRACEBLOC_CONFIG_DIR")
	exe := filepath.Join(t.TempDir(), "tracebloc")
	fn := &fakeNodeboot{executable: exe}
	fn.install(t)

	var out bytes.Buffer
	if err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true, keepData: true}); err != nil {
		t.Fatalf("offboard: %v", err)
	}

	for _, p := range fn.removedPaths {
		if p == dataDir {
			t.Errorf("--keep-data must NOT remove the data dir %q", dataDir)
		}
	}
	// The software teardown + binary removal still happen.
	assertRemoved(t, fn, exe)
	if !containsCall(fn.calls, "uninstall:gpu-box-01") {
		t.Errorf("--keep-data still uninstalls the release; calls: %v", fn.calls)
	}
	if !strings.Contains(out.String(), "--keep-data") {
		t.Errorf("output should note --keep-data:\n%s", out.String())
	}
	// …but the now-dangling active-client pointer must be cleared even under
	// --keep-data (the credential is revoked; a stale pointer would mislead a
	// later sign-in / reinstall).
	cfg, _ := config.Load()
	if got := cfg.Current().ActiveClientID; got != "" {
		t.Errorf("--keep-data should clear the active-client pointer, got %q", got)
	}
}

// When the ~/.tracebloc wipe fails, the active-client pointer must still be
// cleared (persisted) — otherwise the host looks enrolled under a dead credential.
func TestDelete_WipeFails_StillClearsPointer(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":0}]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke"):
			w.WriteHeader(http.StatusOK)
		}
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	dataDir := os.Getenv("TRACEBLOC_CONFIG_DIR")
	exe := filepath.Join(t.TempDir(), "tracebloc")
	// Make the data-dir wipe fail, but let cfg.Save() (which writes into dataDir)
	// succeed — the fake osRemoveAll only errors for the data dir path.
	fn := &fakeNodeboot{executable: exe, removeErr: map[string]error{dataDir: errors.New("permission denied")}}
	fn.install(t)

	var out bytes.Buffer
	if err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true}); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	cfg, _ := config.Load()
	if got := cfg.Current().ActiveClientID; got != "" {
		t.Errorf("a failed wipe must still clear the active-client pointer, got %q", got)
	}
	if !strings.Contains(out.String(), "Couldn't remove local data") {
		t.Errorf("expected a wipe-failure warning, got:\n%s", out.String())
	}
}

// When a teardown step leaves real state behind, the closing line must NOT claim a
// clean offboard — it should flag that some cleanup didn't complete.
func TestDelete_TeardownFailure_HonestClosing(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":0}]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke"):
			w.WriteHeader(http.StatusOK)
		}
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	fn := &fakeNodeboot{
		executable:   filepath.Join(t.TempDir(), "tracebloc"),
		uninstallErr: errors.New("helm: connection refused"),
	}
	fn.install(t)

	var out bytes.Buffer
	if err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true}); err != nil {
		t.Fatalf("offboard should still return nil (revoke succeeded): %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "some cleanup above didn't complete") {
		t.Errorf("a failed teardown step should produce an honest degraded closing, got:\n%s", s)
	}
	if strings.Contains(s, "no longer connected to tracebloc") {
		t.Errorf("must not print the clean-success closing when a step failed:\n%s", s)
	}
}

// --kubeconfig/--context must reach the helm uninstall — otherwise the release is
// uninstalled against the ambient current-context, which may be the wrong cluster.
func TestDelete_KubeconfigContext_ReachHelm(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":0}]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke"):
			w.WriteHeader(http.StatusOK)
		}
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
	fn.install(t)

	var out bytes.Buffer
	err := runDelete(context.Background(), ui.New(&out), nil,
		deleteOpts{yes: true, kubeconfigPath: "/tmp/kc.yaml", contextOverride: "k3d-tracebloc"})
	if err != nil {
		t.Fatalf("offboard: %v", err)
	}
	if fn.uninstallKubeconfig != "/tmp/kc.yaml" || fn.uninstallContext != "k3d-tracebloc" {
		t.Errorf("uninstall got kubeconfig=%q context=%q, want /tmp/kc.yaml + k3d-tracebloc",
			fn.uninstallKubeconfig, fn.uninstallContext)
	}
}

// (d) A running/online client → refuse unless --force.
func TestDelete_RunningJob_RefusesUnlessForce(t *testing.T) {
	newHandler := func() http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
				// status 1 = online (a running client).
				_, _ = w.Write([]byte(`[{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":1}]`))
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke"):
				w.WriteHeader(http.StatusOK)
			}
		}
	}

	t.Run("online without --force refuses, no revoke", func(t *testing.T) {
		revoked := false
		withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke") {
				revoked = true
			}
			newHandler()(w, r)
		})
		setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
		fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
		fn.install(t)

		var out bytes.Buffer
		err := runDelete(context.Background(), ui.New(&out), typedNamePrompter{reply: "gpu-box-01"}, deleteOpts{})
		if err == nil || !strings.Contains(err.Error(), "still online") {
			t.Fatalf("want online refusal, got %v", err)
		}
		if revoked {
			t.Error("revoke must NOT run when the online guard refuses")
		}
		if len(fn.calls) != 0 {
			t.Errorf("no teardown on refusal, got: %v", fn.calls)
		}
	})

	t.Run("--force offboards an online client", func(t *testing.T) {
		revoked := false
		withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke") {
				revoked = true
			}
			newHandler()(w, r)
		})
		setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
		fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
		fn.install(t)

		var out bytes.Buffer
		if err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true, force: true}); err != nil {
			t.Fatalf("offboard --force: %v", err)
		}
		if !revoked {
			t.Error("--force should proceed to revoke an online client")
		}
	})
}

// (e) The RETAINED + LEFT copy is present in the output (the three-way summary).
func TestDelete_ShowsRetainedAndLeftCopy(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":0}]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke"):
			w.WriteHeader(http.StatusOK)
		}
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
	fn.install(t)

	var out bytes.Buffer
	if err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true}); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"Kept on tracebloc, as a record",
		"Your use cases and the models trained here",
		"Left in place (system-wide)",
		"Docker, kubectl, k3d, helm — remove yourself if unused",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing RETAINED/LEFT copy %q:\n%s", want, s)
		}
	}
}

// Typed-name mismatch cancels cleanly, with no side effects.
func TestDelete_TypedNameMismatch_Cancels(t *testing.T) {
	revoked := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke") {
			revoked = true
		}
		_, _ = w.Write([]byte(`[{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":0}]`))
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
	fn.install(t)

	var out bytes.Buffer
	if err := runDelete(context.Background(), ui.New(&out), typedNamePrompter{reply: "wrong-name"}, deleteOpts{}); err != nil {
		t.Fatalf("a mismatch should cancel cleanly (nil error), got: %v", err)
	}
	if revoked || len(fn.calls) != 0 {
		t.Errorf("a mismatched name must not offboard; revoked=%v calls=%v", revoked, fn.calls)
	}
	if !strings.Contains(out.String(), "didn't match") {
		t.Errorf("expected a cancel note, got:\n%s", out.String())
	}
}

// A brew-managed binary that can't be removed prints the brew hint, not a raw rm.
func TestDelete_BrewManagedBinary_Hint(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":0}]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke"):
			w.WriteHeader(http.StatusOK)
		}
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	brewExe := "/opt/homebrew/Cellar/tracebloc/0.5.0/bin/tracebloc"
	fn := &fakeNodeboot{
		executable: brewExe,
		removeErr:  map[string]error{brewExe: errors.New("permission denied")},
	}
	fn.install(t)

	var out bytes.Buffer
	if err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true}); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	if !strings.Contains(out.String(), "brew uninstall tracebloc") {
		t.Errorf("expected the brew-uninstall hint, got:\n%s", out.String())
	}
}

// An empty namespace (no --namespace, no cached ActiveClientNamespace) must warn
// and skip the uninstall, not skip it silently — the summary promised the release
// would go, so a leftover has to be called out.
func TestDelete_NoNamespace_WarnsAndSkipsUninstall(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":0}]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke"):
			w.WriteHeader(http.StatusOK)
		}
	})
	setActiveForDelete(t, "5", "gpu-box-01", "") // no cached namespace
	fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
	fn.install(t)

	var out bytes.Buffer
	if err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true}); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	for _, c := range fn.calls {
		if strings.HasPrefix(c, "uninstall:") {
			t.Errorf("no namespace → no uninstall attempt, got call %q", c)
		}
	}
	if !strings.Contains(out.String(), "skipped the Helm uninstall") {
		t.Errorf("expected a warn that the uninstall was skipped, got:\n%s", out.String())
	}
}

// A 403 on revoke must speak in offboard terms ("offboarding requires
// CLIENT_WRITE"), not the provisioning copy the shared askAnAdmin used to hardcode.
func TestDelete_RevokeForbidden_OffboardCopy(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/":
			_, _ = w.Write([]byte(`[{"id":5,"first_name":"gpu-box-01","namespace":"gpu-box-01","status":0}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/edge-device/admins/":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"detail":"forbidden"}`))
		}
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
	fn.install(t)

	var out bytes.Buffer
	err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true})
	if err == nil || !strings.Contains(err.Error(), "offboarding requires CLIENT_WRITE permission") {
		t.Fatalf("want an offboard-specific CLIENT_WRITE error, got %v", err)
	}
	if strings.Contains(out.String(), "provision") {
		t.Errorf("offboard 403 copy must not mention provisioning, got:\n%s", out.String())
	}
	// A denied revoke aborts before any teardown.
	if len(fn.calls) != 0 {
		t.Errorf("no teardown after a 403 revoke, got: %v", fn.calls)
	}
}

// looksBrewManaged must see through the symlink: on Intel macOS the PATH binary is
// /usr/local/bin/tracebloc, a link into the Cellar, and os.Executable may hand back
// the unresolved link — the raw path matches no marker but the target does.
func TestLooksBrewManaged_ResolvesSymlink(t *testing.T) {
	root := t.TempDir()
	cellar := filepath.Join(root, "usr", "local", "Cellar", "tracebloc", "0.5.0", "bin")
	if err := os.MkdirAll(cellar, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cellar, "tracebloc")
	if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "tracebloc")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if !looksBrewManaged(link) {
		t.Errorf("symlink %q into a Cellar path should be detected as brew-managed", link)
	}
	if looksBrewManaged(filepath.Join(binDir, "nope")) {
		t.Error("a non-brew, non-existent path should not be flagged")
	}
}

// ── small assertion helpers ──

func assertRemoved(t *testing.T, fn *fakeNodeboot, path string) {
	t.Helper()
	for _, p := range fn.removedPaths {
		if p == path {
			return
		}
	}
	t.Errorf("expected %q to be removed; removed paths: %v", path, fn.removedPaths)
}

func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

func safeIdx(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<none>"
}
