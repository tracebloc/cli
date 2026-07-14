package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTmpYAML drops a small YAML doc into the test's t.TempDir and
// returns the path. Using TempDir guarantees cleanup on test exit;
// callers don't have to defer os.Remove themselves.
func writeTmpYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ingest.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// execIngestValidate drives the full cobra dispatch for
// `tracebloc ingest validate <path>` and returns the exit code, the
// captured stdout, and the captured stderr. Tests should never share
// a *cobra.Command across cases — cobra holds flag state on the
// command tree and stale trees leak one test's args into the next.
func execIngestValidate(t *testing.T, path string) (exitCode int, stdout, stderr string) {
	t.Helper()
	root := NewRootCmd(BuildInfo{Version: "test"})
	var so, se bytes.Buffer
	root.SetOut(&so)
	root.SetErr(&se)
	root.SetArgs([]string{"ingest", "validate", path})

	err := root.Execute()
	return ExitCodeFromError(err), so.String(), se.String()
}

func TestIngestValidate_HappyPath(t *testing.T) {
	path := writeTmpYAML(t, `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: cats_dogs_train
intent: train
category: image_classification
csv: /data/labels.csv
images: /data/images/
label: image_label
`)

	code, stdout, stderr := execIngestValidate(t, path)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "ok") {
		t.Errorf("expected 'ok' on stdout, got: %q", stdout)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr on success, got: %q", stderr)
	}
}

func TestIngestValidate_SchemaFailureExitsTwo(t *testing.T) {
	// Missing required fields → schema violation → exit 2.
	path := writeTmpYAML(t, `
kind: IngestConfig
category: image_classification
table: t
csv: /data/labels.csv
images: /data/images/
label: image_label
`)

	code, _, stderr := execIngestValidate(t, path)
	if code != 2 {
		t.Fatalf("expected exit 2 for schema failure, got %d", code)
	}
	// Errors print to stderr (so success can be piped without
	// interference). Both the count line + the per-error lines
	// should be there.
	for _, want := range []string{"schema validation failed", "apiVersion"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("expected stderr to mention %q, got:\n%s", want, stderr)
		}
	}
}

// TestIngestValidate_BogusSchemaTypeExitsTwo pins cli#213 on the `data
// validate` path: the jsonschema types every schema value as a bare string, so
// a bogus SQL type used to pass validate (false green) and only fail
// in-cluster. `data validate` now previews the ingestor's accepted-type set and
// flags it, anchored at schema.<column>.
func TestIngestValidate_BogusSchemaTypeExitsTwo(t *testing.T) {
	path := writeTmpYAML(t, `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: t
intent: train
category: tabular_classification
csv: /data/data.csv
schema:
  age: INT
  bad: BANANA
label: churned
`)
	code, _, stderr := execIngestValidate(t, path)
	if code != 2 {
		t.Fatalf("expected exit 2 for a bogus schema type, got %d\nstderr:\n%s", code, stderr)
	}
	for _, want := range []string{"schema.bad", "supported SQL type"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("expected stderr to mention %q, got:\n%s", want, stderr)
		}
	}
	// A valid type on the same doc must NOT be flagged.
	if strings.Contains(stderr, "schema.age") {
		t.Errorf("a valid type (age: INT) should not be flagged; stderr:\n%s", stderr)
	}
}

// TestIngestValidate_ValidSchemaTypesOK: a doc whose types are all accepted
// (including a two-arg DECIMAL and the non-inferable DOUBLE) validates clean —
// the type preview must not over-reject.
func TestIngestValidate_ValidSchemaTypesOK(t *testing.T) {
	path := writeTmpYAML(t, `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: t
intent: train
category: tabular_classification
schema:
  age: INT
  price: DECIMAL(10,2)
  ratio: DOUBLE
  name: VARCHAR(255)
csv: /data/data.csv
label: churned
`)
	code, stdout, stderr := execIngestValidate(t, path)
	if code != 0 {
		t.Fatalf("expected exit 0 for all-valid types, got %d\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "ok") {
		t.Errorf("expected 'ok' on stdout, got: %q", stdout)
	}
}

func TestIngestValidate_UnreadableFileExitsThree(t *testing.T) {
	// Distinct exit code (3) for file-level problems (missing,
	// permission-denied, etc.) — separates from schema violations
	// (2) so callers can branch on the cause.
	code, _, _ := execIngestValidate(t, "/tmp/definitely-does-not-exist-"+t.Name())
	if code != 3 {
		t.Errorf("expected exit 3 for missing file, got %d", code)
	}
}

func TestIngestValidate_NonMappingExitsThree(t *testing.T) {
	// A top-level YAML sequence (vs mapping) is a parse-shape
	// problem, not a schema problem — surface it as the file-level
	// exit code so the customer knows their file isn't an
	// ingest-config at all, vs being one with the wrong fields.
	path := writeTmpYAML(t, "- one\n- two\n")

	code, _, _ := execIngestValidate(t, path)
	if code != 3 {
		t.Errorf("expected exit 3 for non-mapping YAML, got %d", code)
	}
}

func TestIngestValidate_RequiresExactlyOneArg(t *testing.T) {
	// Cobra catches arg-count violations before our RunE runs;
	// confirm we exit non-zero (the specific code is cobra's
	// default 1, not our exitError 2/3 — that's intentional).
	root := NewRootCmd(BuildInfo{Version: "test"})
	var so, se bytes.Buffer
	root.SetOut(&so)
	root.SetErr(&se)
	root.SetArgs([]string{"ingest", "validate"}) // no path

	err := root.Execute()
	if err == nil {
		t.Fatalf("expected error from missing path arg, got nil")
	}
	if got := ExitCodeFromError(err); got == 0 {
		t.Errorf("expected non-zero exit, got %d", got)
	}
}

func TestExitCodeFromError(t *testing.T) {
	// Defensive: pin the exitError dispatch behavior since this is
	// the only thing main() depends on from this package.
	if got := ExitCodeFromError(nil); got != 0 {
		t.Errorf("nil err should map to 0, got %d", got)
	}
	if got := ExitCodeFromError(&exitError{code: 7}); got != 7 {
		t.Errorf("explicit exit code should propagate, got %d", got)
	}
	// A wrapped exitError still reports its code (asExitError walks
	// the unwrap chain).
	wrapped := &exitError{code: 9, err: &exitError{code: 0}}
	if got := ExitCodeFromError(wrapped); got != 9 {
		t.Errorf("outermost exitError wins, got %d", got)
	}
}

// IsSilentError is the main()-side hook that decides whether to
// print an "Error: ..." stderr line on the way out. Pin the
// contract so main.go doesn't silently regress to swallowing
// errors (the high-severity bugbot finding that led to this
// being added in the first place).
func TestIsSilentError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{
			"exitError with nil inner (e.g. schema-violation already-printed)",
			&exitError{code: 2, err: nil},
			true,
		},
		{
			"exitError with non-nil inner (e.g. file read failure)",
			&exitError{code: 3, err: ioEOFOrSimilar()},
			false,
		},
		{"plain error from cobra (e.g. unknown command)", errorString("unknown command"), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSilentError(c.err); got != c.want {
				t.Errorf("IsSilentError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// errorString is the simplest possible error implementation, used
// in tests that need a "plain" error without any custom Unwrap
// behavior. Same as the stdlib errors.New() result; defined inline
// to keep the test file self-contained.
type errorString string

func (e errorString) Error() string { return string(e) }

func ioEOFOrSimilar() error { return errorString("read: file does not exist") }
