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
	// sniff mirrors the walk, which keys on the literal lowercase name via
	// os.Lstat — so behavior is filesystem-dependent, and each subtest asserts
	// the branch that actually applies on the FS it runs on:
	//   - case-SENSITIVE FS (Linux CI): the walk can't see the folder, so the
	//     lone labels.csv would otherwise fall through to confident tabular and
	//     the media be silently ingested away. The sniff must stay ambiguous
	//     (NOT confident — that covers the confident-tabular footgun) and carry
	//     a rename hint naming both the folder and its lowercase form.
	//   - case-INSENSITIVE FS (macOS APFS, Windows): the walk resolves the
	//     mis-cased folder under its lowercase name, so the layout is valid.
	//     The sniff must AGREE — confident media, no false rename hint telling
	//     the user to fix a layout that already works (#203 cross-platform).
	for _, tc := range []struct {
		folder, canonical string
		wantFamily        Family
	}{
		{"Images", "images", FamilyImage},
		{"Texts", "texts", FamilyText},
		{"Sequences", "sequences", FamilyText},
	} {
		tc := tc
		t.Run("mis-cased "+tc.folder+"/ + labels.csv tracks the walk", func(t *testing.T) {
			dir := t.TempDir()
			writePrev(t, filepath.Join(dir, "labels.csv"), "id,label\n1,c\n")
			if err := os.Mkdir(filepath.Join(dir, tc.folder), 0o755); err != nil {
				t.Fatal(err)
			}
			// The walk's own probe: does the literal lowercase marker resolve?
			fi, err := os.Lstat(filepath.Join(dir, tc.canonical))
			walkSeesIt := err == nil && fi.IsDir()

			s := SniffFamily(dir)
			if walkSeesIt {
				// Case-insensitive FS: the folder IS the marker to the walk.
				if !s.Confident || s.Family != tc.wantFamily {
					t.Fatalf("case-insensitive FS: mis-cased %s/ should sniff confident %v, got %+v", tc.folder, tc.wantFamily, s)
				}
				if s.Hint != "" {
					t.Fatalf("no false rename hint when the walk resolves %s/, got %q", tc.folder, s.Hint)
				}
				return
			}
			// Case-sensitive FS: the real #203 footgun. Not confident at all —
			// that single check covers the confident-tabular masquerade — plus a
			// rename hint that names both the folder and its lowercase form.
			if s.Confident {
				t.Fatalf("case-sensitive FS: mis-cased %s/ + labels.csv must be ambiguous, got %+v", tc.folder, s)
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

	t.Run("lone labels.csv (no media dir) is ambiguous, not a table", func(t *testing.T) {
		// labels.csv is the image/text MANIFEST name; a folder with only it and
		// no images/texts/sequences is far more likely an incomplete image/text
		// dataset than a table. The sniff must ask, not confidently ingest it as
		// a table (which would drop the intended media silently).
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "labels.csv"), "filename,label\na.jpg,cat\n")
		if s := SniffFamily(dir); s.Confident {
			t.Fatalf("lone labels.csv must be ambiguous, got %+v", s)
		} else if s.Hint == "" || !strings.Contains(s.Hint, "labels.csv") {
			t.Fatalf("hint should explain the missing media folder, got %q", s.Hint)
		}
		// Contrast: a single NON-manifest CSV is still confident tabular.
		dir2 := t.TempDir()
		writePrev(t, filepath.Join(dir2, "patients.csv"), "age,label\n1,c\n")
		if s := SniffFamily(dir2); !s.Confident || s.Family != FamilyTabular {
			t.Fatalf("a single non-labels CSV should stay confident tabular, got %+v", s)
		}
	})

	t.Run("symlinked media marker + labels.csv is ambiguous (walk can't use it)", func(t *testing.T) {
		// A symlinked images/ is not a real directory to the walk (Lstat +
		// rejectSymlink), so it must not fall through to confident tabular and
		// silently ingest labels.csv as a table — the symlink sibling of #203.
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "labels.csv"), "filename,label\na.jpg,cat\n")
		if err := os.Symlink(t.TempDir(), filepath.Join(dir, "images")); err != nil {
			t.Skipf("symlink unsupported on this platform: %v", err)
		}
		if s := SniffFamily(dir); s.Confident {
			t.Fatalf("a symlinked images/ marker must be ambiguous, got %+v", s)
		} else if !strings.Contains(s.Hint, "images") {
			t.Fatalf("hint should name the unusable images marker, got %q", s.Hint)
		}
	})

	t.Run("a plain file named images is ambiguous (not a usable marker)", func(t *testing.T) {
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "labels.csv"), "filename,label\na.jpg,cat\n")
		writePrev(t, filepath.Join(dir, "images"), "not a folder")
		if s := SniffFamily(dir); s.Confident {
			t.Fatalf("a plain file named images must be ambiguous, got %+v", s)
		}
	})

	t.Run("dir whose only csv is a symlink is ambiguous (walk rejects it)", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "real.csv")
		writePrev(t, target, "a,b\n1,2\n")
		if err := os.Symlink(target, filepath.Join(dir, "data.csv")); err != nil {
			t.Skipf("symlink unsupported on this platform: %v", err)
		}
		if s := SniffFamily(dir); s.Confident {
			t.Fatalf("a dir whose only csv is a symlink must be ambiguous, got %+v", s)
		}
	})

	t.Run("regular csv + symlinked csv is ambiguous (walk counts both → multiple)", func(t *testing.T) {
		// findSingleCSV counts every non-dir .csv, symlinks included, so a
		// regular CSV plus a symlinked one is "multiple CSVs" to the walk. The
		// sniff must count them the same way and stay ambiguous, not sniff
		// confident tabular off the lone regular CSV. (#223 Bugbot follow-up.)
		dir := t.TempDir()
		writePrev(t, filepath.Join(dir, "data.csv"), "a,b\n1,2\n")
		target := filepath.Join(t.TempDir(), "extra.csv")
		writePrev(t, target, "c,d\n3,4\n")
		if err := os.Symlink(target, filepath.Join(dir, "link.csv")); err != nil {
			t.Skipf("symlink unsupported on this platform: %v", err)
		}
		if s := SniffFamily(dir); s.Confident {
			t.Fatalf("regular + symlinked CSV must be ambiguous, got %+v", s)
		}
		// And the walk it mirrors rejects the same dir as multi-CSV.
		if _, err := DiscoverTabular(dir); err == nil {
			t.Fatal("DiscoverTabular should reject a dir with a regular + a symlinked CSV")
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
	// The self-supervised text tasks have no label column (MLM/CLM predict from
	// the text itself; seq2seq/embeddings derive their target from the record
	// structure), so the interactive flow skips the label question for them.
	for _, id := range []string{"masked_language_modeling", "causal_language_modeling", "seq2seq", "embeddings"} {
		if !SelfSupervisedText(id) {
			t.Errorf("%s should be self-supervised", id)
		}
	}
	// The SUPERVISED text tasks carry a label column and must still be asked.
	for _, id := range []string{"text_classification", "token_classification", "sentence_pair_classification", "tabular_regression", "image_classification"} {
		if SelfSupervisedText(id) {
			t.Errorf("%s should not be self-supervised", id)
		}
	}
}
