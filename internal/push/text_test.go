package push

import (
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/tracebloc/cli/internal/schema"
)

// mkTextDir builds a text-family dataset dir: labels.csv + a sidecar
// directory (texts/ or sequences/) with two .txt files. withTokenizer
// drops a stray tokenizer.json at the root — DiscoverText must ignore
// it (MLM no longer requires or stages one; see #184 / #805).
func mkTextDir(t *testing.T, sidecar string, withTokenizer bool) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "labels.csv", "filename,label\na.txt,pos\nb.txt,neg\n")
	sub := filepath.Join(dir, sidecar)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if withTokenizer {
		writeFile(t, dir, "tokenizer.json", `{"version":"1.0"}`)
	}
	return dir
}

// TestDiscoverText_BareFileRejected: the text layout is directory-only.
// A bare file (even a .csv) must be rejected with a clear "not a directory"
// error — bare-file support is tabular-only (#181).
func TestDiscoverText_BareFileRejected(t *testing.T) {
	dir := t.TempDir()
	bare := writeFile(t, dir, "labels.csv", "filename,label\na.txt,pos\n")
	_, err := DiscoverText("text_classification", bare)
	if err == nil {
		t.Fatal("DiscoverText(bare file) returned nil error; text layout must require a directory")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error = %q, want it to say the path is not a directory", err)
	}
}

// TestDiscoverText_Classification: text_classification stages
// labels.csv + the texts/ directory, no images, no extra files.
func TestDiscoverText_Classification(t *testing.T) {
	dir := mkTextDir(t, "texts", false)
	layout, err := DiscoverText("text_classification", dir)
	if err != nil {
		t.Fatalf("DiscoverText: %v", err)
	}
	if len(layout.Sidecars["texts"]) != 2 {
		t.Errorf("texts files = %d, want 2", len(layout.Sidecars["texts"]))
	}
	if len(layout.Images) != 0 {
		t.Errorf("Images should be empty for text, got %v", layout.Images)
	}
	if got := layout.FileCount(); got != 3 { // labels.csv + 2 texts
		t.Errorf("FileCount = %d, want 3", got)
	}
}

// TestDiscoverText_MLM_NoTokenizer: masked_language_modeling no longer
// requires a tokenizer.json — the ingestor never read one and #805
// removed the dataset-staged tokenizer. A dataset with just the text
// layout is accepted, and a stray tokenizer.json is ignored (not staged,
// not counted), never an error.
func TestDiscoverText_MLM_NoTokenizer(t *testing.T) {
	layout, err := DiscoverText("masked_language_modeling", mkTextDir(t, "sequences", false))
	if err != nil {
		t.Fatalf("DiscoverText(MLM) without tokenizer.json: %v", err)
	}
	if len(layout.Sidecars["sequences"]) != 2 {
		t.Errorf("sequences files = %d, want 2", len(layout.Sidecars["sequences"]))
	}
	if got := layout.FileCount(); got != 3 { // labels.csv + 2 sequences
		t.Errorf("FileCount = %d, want 3", got)
	}

	// A tokenizer.json left in the directory must be ignored, not staged
	// (and not an error).
	withTok, err := DiscoverText("masked_language_modeling", mkTextDir(t, "sequences", true))
	if err != nil {
		t.Fatalf("DiscoverText(MLM) with a stray tokenizer.json: %v", err)
	}
	if got := withTok.FileCount(); got != 3 { // tokenizer.json ignored
		t.Errorf("FileCount with stray tokenizer.json = %d, want 3 (must be ignored)", got)
	}
}

// TestDiscoverText_MissingSidecarDir: a text dataset without its
// text-file directory is a clear error, not a silent empty stage.
func TestDiscoverText_MissingSidecarDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "labels.csv", "filename,label\na.txt,pos\n")
	if _, err := DiscoverText("text_classification", dir); err == nil {
		t.Error("DiscoverText without texts/ returned nil error")
	}
}

// mkStructuredTextDir builds a text dataset whose texts/ files carry the given
// contents (filename → body). labels.csv lists each file; hasLabel adds a
// label column (supervised tasks) so the CSV mirrors what the ingestor reads.
func mkStructuredTextDir(t *testing.T, hasLabel bool, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	header := "filename\n"
	if hasLabel {
		header = "filename,label\n"
	}
	csv := header
	sub := filepath.Join(dir, "texts")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if hasLabel {
			csv += name + ",x\n"
		} else {
			csv += name + "\n"
		}
		if err := os.WriteFile(filepath.Join(sub, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, dir, "labels.csv", csv)
	return dir
}

// TestDiscoverText_AllPhase4Tasks: each of the 5 newly-wired text tasks
// discovers its texts/ layout and stages labels.csv + the text files, with a
// valid fixture per the layout contract (enforced formats get a well-formed
// record; unenforced ones get raw text).
func TestDiscoverText_AllPhase4Tasks(t *testing.T) {
	cases := []struct {
		category string
		hasLabel bool
		body     string
	}{
		{"token_classification", true, "the\tDET\ncat\tNOUN"},                   // per-token; no enforced record_format
		{"sentence_pair_classification", true, "a rose is red\tit is a flower"}, // text_a<TAB>text_b (enforced)
		{"causal_language_modeling", false, "just some raw pretraining text"},   // unenforced
		{"seq2seq", false, "bonjour le monde\thello world"},                     // source<TAB>target (unenforced)
		{"embeddings", false, "query\tpositive doc\thard negative"},             // anchor<TAB>positive<TAB>negative (enforced)
	}
	for _, tc := range cases {
		t.Run(tc.category, func(t *testing.T) {
			dir := mkStructuredTextDir(t, tc.hasLabel, map[string]string{"a.txt": tc.body})
			layout, err := DiscoverText(tc.category, dir)
			if err != nil {
				t.Fatalf("DiscoverText(%s): %v", tc.category, err)
			}
			if len(layout.Sidecars["texts"]) != 1 {
				t.Errorf("texts files = %d, want 1", len(layout.Sidecars["texts"]))
			}
			if got := layout.FileCount(); got != 2 { // labels.csv + 1 text
				t.Errorf("FileCount = %d, want 2", got)
			}
		})
	}
}

// TestDiscoverText_EnforcedRecordFormat_Reject: the ENFORCED formats
// (sentence_pair_classification, embeddings) reject a malformed .txt at
// discovery, with a message that names the file and the expected shape —
// mirroring the ingestor's TabSeparatedRecordValidator. The UNENFORCED formats
// (seq2seq, causal LM) accept the same raw content, so a mirror must not reject.
func TestDiscoverText_EnforcedRecordFormat_Reject(t *testing.T) {
	// A single field, no tab: malformed for the enforced tasks.
	rawNoTab := map[string]string{"bad.txt": "one blob of prose with no tab"}

	for _, category := range []string{"sentence_pair_classification", "embeddings"} {
		t.Run(category+"_rejects", func(t *testing.T) {
			hasLabel := !SelfSupervisedText(category)
			dir := mkStructuredTextDir(t, hasLabel, rawNoTab)
			_, err := DiscoverText(category, dir)
			if err == nil {
				t.Fatalf("DiscoverText(%s) accepted a malformed record", category)
			}
			// The separator label comes from the contract (sepLabel renders a
			// tab as "<TAB>"), not a hardcoded "tab-separated" literal.
			for _, want := range []string{"bad.txt", "<TAB>-separated fields"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
		})
	}

	// The unenforced tasks accept the very same raw file.
	for _, category := range []string{"seq2seq", "causal_language_modeling"} {
		t.Run(category+"_accepts_raw", func(t *testing.T) {
			dir := mkStructuredTextDir(t, false, rawNoTab)
			if _, err := DiscoverText(category, dir); err != nil {
				t.Errorf("DiscoverText(%s) rejected raw text it should accept: %v", category, err)
			}
		})
	}
}

// TestDiscoverText_EnforcedRecordFormat_IgnoresUnreferenced: the enforced
// record-format check runs only over the files labels.csv references, mirroring
// the ingestor's manifest walk (TabSeparatedRecordValidator iterates the CSV
// rows, not the directory). A stray unreferenced .txt in texts/ — a README, a
// scratch draft with no tab — must NOT fail discovery: the ingestor never opens
// a file no row names, so rejecting it would block a layout the cluster accepts
// (RFC-0002 Principle 6).
func TestDiscoverText_EnforcedRecordFormat_IgnoresUnreferenced(t *testing.T) {
	for _, category := range []string{"sentence_pair_classification", "embeddings"} {
		t.Run(category, func(t *testing.T) {
			hasLabel := !SelfSupervisedText(category)
			// a.txt is a well-formed 2-field record AND is referenced by
			// labels.csv; notes.txt is prose with no tab and is NOT referenced.
			dir := mkStructuredTextDir(t, hasLabel, map[string]string{"a.txt": "left side\tright side"})
			stray := filepath.Join(dir, "texts", "notes.txt")
			if err := os.WriteFile(stray, []byte("just some prose with no tab"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := DiscoverText(category, dir); err != nil {
				t.Errorf("DiscoverText(%s) rejected a dataset with an unreferenced stray .txt "+
					"the ingestor would accept: %v", category, err)
			}
		})
	}
}

// TestDiscoverText_SentencePair_WrongFieldCount: sentence_pair requires exactly
// 2 fields — a 3-field record is rejected, whereas embeddings accepts 2 or 3.
func TestDiscoverText_SentencePair_WrongFieldCount(t *testing.T) {
	three := map[string]string{"a.txt": "one\ttwo\tthree"}

	dir := mkStructuredTextDir(t, true, three)
	if _, err := DiscoverText("sentence_pair_classification", dir); err == nil {
		t.Error("sentence_pair_classification should reject a 3-field record")
	}

	dir2 := mkStructuredTextDir(t, false, three)
	if _, err := DiscoverText("embeddings", dir2); err != nil {
		t.Errorf("embeddings should accept a 3-field triplet: %v", err)
	}
}

// TestDiscoverText_ConfiguredExtension: the ingestor appends the CONFIGURED
// extension (.txt OR .text — file_options.extension), so the enforced check
// must match a manifest value against the file actually on disk, not a
// reconstructed "<value>.txt". A row "a" resolves to texts/a.text when that is
// what exists — a malformed a.text is rejected, a well-formed one passes.
func TestDiscoverText_ConfiguredExtension(t *testing.T) {
	mk := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		writeFile(t, dir, "labels.csv", "filename\na\n") // embeddings: no label; value carries no extension
		sub := filepath.Join(dir, "texts")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "a.text"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	if _, err := DiscoverText("embeddings", mk(t, "one blob no tab")); err == nil {
		t.Fatal("malformed a.text should be rejected via the configured .text extension")
	} else if !strings.Contains(err.Error(), "a.text") {
		t.Errorf("error should name the on-disk file a.text: %v", err)
	}

	if _, err := DiscoverText("embeddings", mk(t, "anchor\tpositive")); err != nil {
		t.Errorf("well-formed a.text rejected: %v", err)
	}
}

// TestDiscoverText_MissingFilenameColumn: the ingestor's validator rejects a
// manifest with no filename column ("Missing required column: filename"); the
// enforced check surfaces that locally instead of validating nothing (which
// would fail-open post-upload).
func TestDiscoverText_MissingFilenameColumn(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "labels.csv", "id\nrow1\n") // no filename column
	sub := filepath.Join(dir, "texts")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("anchor\tpositive"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := DiscoverText("embeddings", dir)
	if err == nil {
		t.Fatal("a manifest with no filename column should be rejected locally")
	}
	if !strings.Contains(err.Error(), "filename") {
		t.Errorf("error should name the missing filename column: %v", err)
	}
}

// TestDiscoverText_CaseMismatchedBasename: a manifest value "A.txt" must resolve
// to the on-disk a.txt case-insensitively — otherwise the file goes unchecked
// (fail-open). The malformed a.txt is therefore still rejected.
func TestDiscoverText_CaseMismatchedBasename(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "labels.csv", "filename,label\nA.txt,x\n")
	sub := filepath.Join(dir, "texts")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("one blob no tab"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverText("sentence_pair_classification", dir); err == nil {
		t.Fatal("case-mismatched manifest value A.txt should still match a.txt and reject the malformed file")
	}
}

// TestDiscoverText_TolerantManifestRow: a row Go's strict csv.Reader rejects but
// pandas tolerates (an unescaped quote) must still be read — LazyQuotes — so its
// filename is validated, not silently dropped. Here the tolerated row names a
// malformed a.txt, which must be rejected; without LazyQuotes the row (and its
// file) would be skipped and discovery would fail-open.
func TestDiscoverText_TolerantManifestRow(t *testing.T) {
	dir := t.TempDir()
	// The unescaped " in the label field trips Go's strict reader; pandas reads it.
	writeFile(t, dir, "labels.csv", "filename,label\na.txt,he said \"hi\"\n")
	sub := filepath.Join(dir, "texts")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("one blob no tab"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverText("sentence_pair_classification", dir); err == nil {
		t.Fatal("a pandas-tolerable row must be read (LazyQuotes) so its file is validated")
	}
}

// TestBuild_Text_PassesSchema: the text Build branch emits the right
// sidecar field (texts vs sequences), a label for text_classification
// but NOT for masked_language_modeling, never an images field, and a
// schema-valid spec.
func TestBuild_Text_PassesSchema(t *testing.T) {
	v, err := schema.NewV1Validator()
	if err != nil {
		t.Fatalf("NewV1Validator: %v", err)
	}
	check := func(name string, a SpecArgs, wantSidecar string, wantLabel bool) {
		t.Run(name, func(t *testing.T) {
			spec := a.Build()
			if _, ok := spec[wantSidecar]; !ok {
				t.Errorf("spec missing %q field: %v", wantSidecar, spec)
			}
			if _, hasImages := spec["images"]; hasImages {
				t.Errorf("text spec emitted an images field: %v", spec)
			}
			if _, hasLabel := spec["label"]; hasLabel != wantLabel {
				t.Errorf("label present = %v, want %v (%v)", hasLabel, wantLabel, spec)
			}
			b, err := yaml.Marshal(spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			_, errs, parseErr := v.ValidateYAML(b)
			if parseErr != nil {
				t.Fatalf("parse: %v\n%s", parseErr, b)
			}
			if len(errs) != 0 {
				t.Fatalf("schema validation failed: %s\n%s", schema.FormatErrors(errs), b)
			}
		})
	}

	check("text_classification", SpecArgs{
		Table: "t_txt", Category: "text_classification", Intent: "train", LabelColumn: "label",
	}, "texts", true)

	check("masked_language_modeling", SpecArgs{
		Table: "t_mlm", Category: "masked_language_modeling", Intent: "train",
	}, "sequences", false)

	// Phase 4: supervised text tasks emit a label under texts/; the
	// self-supervised ones emit none. Each must still be schema-valid.
	check("token_classification", SpecArgs{
		Table: "t_tok", Category: "token_classification", Intent: "train", LabelColumn: "label",
	}, "texts", true)

	check("sentence_pair_classification", SpecArgs{
		Table: "t_sp", Category: "sentence_pair_classification", Intent: "train", LabelColumn: "label",
	}, "texts", true)

	check("causal_language_modeling", SpecArgs{
		Table: "t_clm", Category: "causal_language_modeling", Intent: "train",
	}, "texts", false)

	check("seq2seq", SpecArgs{
		Table: "t_s2s", Category: "seq2seq", Intent: "train",
	}, "texts", false)

	check("embeddings", SpecArgs{
		Table: "t_emb", Category: "embeddings", Intent: "train",
	}, "texts", false)
}

// failAfterReader yields its data once, then returns err on every subsequent
// Read — simulating labels.csv failing to read partway through (the header
// parses, the body read then errors).
type failAfterReader struct {
	data []byte
	err  error
	done bool
}

func (f *failAfterReader) Read(p []byte) (int, error) {
	if !f.done {
		f.done = true
		return copy(p, f.data), nil
	}
	return 0, f.err
}

// TestReferencedTextNames_ReadErrorFailsClosed: a mid-stream read error on
// labels.csv must abort the manifest walk (fail closed) rather than silently
// return a partial referenced set that leaves the unread rows' text files
// unvalidated — the image mirror-check (CrossCheckLabels) aborts on the same
// error, and the enforced-text path must match. The trigger is I/O, not
// malformed CSV: LazyQuotes + FieldsPerRecord=-1 parse every bad shape cleanly
// (like pandas), so the only way into this branch is a genuine read failure.
func TestReferencedTextNames_ReadErrorFailsClosed(t *testing.T) {
	sentinel := errors.New("disk gave up mid-read")
	r := csv.NewReader(&failAfterReader{data: []byte("filename,label\n"), err: sentinel})
	r.FieldsPerRecord = -1 // match openCSVReader

	if _, err := referencedTextNames(r); err == nil {
		t.Fatal("referencedTextNames returned nil error on a mid-stream read failure; " +
			"the manifest walk must fail closed")
	} else if !errors.Is(err, sentinel) {
		t.Errorf("error should wrap the underlying read failure, got: %v", err)
	}
}
