package push

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestParseMinSize pins the --min-size entry point (#183 / data-ingestors #348).
// It shares parseWxH with --target-size, so the grammar is the same; what this
// guards is that the flag is wired AND that errors surface the "min size" kind
// verbatim (not "target size"), so the user sees the flag they actually passed.
func TestParseMinSize(t *testing.T) {
	cases := []struct {
		in      string
		w, h    int
		wantErr bool
	}{
		{"512x512", 512, 512, false},
		{"64,64", 64, 64, false},
		{"1x1", 1, 1, false},
		{"512", 0, 0, true},    // no separator
		{"0x512", 0, 0, true},  // non-positive
		{"-4x512", 0, 0, true}, // negative
		{"axb", 0, 0, true},    // non-numeric
		{"1x2x3", 0, 0, true},  // too many parts
		{"", 0, 0, true},       // empty
		// Floats are rejected, mirroring the di#365 value contract: the
		// ingestor's validator rejects a non-integral float at construction,
		// and even the integer-valued float it would coerce ("32.0") is
		// rejected here — the flag grammar is integers only, so the emitted
		// spec.file_options.min_size can never carry a float at all.
		{"16.5x32", 0, 0, true}, // non-integral float
		{"32.0x32", 0, 0, true}, // integer-valued float — still not flag grammar
	}
	for _, c := range cases {
		w, h, err := ParseMinSize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseMinSize(%q) = (%d,%d,nil), want error", c.in, w, h)
				continue
			}
			if !strings.Contains(err.Error(), "min size") {
				t.Errorf("ParseMinSize(%q) error %q must name the \"min size\" flag, not target size", c.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMinSize(%q) unexpected error: %v", c.in, err)
		} else if w != c.w || h != c.h {
			t.Errorf("ParseMinSize(%q) = (%d,%d), want (%d,%d)", c.in, w, h, c.w, c.h)
		}
	}
}

// TestLabelSchemaType covers the case/whitespace-insensitive fallback that the
// merged profile left at 28.6% — the branch that mirrors the ingestor's
// LabelDiversityValidator._schema_type_for. A regression here silently
// mis-resolves a case-mismatched label column's SQL type, which mis-collapses a
// VARCHAR label and false-rejects a diverse dataset the cluster would accept.
func TestLabelSchemaType(t *testing.T) {
	schema := map[string]string{"Label": "VARCHAR(50)", " Score ": "INT"}
	cases := []struct {
		name, col, wantType string
		wantOK              bool
	}{
		{"exact match", "Label", "VARCHAR(50)", true},
		{"case-insensitive fallback", "label", "VARCHAR(50)", true},
		{"upper-case fallback", "LABEL", "VARCHAR(50)", true},
		{"whitespace-insensitive fallback", "Score", "INT", true},
		{"case+whitespace fallback", "score", "INT", true},
		{"not a schema column", "missing", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := labelSchemaType(schema, c.col)
			if ok != c.wantOK || got != c.wantType {
				t.Fatalf("labelSchemaType(_, %q) = (%q,%v), want (%q,%v)", c.col, got, ok, c.wantType, c.wantOK)
			}
		})
	}
}

// TestCheckSchemaColumns covers the missing-column rejection (preflight.go:735)
// that no tabular case fired — every schema column must appear in the header,
// stripped but case-SENSITIVE, matching DataValidator's set-difference probe.
func TestCheckSchemaColumns(t *testing.T) {
	t.Run("all present -> nil", func(t *testing.T) {
		err := CheckSchemaColumns([]string{"id", " label "}, map[string]string{"id": "INT", "label": "TEXT"}, "data.csv")
		if err != nil {
			t.Fatalf("all columns present, want nil, got %v", err)
		}
	})
	t.Run("missing columns -> error names them sorted + the csv", func(t *testing.T) {
		err := CheckSchemaColumns([]string{"id"}, map[string]string{"id": "INT", "zed": "INT", "amp": "INT"}, "train.csv")
		if err == nil {
			t.Fatal("schema names columns absent from the header, want error")
		}
		msg := err.Error()
		for _, want := range []string{"amp, zed", "train.csv"} { // sorted, and names the file
			if !strings.Contains(msg, want) {
				t.Errorf("error %q must contain %q", msg, want)
			}
		}
	})
	t.Run("case-sensitive: header cases differently -> missing", func(t *testing.T) {
		err := CheckSchemaColumns([]string{"Label"}, map[string]string{"label": "TEXT"}, "d.csv")
		if err == nil {
			t.Fatal("column present only in a different case must count as missing (case-sensitive)")
		}
	})
}

// TestTruncateList covers the "… and N more" truncation arithmetic (66.7%) that
// backs every offender list (ValidateImages, CheckAnnotationPairing, TSC).
func TestTruncateList(t *testing.T) {
	cases := []struct {
		name  string
		items []string
		max   int
		want  string
	}{
		{"under the cap joins all", []string{"a", "b"}, 5, "a, b"},
		{"exactly at the cap joins all", []string{"a", "b", "c"}, 3, "a, b, c"},
		{"over the cap truncates with count", []string{"a", "b", "c", "d", "e"}, 3, "a, b, c, … and 2 more"},
		{"empty", nil, 3, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TruncateList(c.items, c.max); got != c.want {
				t.Fatalf("TruncateList(%v, %d) = %q, want %q", c.items, c.max, got, c.want)
			}
		})
	}
}

// TestDiscoverSidecarFiles_SecurityGuards pins the symlink/not-a-dir rejects on
// the shared sidecar walker (text.go:256), which was 0% at the security-guard
// level. This walker backs annotations/ today and masks/ (semantic_segmentation)
// next, so a regression is an arbitrary-local-file-disclosure hole. Mirrors the
// images/ symlink regression pins in walk_test.go.
func TestDiscoverSidecarFiles_SecurityGuards(t *testing.T) {
	exts := map[string]struct{}{".txt": {}}

	t.Run("happy path returns files + total size", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "masks")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		files, total, err := discoverSidecarFiles(root, "masks", exts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 1 || total != 5 {
			t.Fatalf("got files=%v total=%d, want 1 file / 5 bytes", files, total)
		}
	})

	t.Run("missing subdirectory -> error", func(t *testing.T) {
		_, _, err := discoverSidecarFiles(t.TempDir(), "masks", exts)
		if err == nil || !strings.Contains(err.Error(), "missing masks/") {
			t.Fatalf("want a missing-subdirectory error, got %v", err)
		}
	})

	t.Run("a file where the subdirectory should be -> error", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "masks"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := discoverSidecarFiles(root, "masks", exts); err == nil {
			t.Fatal("a regular file named masks must be rejected, not walked")
		}
	})

	if runtime.GOOS == "windows" {
		t.Skip("symlinks require admin privileges on Windows")
	}

	t.Run("symlinked subdirectory -> rejected", func(t *testing.T) {
		root := t.TempDir()
		real := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.Mkdir(real, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(root, "masks")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := discoverSidecarFiles(root, "masks", exts); err == nil {
			t.Fatal("a symlinked sidecar directory must be rejected (file-disclosure guard)")
		}
	})

	t.Run("symlinked entry inside -> rejected", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "masks")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		secret := filepath.Join(t.TempDir(), "secret.txt")
		if err := os.WriteFile(secret, []byte("private"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, filepath.Join(dir, "leak.txt")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := discoverSidecarFiles(root, "masks", exts); err == nil {
			t.Fatal("a symlinked entry must be rejected so it can't exfiltrate a file outside the tree")
		}
	})
}

// TestStderrSuffix covers the remote-stderr parenthetical (teardown.go:204, 0%):
// every Teardown/CleanStaging/ListDatasets error path that carries captured
// stderr relies on it, but every existing test has exec succeed (empty stderr).
func TestStderrSuffix(t *testing.T) {
	if got := stderrSuffix(bytes.NewBufferString("   ")); got != "" {
		t.Errorf("whitespace-only stderr must render as empty, got %q", got)
	}
	if got := stderrSuffix(bytes.NewBufferString("")); got != "" {
		t.Errorf("empty stderr must render as empty, got %q", got)
	}
	if got := stderrSuffix(bytes.NewBufferString("  boom: denied\n")); got != " (boom: denied)" {
		t.Errorf("non-empty stderr must render trimmed in parens, got %q", got)
	}
}

// TestFinalDestPrefix covers the path-traversal panic guard (spec.go:449): the
// PVC destination is only ever built from a name that already passed
// ValidateTableName, and an unsafe name must panic rather than construct a path.
func TestFinalDestPrefix(t *testing.T) {
	t.Run("valid table -> path ending in the table", func(t *testing.T) {
		got := FinalDestPrefix("my_table")
		if !strings.HasSuffix(got, "/my_table") {
			t.Fatalf("FinalDestPrefix(my_table) = %q, want a path ending in /my_table", got)
		}
	})
	for _, bad := range []string{"../evil", "a/b", "a b", "1leading_digit", "has-dash", "", "drop;table"} {
		t.Run("unsafe name panics: "+bad, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("FinalDestPrefix(%q) must panic on an unsafe name (traversal guard)", bad)
				}
			}()
			_ = FinalDestPrefix(bad)
		})
	}
}
