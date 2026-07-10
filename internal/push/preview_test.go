package push

import (
	"os"
	"path/filepath"
	"strings"
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

	t.Run("multi-csv dir is ambiguous (walk rejects >1 csv)", func(t *testing.T) {
		// DiscoverTabular's findSingleCSV requires exactly one CSV; a dir with
		// two would fail the walk, so the sniff must not confidently place it
		// as tabular (it would echo "this is tabular data" then the walk would
		// reject). Mirrors the walk's exactly-one rule.
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "a.csv"), "x\n1\n")
		writePrev(t, filepath.Join(dir, "b.csv"), "y\n2\n")
		if s := SniffFamily(dir); s.Confident {
			t.Fatalf("a multi-csv dir should be ambiguous, got %+v", s)
		}
		// And the walk it mirrors does reject it.
		if _, err := DiscoverTabular(dir); err == nil {
			t.Fatal("DiscoverTabular should reject a multi-csv dir")
		}
	})

	t.Run("bare .csv file is confident tabular (walk now accepts it)", func(t *testing.T) {
		// DiscoverTabular now stages a bare .csv as the one CSV under the
		// dataset (cli#181), so the sniff confidently places a lone .csv as
		// tabular — mirroring the shape the walk accepts.
		dir := t.TempDir()
		csv := filepath.Join(dir, "t.csv")
		writePrev(t, csv, "a,b\n1,2\n")
		s := SniffFamily(csv)
		if !s.Confident || s.Family != FamilyTabular {
			t.Fatalf("a bare .csv file should sniff confident tabular, got %+v", s)
		}
		// And the walk it mirrors accepts the same bare file.
		if _, err := DiscoverTabular(csv); err != nil {
			t.Fatalf("DiscoverTabular should accept a bare .csv: %v", err)
		}
	})

	t.Run("bare non-.csv file is ambiguous (media families need a folder)", func(t *testing.T) {
		dir := t.TempDir()
		txt := filepath.Join(dir, "notes.txt")
		writePrev(t, txt, "hello")
		if s := SniffFamily(txt); s.Confident {
			t.Fatalf("a bare non-.csv file should be ambiguous, got %+v", s)
		}
	})

	t.Run("symlinked .csv is ambiguous, matching the walk's symlink rejection", func(t *testing.T) {
		// DiscoverTabular rejects a symlinked CSV (rejectSymlink), so the
		// sniff must not confidently promise tabular for one — otherwise the
		// guided flow locks to tabular, then hard-fails on the walk. Sniff and
		// walk must agree: both refuse. (cli#202 review)
		dir := t.TempDir()
		real := filepath.Join(dir, "real.csv")
		writePrev(t, real, "a,b\n1,2\n")
		link := filepath.Join(dir, "link.csv")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink unsupported on this platform: %v", err)
		}
		if s := SniffFamily(link); s.Confident {
			t.Fatalf("a symlinked .csv should be ambiguous (walk rejects it), got %+v", s)
		}
		// And the walk it mirrors does reject the same symlinked file.
		if _, err := DiscoverTabular(link); err == nil {
			t.Fatalf("DiscoverTabular should reject a symlinked .csv")
		}
	})

	// The mis-cased-media footgun (#203): labels.csv next to a media folder
	// whose name matches a marker case-insensitively but not exactly. The
	// walk keys on the literal lowercase name, so it won't see the folder;
	// without the guard the lone labels.csv falls through to confident
	// tabular and the media is silently ingested away as a table. Each
	// mis-cased marker (Images/, Texts/, Sequences/) must stay ambiguous —
	// NOT confident image AND NOT confident tabular — and carry a hint that
	// names the likely lowercase rename so the flow can guide the fix.
	for _, tc := range []struct{ folder, canonical string }{
		{"Images", "images"},
		{"Texts", "texts"},
		{"Sequences", "sequences"},
	} {
		tc := tc
		t.Run("mis-cased "+tc.folder+"/ + labels.csv is ambiguous with a rename hint", func(t *testing.T) {
			dir := t.TempDir()
			writePrev(t, filepath.Join(dir, "labels.csv"), "id,label\n1,c\n")
			if err := os.Mkdir(filepath.Join(dir, tc.folder), 0o755); err != nil {
				t.Fatal(err)
			}
			s := SniffFamily(dir)
			if s.Confident {
				t.Fatalf("mis-cased %s/ + labels.csv must be ambiguous, got %+v", tc.folder, s)
			}
			// Regression: the real footgun is confident TABULAR, not just
			// not-image. Pin that the family is not confidently claimed at all.
			if s.Family == FamilyTabular && s.Confident {
				t.Fatalf("mis-cased %s/ must NOT sniff as confident tabular, got %+v", tc.folder, s)
			}
			if s.Hint == "" {
				t.Fatalf("mis-cased %s/ should carry a rename hint, got %+v", tc.folder, s)
			}
			if !strings.Contains(s.Hint, tc.folder) || !strings.Contains(s.Hint, tc.canonical) {
				t.Fatalf("hint should name both %q and %q, got %q", tc.folder, tc.canonical, s.Hint)
			}
		})
	}

	t.Run("single csv + unrelated subdir stays confident tabular, no hint", func(t *testing.T) {
		// The mis-cased guard must be narrow: a subdir that is NOT a marker
		// name (case-insensitively) — a stray backup/ etc. — must not derail
		// the confident-tabular sniff, since DiscoverTabular ignores it too.
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "data.csv"), "a,b\n1,2\n")
		if err := os.Mkdir(filepath.Join(dir, "backup"), 0o755); err != nil {
			t.Fatal(err)
		}
		s := SniffFamily(dir)
		if !s.Confident || s.Family != FamilyTabular {
			t.Fatalf("single csv + unrelated subdir should stay confident tabular, got %+v", s)
		}
		if s.Hint != "" {
			t.Fatalf("an unrelated subdir must not trigger a mis-cased hint, got %q", s.Hint)
		}
	})

	t.Run("images/ without labels.csv is not confident image", func(t *testing.T) {
		// Discover requires BOTH labels.csv and images/; without labels.csv it
		// errors, so the sniff must not claim confident image on the subdir
		// alone.
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "images"), 0o755); err != nil {
			t.Fatal(err)
		}
		if s := SniffFamily(dir); s.Confident && s.Family == FamilyImage {
			t.Fatalf("images/ without labels.csv must not be confident image, got %+v", s)
		}
	})

	t.Run("texts/ without labels.csv is not confident text", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "texts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if s := SniffFamily(dir); s.Confident && s.Family == FamilyText {
			t.Fatalf("texts/ without labels.csv must not be confident text, got %+v", s)
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

	t.Run("multi-csv errors instead of silently picking the first", func(t *testing.T) {
		// DiscoverTabular rejects a directory with more than one CSV; the
		// preview must mirror that (not silently read the alphabetically-first
		// header) so the caller falls back to free text rather than offering
		// columns from a CSV the walk will reject.
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "a.csv"), "x\n1\n")
		writePrev(t, filepath.Join(dir, "b.csv"), "y\n2\n")
		if _, err := PreviewLabelHeaders("tabular_classification", dir); err == nil {
			t.Fatal("expected an error for a multi-csv tabular directory")
		}
		// Same rule the walk enforces.
		if _, err := DiscoverTabular(dir); err == nil {
			t.Fatal("DiscoverTabular should also reject a multi-csv directory")
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
