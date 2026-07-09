package push

import (
	"os"
	"path/filepath"
	"testing"
)

func writePrev(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSniffFamily covers each confident layout + the ambiguous fallbacks.
func TestSniffFamily(t *testing.T) {
	t.Run("tabular dir (single csv)", func(t *testing.T) {
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "data.csv"), "a,b\n1,2\n")
		s := SniffFamily(dir)
		if !s.Confident || s.Family != FamilyTabular {
			t.Fatalf("got %+v, want confident tabular", s)
		}
		if s.Echo == "" {
			t.Error("confident sniff should carry an echo")
		}
	})

	t.Run("tabular file path", func(t *testing.T) {
		dir := t.TempDir()
		csv := filepath.Join(dir, "t.csv")
		writePrev(t, csv, "a,b\n1,2\n")
		s := SniffFamily(csv)
		if !s.Confident || s.Family != FamilyTabular {
			t.Fatalf("got %+v, want confident tabular for a .csv file path", s)
		}
	})

	t.Run("image dir", func(t *testing.T) {
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "labels.csv"), "image_id,label\n1.jpg,c\n")
		if err := os.Mkdir(filepath.Join(dir, "images"), 0o755); err != nil {
			t.Fatal(err)
		}
		s := SniffFamily(dir)
		if !s.Confident || s.Family != FamilyImage {
			t.Fatalf("got %+v, want confident image", s)
		}
	})

	t.Run("text dir (sequences)", func(t *testing.T) {
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "labels.csv"), "text_id,label\n1.txt,c\n")
		if err := os.Mkdir(filepath.Join(dir, "sequences"), 0o755); err != nil {
			t.Fatal(err)
		}
		s := SniffFamily(dir)
		if !s.Confident || s.Family != FamilyText {
			t.Fatalf("got %+v, want confident text", s)
		}
	})

	t.Run("empty dir is ambiguous", func(t *testing.T) {
		if s := SniffFamily(t.TempDir()); s.Confident {
			t.Fatalf("empty dir should be ambiguous, got %+v", s)
		}
	})

	t.Run("image+text mix is ambiguous", func(t *testing.T) {
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "labels.csv"), "x\n1\n")
		_ = os.Mkdir(filepath.Join(dir, "images"), 0o755)
		_ = os.Mkdir(filepath.Join(dir, "texts"), 0o755)
		if s := SniffFamily(dir); s.Confident {
			t.Fatalf("an images/+texts/ mix should be ambiguous, got %+v", s)
		}
	})

	t.Run("missing path is ambiguous", func(t *testing.T) {
		if s := SniffFamily(filepath.Join(t.TempDir(), "nope")); s.Confident {
			t.Fatalf("missing path should be ambiguous, got %+v", s)
		}
	})
}

// TestPreviewLabelHeaders reads the header from the right CSV per family.
func TestPreviewLabelHeaders(t *testing.T) {
	t.Run("tabular reads the single csv", func(t *testing.T) {
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "data.csv"), "age,income,churned\n1,2,yes\n")
		hdr, err := PreviewLabelHeaders("tabular_classification", dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(hdr) != 3 || hdr[2] != "churned" {
			t.Fatalf("headers = %v, want [age income churned]", hdr)
		}
	})

	t.Run("image reads labels.csv", func(t *testing.T) {
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "labels.csv"), "image_id,label\n1.jpg,c\n")
		hdr, err := PreviewLabelHeaders("image_classification", dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(hdr) != 2 || hdr[1] != "label" {
			t.Fatalf("headers = %v, want [image_id label]", hdr)
		}
	})

	t.Run("missing csv errors (caller falls back to free text)", func(t *testing.T) {
		if _, err := PreviewLabelHeaders("tabular_classification", t.TempDir()); err == nil {
			t.Fatal("expected an error when no csv is present")
		}
	})
}

// TestDisplayNameGlosses pins the locked glosses win over the label, and a
// task without a gloss shows its label.
func TestDisplayNameGlosses(t *testing.T) {
	cases := map[string]string{
		"time_to_event_prediction": "Survival analysis",
		"masked_language_modeling": "fill-mask",
		"seq2seq":                  "translation / summarization",
		"image_classification":     "Image classification", // no gloss → label
	}
	for id, want := range cases {
		spec, ok := Lookup(id)
		if !ok {
			t.Fatalf("Lookup(%q) not found", id)
		}
		if got := spec.DisplayName(); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestCategoriesByFamily returns only that family, and every spec carries a
// blurb (so the picker line is never "Display —  · id").
func TestCategoriesByFamily(t *testing.T) {
	for _, fam := range []Family{FamilyImage, FamilyText, FamilyTabular} {
		got := CategoriesByFamily(fam)
		if len(got) == 0 {
			t.Fatalf("family %d has no categories", fam)
		}
		for _, c := range got {
			if c.Family != fam {
				t.Errorf("CategoriesByFamily(%d) returned %q from family %d", fam, c.ID, c.Family)
			}
			if c.Blurb == "" {
				t.Errorf("category %q has no blurb", c.ID)
			}
		}
	}
}

func TestSelfSupervisedText(t *testing.T) {
	for _, id := range []string{"masked_language_modeling", "causal_language_modeling"} {
		if !SelfSupervisedText(id) {
			t.Errorf("%s should be self-supervised", id)
		}
	}
	for _, id := range []string{"text_classification", "tabular_regression", "image_classification"} {
		if SelfSupervisedText(id) {
			t.Errorf("%s should not be self-supervised", id)
		}
	}
}
