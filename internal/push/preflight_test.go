package push

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTmp(t *testing.T, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

var bom = []byte{0xEF, 0xBB, 0xBF}

func TestReadCSVHeader_StripsBOMAndTrims(t *testing.T) {
	// Parity (cli#71): pandas strips the BOM in-cluster, so the local
	// reader must too — otherwise the CLI would reject label columns the
	// cluster accepts.
	p := writeTmp(t, "labels.csv", append(bom, []byte(" filename , label \nx.jpg,cat\n")...))
	h, err := ReadCSVHeader(p)
	if err != nil {
		t.Fatal(err)
	}
	if h[0] != "filename" || h[1] != "label" {
		t.Errorf("header = %v, want BOM-stripped + trimmed [filename label]", h)
	}
}

func TestCheckTabularBOM(t *testing.T) {
	// Parity (cli#71): the in-cluster tabular schema probe does NOT strip
	// the BOM and falsely rejects — the CLI must reject locally, before
	// the upload, with the actual fix.
	withBOM := writeTmp(t, "d.csv", append(bom, []byte("age,income\n1,2\n")...))
	if err := CheckTabularBOM(withBOM); err == nil {
		t.Fatal("BOM'd tabular CSV must be rejected (in-cluster schema probe would falsely reject it post-upload)")
	} else if !strings.Contains(err.Error(), "byte-order mark") || !strings.Contains(err.Error(), "Re-save") {
		t.Errorf("BOM error must explain + remediate, got: %v", err)
	}
	clean := writeTmp(t, "c.csv", []byte("age,income\n1,2\n"))
	if err := CheckTabularBOM(clean); err != nil {
		t.Errorf("clean CSV rejected: %v", err)
	}
}

func TestCheckLabelColumn_MatchesLikeTheIngestor(t *testing.T) {
	// Parity (cli#69): exact first, then case-insensitive + trimmed —
	// the ingestor's _match_column rule. Stricter matching would reject
	// datasets the cluster accepts.
	header := []string{"filename", " Label "}
	if err := CheckLabelColumn(header, "label", "labels.csv"); err != nil {
		t.Errorf("case-insensitive+trimmed match must pass (ingestor accepts it): %v", err)
	}
	if err := CheckLabelColumn(header, "target", "labels.csv"); err == nil {
		t.Fatal("absent label column must be rejected")
	} else {
		for _, want := range []string{`"target"`, "filename", "--label-column"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q, got: %v", want, err)
			}
		}
	}
}

func TestCheckDuplicateHeaders_CaseSensitiveLikeTheIngestor(t *testing.T) {
	// Parity (cli#73a): the ingestor compares stripped but case-SENSITIVE.
	if err := CheckDuplicateHeaders([]string{"a", "A"}, "the data CSV"); err != nil {
		t.Errorf("'a' vs 'A' are NOT duplicates to the ingestor: %v", err)
	}
	err := CheckDuplicateHeaders([]string{"age", "income", "age"}, "the data CSV")
	if err == nil {
		t.Fatal("duplicate headers must be rejected")
	}
	if !strings.Contains(err.Error(), "age") || !strings.Contains(err.Error(), "Rename") {
		t.Errorf("dup error must name the column + remediate: %v", err)
	}
}

func TestCheckHasDataRows(t *testing.T) {
	ok := writeTmp(t, "ok.csv", []byte("a,b\n1,2\n"))
	if err := CheckHasDataRows(ok); err != nil {
		t.Errorf("CSV with rows rejected: %v", err)
	}
	headerOnly := writeTmp(t, "h.csv", []byte("a,b\n"))
	if err := CheckHasDataRows(headerOnly); err == nil {
		t.Fatal("header-only CSV must be rejected (cli#73b — 0 ingestable records)")
	} else if !strings.Contains(err.Error(), "no data rows") {
		t.Errorf("unexpected message: %v", err)
	}
	empty := writeTmp(t, "e.csv", nil)
	if err := CheckHasDataRows(empty); err == nil {
		t.Fatal("empty CSV must be rejected")
	}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var sb strings.Builder
	if err := png.Encode(&nopWriter{&sb}, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return []byte(sb.String())
}

type nopWriter struct{ b *strings.Builder }

func (w *nopWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

func TestValidateImages(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, body []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.png", pngBytes(t, 8, 8))
	odd := write("odd.png", pngBytes(t, 4, 4))
	zero := write("zero.png", nil)
	corrupt := write("corrupt.png", []byte("not an image at all"))

	if err := ValidateImages([]string{good}, 8, 8); err != nil {
		t.Errorf("valid image rejected: %v", err)
	}
	if err := ValidateImages([]string{good, zero}, 8, 8); err == nil {
		t.Fatal("zero-byte image must be rejected (cli#72b)")
	} else if !strings.Contains(err.Error(), "0 bytes") {
		t.Errorf("zero-byte diagnosis missing: %v", err)
	}
	if err := ValidateImages([]string{good, corrupt}, 8, 8); err == nil {
		t.Fatal("corrupt image must be rejected (cli#72b)")
	}
	if err := ValidateImages([]string{good, odd}, 8, 8); err == nil {
		t.Fatal("resolution mismatch must be rejected (cli#72c — the ingestor validates, it does not resize)")
	} else if !strings.Contains(err.Error(), "4x4") || !strings.Contains(err.Error(), "8x8") {
		t.Errorf("mismatch error must show both sizes: %v", err)
	}
	// 0x0 expectation skips the resolution comparison entirely.
	if err := ValidateImages([]string{good, odd}, 0, 0); err != nil {
		t.Errorf("no expected size → no resolution rejection: %v", err)
	}
}

func TestCrossCheckLabels(t *testing.T) {
	dir := t.TempDir()
	imgs := filepath.Join(dir, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.jpg", "b.jpg", "extra.jpg"} {
		if err := os.WriteFile(filepath.Join(imgs, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// One row exact, one extensionless (the ingestor appends the dataset
	// extension — the check must mirror that), one missing.
	csvPath := filepath.Join(dir, "labels.csv")
	// Realistic image_classification labels.csv: the ingestor reads the image name
	// from the column NAMED "filename" (record.get("filename")).
	if err := os.WriteFile(csvPath,
		[]byte("filename,label\na.jpg,cat\nb,dog\nghost.jpg,cat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	images := []string{filepath.Join(imgs, "a.jpg"), filepath.Join(imgs, "b.jpg"), filepath.Join(imgs, "extra.jpg")}
	missing, orphans, err := CrossCheckLabels(csvPath, images, ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != "ghost.jpg" {
		t.Errorf("missing = %v, want [ghost.jpg] (extensionless 'b' must resolve to b.jpg)", missing)
	}
	if len(orphans) != 1 || orphans[0] != "extra.jpg" {
		t.Errorf("orphans = %v, want [extra.jpg]", orphans)
	}
}

// The image filename is resolved by the "filename" COLUMN NAME, not positionally —
// so a `label,filename` header (filename not first, a layout the ingestor accepts
// via record.get("filename")) must not false-reject. Reading rec[0] would treat the
// label value ("cat"/"dog") as the filename and flag every row missing (exit 3).
func TestCrossCheckLabels_FilenameColumnNotFirst(t *testing.T) {
	dir := t.TempDir()
	imgs := filepath.Join(dir, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.jpg", "b.jpg", "extra.jpg"} {
		if err := os.WriteFile(filepath.Join(imgs, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	csvPath := filepath.Join(dir, "labels.csv")
	// filename is the SECOND column, and mixed-case to exercise the ci match.
	if err := os.WriteFile(csvPath,
		[]byte("label,Filename\ncat,a.jpg\ndog,b\ncat,ghost.jpg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	images := []string{filepath.Join(imgs, "a.jpg"), filepath.Join(imgs, "b.jpg"), filepath.Join(imgs, "extra.jpg")}
	missing, orphans, err := CrossCheckLabels(csvPath, images, ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != "ghost.jpg" {
		t.Errorf("missing = %v, want [ghost.jpg] — the label values must NOT be read as filenames", missing)
	}
	if len(orphans) != 1 || orphans[0] != "extra.jpg" {
		t.Errorf("orphans = %v, want [extra.jpg]", orphans)
	}
}

// A `label,data_id` header (no filename column) is ingested cleanly by the cluster —
// the ingestor resolves the image column as filename ELSE data_id. CrossCheckLabels
// must resolve data_id, not fall back to index 0 (the label column), which would read
// "cat"/"dog" as filenames and false-reject every row (exit 3). Regression for Asad's
// #207 review.
func TestCrossCheckLabels_DataIDColumn(t *testing.T) {
	dir := t.TempDir()
	imgs := filepath.Join(dir, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.jpg", "b.jpg", "extra.jpg"} {
		if err := os.WriteFile(filepath.Join(imgs, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	csvPath := filepath.Join(dir, "labels.csv")
	// data_id instead of filename, and not first — a layout the ingestor accepts.
	if err := os.WriteFile(csvPath,
		[]byte("label,data_id\ncat,a.jpg\ndog,b\ncat,ghost.jpg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	images := []string{filepath.Join(imgs, "a.jpg"), filepath.Join(imgs, "b.jpg"), filepath.Join(imgs, "extra.jpg")}
	missing, orphans, err := CrossCheckLabels(csvPath, images, ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != "ghost.jpg" {
		t.Errorf("missing = %v, want [ghost.jpg] — data_id must resolve; label values must NOT be read as filenames", missing)
	}
	if len(orphans) != 1 || orphans[0] != "extra.jpg" {
		t.Errorf("orphans = %v, want [extra.jpg]", orphans)
	}
}

func TestImageFileColIndex(t *testing.T) {
	cases := []struct {
		header []string
		want   int
	}{
		{[]string{"filename", "label"}, 0},            // filename by name, first
		{[]string{"label", "filename"}, 1},            // filename by name, not first (the original bug)
		{[]string{"label", " Filename "}, 1},          // case + whitespace insensitive
		{[]string{"label", "data_id"}, 1},             // no filename → data_id (the ingestor's fallback)
		{[]string{"label", "Data_ID"}, 1},             // data_id, case-insensitive
		{[]string{"data_id", "filename", "label"}, 1}, // filename wins over data_id when both present
		{[]string{"image_id", "label"}, 0},            // neither → fallback to 0
	}
	for _, c := range cases {
		if got := imageFileColIndex(c.header); got != c.want {
			t.Errorf("imageFileColIndex(%v) = %d, want %d", c.header, got, c.want)
		}
	}
}

func TestCheckAnnotationPairing(t *testing.T) {
	imgs := []string{"images/a.jpg", "images/b.jpg"}
	anns := []string{"annotations/a.xml", "annotations/c.xml"}
	err := CheckAnnotationPairing(imgs, anns)
	if err == nil {
		t.Fatal("stem mismatch must be rejected (FilePairingValidator preview)")
	}
	for _, want := range []string{"b", "c", "don't pair up"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("pairing error should mention %q: %v", want, err)
		}
	}
	if err := CheckAnnotationPairing(imgs, []string{"annotations/a.xml", "annotations/b.xml"}); err != nil {
		t.Errorf("matched stems rejected: %v", err)
	}
}

func TestCheckLabelDiversity(t *testing.T) {
	// Parity: LabelDiversityValidator — classification needs >=2 distinct
	// (stripped) label values. Discovered by the parity harness's first
	// run; previously an in-cluster-only, post-upload failure.
	two := writeTmp(t, "two.csv", []byte("id,label\na,cat\nb, cat \nc,dog\n"))
	if err := CheckLabelDiversity(two, "label", false, false); err != nil {
		t.Errorf("2 distinct labels rejected: %v", err)
	}
	one := writeTmp(t, "one.csv", []byte("id,label\na,cat\nb,cat\n"))
	if err := CheckLabelDiversity(one, "label", false, false); err == nil {
		t.Fatal("single-class dataset must be rejected")
	} else if !strings.Contains(err.Error(), "at least 2 classes") {
		t.Errorf("unexpected message: %v", err)
	}
	// benign-skip when the column is absent (that's CheckLabelColumn's job)
	if err := CheckLabelDiversity(one, "nope", false, false); err != nil {
		t.Errorf("missing column must benign-skip like the ingestor: %v", err)
	}

	// Schema-type sensitivity (data-ingestors #252): "1" and "1.0" are two
	// distinct classes for a string-typed (VARCHAR) label — the ingestor
	// pins dtype=str and does NOT collapse them — but one class for a
	// numeric label, where pandas numeric inference merges them.
	numeric := writeTmp(t, "numeric.csv", []byte("id,label\na,1\nb,1.0\n"))
	if err := CheckLabelDiversity(numeric, "label", true /*dropNA*/, false /*collapseNumeric*/); err != nil {
		t.Errorf("VARCHAR label '1'/'1.0' must stay 2 classes (no numeric collapse): %v", err)
	}
	if err := CheckLabelDiversity(numeric, "label", true /*dropNA*/, true /*collapseNumeric*/); err == nil {
		t.Error("numeric label '1'/'1.0' must collapse to a single class and be rejected")
	}
}

func TestCheckLabelDiversitySchemaTypeDispatch(t *testing.T) {
	// PreflightDataset must derive the collapse flag from the label's schema
	// type: a numeric-looking VARCHAR label is accepted (2 classes), the same
	// data typed FLOAT is rejected (collapses to 1). Locks the fix at the
	// dispatch level, not just the leaf function.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("feat,label\nx,1\ny,1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout := &LocalLayout{Root: dir, LabelsCSV: filepath.Join(dir, "data.csv")}

	strSpec := SpecArgs{Category: "tabular_classification", LabelColumn: "label",
		Schema: map[string]string{"feat": "VARCHAR(255)", "label": "VARCHAR(10)"}}
	if _, problem := PreflightDataset(strSpec, layout); problem != nil {
		t.Errorf("VARCHAR label should keep '1'/'1.0' distinct and pass: %v", problem.Err)
	}

	numSpec := SpecArgs{Category: "tabular_classification", LabelColumn: "label",
		Schema: map[string]string{"feat": "VARCHAR(255)", "label": "FLOAT"}}
	if _, problem := PreflightDataset(numSpec, layout); problem == nil {
		t.Error("FLOAT label should collapse '1'/'1.0' and be rejected")
	}
}
