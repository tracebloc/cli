package push

import (
	"os"
	"path/filepath"
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
}
