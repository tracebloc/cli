package cli

// Tests for draining the installer's spool — backend#2217 option (b).
//
// The risky property here is not "does it deliver" but "does it deliver the RIGHT
// records to the RIGHT backend, and leave everything else alone". These files
// belong to another component, so every test that asserts something was sent also
// asserts what was NOT sent and what survived on disk.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installerEvent mimics what scripts/lib/telemetry.sh writes: the compact
// {resource, attributes} shape, with the environment on the RESOURCE layer.
func installerEvent(env, instance string, exit int) spooledEvent {
	return spooledEvent{
		Resource: map[string]string{
			"service.name":           "installer",
			"tracebloc.component":    "install",
			"deployment.environment": env,
			"service.instance.id":    instance,
		},
		Attributes: map[string]any{
			"event.name":                  "install.run.failed",
			"error.type":                  "preflight",
			"tracebloc.install.exit_code": exit,
		},
	}
}

func writeInstallerSpool(t *testing.T, path string, events ...spooledEvent) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(line)
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// ─────────────────────────────────────────────────── locating the spool files

func TestInstallerSpoolFilesFindsBothShapes(t *testing.T) {
	dataDir := t.TempDir()
	tmpDir := t.TempDir()
	// The predictable data-dir spool.
	dataSpool := filepath.Join(dataDir, "telemetry", "pending.jsonl")
	writeInstallerSpool(t, dataSpool, installerEvent("prod", "a", 1))
	// A pre-log fallback, whose exact name mktemp chose and we cannot predict.
	fallback := filepath.Join(tmpDir, "tracebloc-telemetry-Ab3xY9")
	writeInstallerSpool(t, fallback, installerEvent("prod", "b", 2))
	// A file that is NOT ours, in the same directory.
	if err := os.WriteFile(filepath.Join(tmpDir, "unrelated.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	getenv := func(k string) string {
		switch k {
		case "HOST_DATA_DIR":
			return dataDir
		case "TMPDIR":
			return tmpDir
		}
		return ""
	}
	files := installerSpoolFiles(getenv)

	var sawData, sawFallback, sawUnrelated bool
	for _, f := range files {
		switch {
		case f == dataSpool:
			sawData = true
		case f == fallback:
			sawFallback = true
		case strings.HasSuffix(f, "unrelated.jsonl"):
			sawUnrelated = true
		}
	}
	if !sawData {
		t.Errorf("the data-dir spool was not found; got %v", files)
	}
	if !sawFallback {
		t.Errorf("the mktemp fallback was not found by glob; got %v", files)
	}
	if sawUnrelated {
		t.Errorf("an unrelated file matched the glob; got %v", files)
	}
}

func TestInstallerSpoolFilesDoesNotDuplicateOneDirectory(t *testing.T) {
	dir := t.TempDir()
	writeInstallerSpool(t, filepath.Join(dir, "tracebloc-telemetry-Zz1"), installerEvent("prod", "a", 1))
	// TMPDIR and HOME pointing at the same place must not yield the file twice —
	// a duplicate would send the same install outcome twice in one batch.
	getenv := func(k string) string {
		if k == "TMPDIR" || k == "HOME" {
			return dir
		}
		return ""
	}
	files := installerSpoolFiles(getenv)
	count := 0
	for _, f := range files {
		if strings.Contains(f, "tracebloc-telemetry-Zz1") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the same fallback file appears %d times in %v", count, files)
	}
}

// TestInstallerSpoolFilesFindsTheFallbackOnEveryHomeVariable pins backend#2377.
//
// THE NAMES BELOW ARE WRITTEN DOWN INDEPENDENTLY of installerFallbackDirVars, on
// purpose. A test that iterates the production list and feeds it back in is
// self-consistent and therefore blind: mistype `USERPROFILE` there and the test
// plants the same typo, finds the file, and goes green while Windows stays
// undrainable. These five literals come from the two producers
// (`client/scripts/lib/telemetry.sh` `_telemetry_fallback_dir` and
// `client/scripts/lib/telemetry.ps1` `Get-TelemetryFallbackSpool`), read there
// and copied here, so a drift between the list and the world is a failing test
// rather than agreement with itself.
//
// The Windows three are the regression: before #2377 the search was TMPDIR/HOME
// //tmp, and Windows sets neither of the first two.
func TestInstallerSpoolFilesFindsTheFallbackOnEveryHomeVariable(t *testing.T) {
	for _, envVar := range []string{"TMPDIR", "HOME", "USERPROFILE", "TEMP", "TMP"} {
		t.Run(envVar, func(t *testing.T) {
			dir := t.TempDir()
			// The ps1 twin's exact spelling, suffix included — the bash twin's
			// mktemp name has no suffix, and the glob must cover both.
			spool := filepath.Join(dir, "tracebloc-telemetry-3f9c1a.jsonl")
			writeInstallerSpool(t, spool, installerEvent("prod", "win-run", 1))

			// ONLY this variable is set. Nothing else may stand in for it, which
			// is what makes the assertion about this variable and not about the
			// environment as a whole.
			getenv := func(k string) string {
				if k == envVar {
					return dir
				}
				return ""
			}

			var found bool
			for _, f := range installerSpoolFiles(getenv) {
				if f == spool {
					found = true
				}
			}
			if !found {
				t.Errorf("a fallback spool reachable only through $%s was not found; "+
					"an install failure written before the log exists is undeliverable",
					envVar)
			}
		})
	}
}

// TestInstallerFallbackDirVarsAreAllSearched is the totality half: every name the
// production list DECLARES must actually be read by installerSpoolFiles.
//
// It cannot see a name that is missing from the list — that is what the test
// above is for — but it does catch the opposite failure, a name added to the
// vocabulary and never wired into the loop, which would look like coverage while
// searching nothing.
func TestInstallerFallbackDirVarsAreAllSearched(t *testing.T) {
	if len(installerFallbackDirVars) == 0 {
		t.Fatal("installerFallbackDirVars is empty; the loop below would assert nothing")
	}
	for _, envVar := range installerFallbackDirVars {
		dir := t.TempDir()
		spool := filepath.Join(dir, "tracebloc-telemetry-Qq7")
		writeInstallerSpool(t, spool, installerEvent("prod", "a", 1))
		getenv := func(k string) string {
			if k == envVar {
				return dir
			}
			return ""
		}
		var found bool
		for _, f := range installerSpoolFiles(getenv) {
			if f == spool {
				found = true
			}
		}
		if !found {
			t.Errorf("$%s is declared in installerFallbackDirVars but is never searched", envVar)
		}
	}
}

// A trailing separator must not turn one directory into two candidates. The
// pre-#2377 code trimmed only "/", so this also guards the filepath.Clean that
// replaced it.
func TestInstallerSpoolFilesDedupesATrailingSeparator(t *testing.T) {
	dir := t.TempDir()
	writeInstallerSpool(t, filepath.Join(dir, "tracebloc-telemetry-Ss2"), installerEvent("prod", "a", 1))
	getenv := func(k string) string {
		switch k {
		case "USERPROFILE":
			return dir
		case "TEMP":
			return dir + string(filepath.Separator)
		}
		return ""
	}
	count := 0
	for _, f := range installerSpoolFiles(getenv) {
		if strings.Contains(f, "tracebloc-telemetry-Ss2") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the same fallback file appears %d times; one install outcome would be sent twice", count)
	}
}

// ─────────────────────────────────────────────── filtering by environment

func TestInstallerRecordsOnlyTakesThisEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.jsonl")
	writeInstallerSpool(t, path,
		installerEvent("prod", "prod-run", 1),
		installerEvent("stg", "stg-run", 2),
		installerEvent("prod", "prod-run-2", 3),
	)

	batch := installerRecords([]string{path}, "prod", installerDrainMax)

	if len(batch.events) != 2 {
		t.Fatalf("want the 2 prod records, got %d", len(batch.events))
	}
	for _, ev := range batch.events {
		if got := ev.Resource["deployment.environment"]; got != "prod" {
			t.Errorf("forwarded a %q record while draining prod", got)
		}
	}
	// The stg record must be RETAINED, not dropped: it is deliverable by a later
	// invocation against stg, and discarding another environment's evidence is
	// not this function's call to make.
	keep := batch.remainder[path]
	if len(keep) != 1 || keep[0].Resource["deployment.environment"] != "stg" {
		t.Errorf("the stg record should be left behind for a later stg run; remainder=%v", keep)
	}
}

func TestInstallerRecordsLeavesRecordsWithNoEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.jsonl")
	noEnv := installerEvent("prod", "x", 1)
	delete(noEnv.Resource, "deployment.environment")
	writeInstallerSpool(t, path, noEnv)

	batch := installerRecords([]string{path}, "prod", installerDrainMax)
	if len(batch.events) != 0 {
		t.Errorf("a record with no environment must not be forwarded; got %d", len(batch.events))
	}
	if len(batch.remainder) != 0 {
		t.Errorf("a file that contributed nothing must not be scheduled for rewrite; got %v", batch.remainder)
	}
}

func TestInstallerRecordsRespectsTheDrainCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.jsonl")
	var events []spooledEvent
	for i := 0; i < installerDrainMax+7; i++ {
		events = append(events, installerEvent("prod", "run", i))
	}
	writeInstallerSpool(t, path, events...)

	batch := installerRecords([]string{path}, "prod", installerDrainMax)
	if len(batch.events) != installerDrainMax {
		t.Fatalf("want exactly %d records, got %d", installerDrainMax, len(batch.events))
	}
	// The overflow must survive, or a big installer spool loses records silently.
	if got := len(batch.remainder[path]); got != 7 {
		t.Errorf("want the 7 uncarried records retained, got %d", got)
	}
}

func TestInstallerRecordsSkipsAFileWithNothingForUs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.jsonl")
	writeInstallerSpool(t, path, installerEvent("stg", "s", 1))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	batch := installerRecords([]string{path}, "prod", installerDrainMax)

	// THE PRIMARY ASSERTION IS ON `remainder`, not on the file's bytes. A byte
	// comparison is satisfied by a rewrite that happens to produce identical
	// content — which is exactly what a rewrite of unchanged records does, so the
	// first version of this test passed under the mutation "rewrite files we took
	// nothing from". `remainder` is the actual contract: only files that
	// contributed a forwarded record may appear in it.
	if _, scheduled := batch.remainder[path]; scheduled {
		t.Errorf("a file we took nothing from was scheduled for rewrite; remainder=%v", batch.remainder)
	}

	clearInstallerRecords(batch.remainder)

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file must not be removed when nothing was taken: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("a file we took nothing from lost content:\n before %q\n after  %q", before, after)
	}
}

// ─────────────────────────────────────────────── end to end through deliver

func TestDeliverCarriesTheInstallersRecords(t *testing.T) {
	ownSpool := withTempConfigDir(t, "prod")
	dataDir := t.TempDir()
	t.Setenv("HOST_DATA_DIR", dataDir)
	t.Setenv("TMPDIR", t.TempDir())

	installerSpool := filepath.Join(dataDir, "telemetry", "pending.jsonl")
	writeInstallerSpool(t, installerSpool,
		installerEvent("prod", "installer-prod", 2),
		installerEvent("dev", "installer-dev", 3),
	)

	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	deliver(ownSpool, srv.URL+telemetryIngestPath, "tok", "prod",
		event("cli-run", 0), time.Now())

	if body == "" {
		t.Fatal("nothing was POSTed")
	}
	if !strings.Contains(body, "installer-prod") {
		t.Errorf("the installer's prod record was not carried: %s", body)
	}
	if strings.Contains(body, "installer-dev") {
		t.Errorf("a dev-labelled installer record was sent to the prod endpoint: %s", body)
	}
	if !strings.Contains(body, "cli-run") {
		t.Errorf("the CLI's own event was lost: %s", body)
	}
	// Delivered records are gone; the foreign-env one survives.
	left := readSpool(installerSpool)
	if len(left) != 1 || left[0].Resource["service.instance.id"] != "installer-dev" {
		t.Errorf("after delivery the installer spool should hold only the dev record; got %v", left)
	}
}

func TestDeliverLeavesTheInstallerSpoolAloneOnFailure(t *testing.T) {
	ownSpool := withTempConfigDir(t, "prod")
	dataDir := t.TempDir()
	t.Setenv("HOST_DATA_DIR", dataDir)
	t.Setenv("TMPDIR", t.TempDir())

	installerSpool := filepath.Join(dataDir, "telemetry", "pending.jsonl")
	writeInstallerSpool(t, installerSpool, installerEvent("prod", "installer-prod", 2))
	before, err := os.ReadFile(installerSpool)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Unreachable endpoint.
	deliver(ownSpool, "http://127.0.0.1:1"+telemetryIngestPath, "tok", "prod",
		event("cli-run", 0), time.Now())

	after, err := os.ReadFile(installerSpool)
	if err != nil {
		t.Fatalf("the installer spool must survive a failed delivery: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("a failed delivery modified the installer's spool:\n before %q\n after  %q", before, after)
	}
}

func TestDeliverLeavesTheInstallerSpoolAloneWhenNotSignedIn(t *testing.T) {
	ownSpool := withTempConfigDir(t, "prod")
	dataDir := t.TempDir()
	t.Setenv("HOST_DATA_DIR", dataDir)
	t.Setenv("TMPDIR", t.TempDir())

	installerSpool := filepath.Join(dataDir, "telemetry", "pending.jsonl")
	writeInstallerSpool(t, installerSpool, installerEvent("prod", "installer-prod", 2))
	before, _ := os.ReadFile(installerSpool)

	// No token: nothing is attempted, so nothing of the installer's is consumed.
	deliver(ownSpool, "http://127.0.0.1:1"+telemetryIngestPath, "", "prod",
		event("cli-run", 0), time.Now())

	after, err := os.ReadFile(installerSpool)
	if err != nil {
		t.Fatalf("the installer spool must survive an unauthenticated run: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("an unauthenticated run modified the installer's spool")
	}
}
