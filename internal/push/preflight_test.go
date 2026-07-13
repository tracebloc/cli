package push

import (
	"encoding/csv"
	"errors"
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

	// minW/minH of 0 disables the floor so these decode/mismatch cases
	// exercise the same behavior as before the #348 floor landed (the
	// 8x8 / 4x4 fixtures are below the real 32x32 default).
	if err := ValidateImages([]string{good}, 8, 8, 0, 0); err != nil {
		t.Errorf("valid image rejected: %v", err)
	}
	if err := ValidateImages([]string{good, zero}, 8, 8, 0, 0); err == nil {
		t.Fatal("zero-byte image must be rejected (cli#72b)")
	} else if !strings.Contains(err.Error(), "0 bytes") {
		t.Errorf("zero-byte diagnosis missing: %v", err)
	}
	if err := ValidateImages([]string{good, corrupt}, 8, 8, 0, 0); err == nil {
		t.Fatal("corrupt image must be rejected (cli#72b)")
	}
	if err := ValidateImages([]string{good, odd}, 8, 8, 0, 0); err == nil {
		t.Fatal("resolution mismatch must be rejected (cli#72c — the ingestor validates, it does not resize)")
	} else if !strings.Contains(err.Error(), "4x4") || !strings.Contains(err.Error(), "8x8") {
		t.Errorf("mismatch error must show both sizes: %v", err)
	}
	// 0x0 expectation skips the resolution comparison entirely.
	if err := ValidateImages([]string{good, odd}, 0, 0, 0, 0); err != nil {
		t.Errorf("no expected size → no resolution rejection: %v", err)
	}
}

// TestValidateImagesMinSize covers the #348 minimum-size floor preview:
// an image below the floor is rejected (naming the file, its dimensions,
// and the floor); an image exactly at the floor passes; the floor takes
// precedence over a target_size mismatch; and it mirrors the ingestor's
// default (push.MinImageSize).
func TestValidateImagesMinSize(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, w, h int) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pngBytes(t, w, h), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	minW, minH := MinImageSize[0], MinImageSize[1] // 32x32, mirrors data-ingestors #348

	atFloor := write("at_floor.png", minW, minH)
	aboveFloor := write("above.png", minW+16, minH+16)
	belowW := write("below_w.png", minW-1, minH) // one side under → too small
	tiny := write("tiny.png", 8, 8)              // both sides under

	// At or above the floor passes (exact-floor image is accepted).
	if err := ValidateImages([]string{atFloor, aboveFloor}, 0, 0, minW, minH); err != nil {
		t.Errorf("at/above-floor images rejected: %v", err)
	}
	// One side below the floor → rejected, naming the file, its size, and the floor.
	err := ValidateImages([]string{atFloor, belowW}, 0, 0, minW, minH)
	if err == nil {
		t.Fatal("below-floor image must be rejected (#348)")
	}
	for _, want := range []string{"below_w.png", "31x32", "32x32", "min-size"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("too-small error missing %q: %v", want, err)
		}
	}
	// The floor takes precedence over a target_size mismatch: tiny is both
	// below the floor AND != the 64x64 target, but the too-small message wins.
	err = ValidateImages([]string{tiny}, 64, 64, minW, minH)
	if err == nil {
		t.Fatal("tiny image must be rejected")
	}
	if !strings.Contains(err.Error(), "minimum") {
		t.Errorf("floor must take precedence over the mismatch message: %v", err)
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

// TestPreflightDataset_TextLabelParity locks the text-family label preflight to
// the ingestor's wiring across ALL supervised text tasks (not just
// text_classification, which is how the gate drifted when #182 wired the rest):
//   - a missing label column fails locally (BadFlag) for every supervised text
//     task — LabelColumnValidator for text/sentence_pair, BIOLabelValidator for
//     token_classification — instead of uploading and failing in-cluster;
//   - a single-class label is rejected for the is_classification text tasks
//     (LabelDiversityValidator), but token_classification (BIO tag sequences,
//     is_classification=false) must NOT trigger diversity — the ingestor never
//     runs it, so neither may the CLI.
func TestPreflightDataset_TextLabelParity(t *testing.T) {
	writeLayout := func(t *testing.T, content string) *LocalLayout {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "labels.csv")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return &LocalLayout{Root: dir, LabelsCSV: p}
	}

	for _, cat := range []string{"text_classification", "sentence_pair_classification", "token_classification"} {
		layout := writeLayout(t, "filename,text\na.txt,foo\nb.txt,bar\n")
		_, problem := PreflightDataset(SpecArgs{Category: cat, LabelColumn: "label"}, layout)
		if problem == nil {
			t.Errorf("%s: a missing label column should fail preflight before upload", cat)
			continue
		}
		if !problem.BadFlag {
			t.Errorf("%s: a missing label column should be a BadFlag (settings) problem, got %v", cat, problem.Err)
		}
	}

	single := "filename,label\na.txt,x\nb.txt,x\n"
	for _, cat := range []string{"text_classification", "sentence_pair_classification"} {
		layout := writeLayout(t, single)
		if _, problem := PreflightDataset(SpecArgs{Category: cat, LabelColumn: "label"}, layout); problem == nil {
			t.Errorf("%s: a single-class label should be rejected (classification needs >=2 classes)", cat)
		}
	}
	layout := writeLayout(t, single)
	if _, problem := PreflightDataset(SpecArgs{Category: "token_classification", LabelColumn: "label"}, layout); problem != nil {
		t.Errorf("token_classification: a single-value label must NOT trigger the diversity check "+
			"(BIO labels aren't class labels; the ingestor runs BIOLabelValidator, not LabelDiversity): %v", problem.Err)
	}
}

func TestCheckSequenceSchemaColumns(t *testing.T) {
	// Previews the ingest.v1 sequence-grouped conditional (backend#1054
	// Decision-2): the schema must declare BOTH fixed sequence columns.
	g := GroupingSpec{GroupColumn: "sequence_id", TimeColumn: "timestamp", CountUnit: "sequences"}

	ok := map[string]string{"sequence_id": "VARCHAR(64)", "timestamp": "INT", "hr": "FLOAT", "label": "VARCHAR(16)"}
	if err := CheckSequenceSchemaColumns(ok, g); err != nil {
		t.Errorf("schema with both sequence columns rejected: %v", err)
	}

	missingBoth := map[string]string{"hr": "FLOAT", "label": "VARCHAR(16)"}
	err := CheckSequenceSchemaColumns(missingBoth, g)
	if err == nil {
		t.Fatal("schema without sequence_id/timestamp must be rejected")
	}
	for _, want := range []string{"sequence_id", "timestamp", "fixed by the platform"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}

	// The JSON-schema `required` is exact on keys — a case variant must not
	// satisfy it, or the CLI would accept a YAML the schema validation (and
	// the cluster) rejects.
	caseVariant := map[string]string{"Sequence_ID": "VARCHAR(64)", "timestamp": "INT"}
	if err := CheckSequenceSchemaColumns(caseVariant, g); err == nil {
		t.Error("a case-variant sequence_id key must not satisfy the schema conditional")
	}
}

func TestCheckSequenceRows(t *testing.T) {
	// Previews SequenceGroupValidator's null-id rule: every timestep row
	// must carry a sequence id; NA sentinels count as null (pandas parity).
	g := "sequence_id"

	good := writeTmp(t, "good.csv", []byte("sequence_id,timestamp,hr,label\np1,1,80,sepsis\np1,2,82,sepsis\np2,1,70,healthy\n"))
	seqs, err := CheckSequenceRows(good, g)
	if err != nil {
		t.Fatalf("clean grouped CSV rejected: %v", err)
	}
	if seqs != 2 {
		t.Errorf("sequences = %d, want 2 (the platform counts sequences, not rows)", seqs)
	}

	// Empty and NA-sentinel ids are both null (the ingestor loads with
	// pandas, whose NA parsing fires before the isna() probe).
	nulls := writeTmp(t, "nulls.csv", []byte("sequence_id,timestamp,hr,label\np1,1,80,a\n,2,82,a\nNA,3,84,b\n"))
	if _, err := CheckSequenceRows(nulls, g); err == nil {
		t.Fatal("rows with empty/NA sequence ids must be rejected")
	} else if !strings.Contains(err.Error(), "2 empty/null value(s)") {
		t.Errorf("error should count both null forms, got: %v", err)
	}

	// Header resolution follows the shared case-/whitespace-insensitive
	// rule (#340) — a " Sequence_ID " header still resolves.
	loose := writeTmp(t, "loose.csv", []byte(" Sequence_ID ,timestamp,label\np1,1,a\np2,1,b\n"))
	if seqs, err := CheckSequenceRows(loose, g); err != nil || seqs != 2 {
		t.Errorf("case/whitespace-variant header must resolve (ingestor rule): seqs=%d err=%v", seqs, err)
	}

	// Absent column benign-skips — CheckSequenceSchemaColumns owns that
	// diagnostic, exactly like the diversity check's benign skip.
	noCol := writeTmp(t, "nocol.csv", []byte("id,timestamp,label\np1,1,a\n"))
	if _, err := CheckSequenceRows(noCol, g); err != nil {
		t.Errorf("missing column must benign-skip: %v", err)
	}

	// The null set is pandas' DEFAULT NA tokens (STR_NA_VALUES, raw-matched) ∪
	// whitespace-only — SequenceGroupValidator's plain read_csv, NOT the curated
	// coercion.NA_SENTINELS the label checks use (cli#239). Each boundary below
	// was ground-truthed against the real validator's
	// `isna() | (astype(str).str.strip()=="")`.

	// 'none' (lowercase) is in coercion.NA_SENTINELS but NOT in STR_NA_VALUES,
	// so pandas keeps it → a REAL id → ACCEPT. Before the fix the trim+naSentinels
	// probe saw it as null and REJECTED (the dangerous over-reject). Reverting
	// sequenceScanFrom to naSentinels flips this to reject — mutation proof.
	// Parity twin: cases/tsc-none-sequence-id.
	none := writeTmp(t, "none.csv", []byte("sequence_id,timestamp,label\nnone,1,a\nnone,2,a\np2,1,b\n"))
	if seqs, err := CheckSequenceRows(none, g); err != nil || seqs != 2 {
		t.Errorf("'none' is not a pandas NA token → a real id; must accept with 2 sequences: seqs=%d err=%v", seqs, err)
	}

	// '#NA' is in STR_NA_VALUES but NOT in coercion.NA_SENTINELS, so pandas
	// drops it to NaN (null) while the pre-fix probe MISSED it (the under-reject
	// direction). Must now reject.
	hashNA := writeTmp(t, "hashna.csv", []byte("sequence_id,timestamp,label\np1,1,a\n#NA,2,b\n"))
	if _, err := CheckSequenceRows(hashNA, g); err == nil {
		t.Error("'#NA' is a pandas default NA token → a null id; must be rejected")
	}

	// A whitespace-only id is null via the .str.strip()=="" clause (a non-NA
	// object cell that strips to empty). "" is already in pandasDefaultNA, so this
	// clause is what catches genuine whitespace — a guard that it wasn't lost when
	// the raw-cell trim was dropped.
	ws := writeTmp(t, "ws.csv", []byte("sequence_id,timestamp,label\np1,1,a\n\"   \",2,b\n"))
	if _, err := CheckSequenceRows(ws, g); err == nil {
		t.Error("a whitespace-only sequence id must be rejected (str.strip()=='' clause)")
	}

	// A PADDED sentinel (' NA ') is NOT a pandas NA token: pandas tokenises NA on
	// the raw field (skipinitialspace defaults False), so ' NA ' is a REAL id
	// in-cluster. Matching the raw (not trimmed) cell keeps parity; the pre-fix
	// trim made it null (over-reject).
	padded := writeTmp(t, "padded.csv", []byte("sequence_id,timestamp,label\n\" NA \",1,a\n\" NA \",2,a\np2,1,b\n"))
	if seqs, err := CheckSequenceRows(padded, g); err != nil || seqs != 2 {
		t.Errorf("padded ' NA ' is a real id in-cluster (raw NA match); must accept with 2 sequences: seqs=%d err=%v", seqs, err)
	}
}

// TestPreflightDataset_SequenceGrouped locks the dispatch-level wiring for the
// sequence-grouped tabular task (time_series_classification, backend#1054):
// the grouping checks fire off the vendored contract's grouping TRAIT
// (Decision-4), the diversity gate fires off IsClassification (not a
// hardcoded id), and the ungrouped time-series sibling is untouched.
func TestPreflightDataset_SequenceGrouped(t *testing.T) {
	writeLayout := func(t *testing.T, content string) *LocalLayout {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "data.csv")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return &LocalLayout{Root: dir, LabelsCSV: p}
	}
	schema := map[string]string{
		"sequence_id": "VARCHAR(64)", "timestamp": "INT",
		"hr": "FLOAT", "label": "VARCHAR(16)",
	}
	spec := func(s map[string]string) SpecArgs {
		return SpecArgs{Category: "time_series_classification", LabelColumn: "label", Schema: s}
	}

	// Valid two-sequence, two-class dataset: accepted, and the advisory
	// note surfaces the SEQUENCE count (Decision-3's sample unit).
	good := "sequence_id,timestamp,hr,label\np1,1,80,sepsis\np1,2,82,sepsis\np2,1,70,healthy\n"
	notes, problem := PreflightDataset(spec(schema), writeLayout(t, good))
	if problem != nil {
		t.Fatalf("valid grouped dataset rejected: %v", problem.Err)
	}
	foundNote := false
	for _, n := range notes {
		if strings.Contains(n, "2 sequence(s)") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Errorf("expected a sequence-count note, got %v", notes)
	}

	// Schema missing the fixed sequence columns → rejected before upload.
	bare := map[string]string{"hr": "FLOAT", "label": "VARCHAR(16)"}
	bareCSV := "hr,label\n80,sepsis\n70,healthy\n"
	if _, problem := PreflightDataset(spec(bare), writeLayout(t, bareCSV)); problem == nil {
		t.Error("schema without sequence_id/timestamp should be rejected")
	}

	// A null sequence id → rejected (SequenceGroupValidator preview).
	nullID := "sequence_id,timestamp,hr,label\np1,1,80,sepsis\n,2,82,sepsis\np2,1,70,healthy\n"
	if _, problem := PreflightDataset(spec(schema), writeLayout(t, nullID)); problem == nil {
		t.Error("a row with an empty sequence_id should be rejected")
	}

	// Single-class labels → rejected via IsClassification (the diversity
	// gate must cover TSC, not just tabular_classification).
	oneClass := "sequence_id,timestamp,hr,label\np1,1,80,sepsis\np2,1,70,sepsis\n"
	if _, problem := PreflightDataset(spec(schema), writeLayout(t, oneClass)); problem == nil {
		t.Error("a single-class grouped dataset should be rejected (LabelDiversityValidator preview)")
	}

	// The ungrouped time-series sibling must NOT gain the grouping checks:
	// forecasting has no grouping trait and no diversity gate.
	tsf := SpecArgs{Category: "time_series_forecasting", LabelColumn: "label",
		Schema: map[string]string{"timestamp": "INT", "hr": "FLOAT", "label": "FLOAT"}}
	tsfCSV := "timestamp,hr,label\n1,80,0.1\n2,82,0.1\n"
	if _, problem := PreflightDataset(tsf, writeLayout(t, tsfCSV)); problem != nil {
		t.Errorf("time_series_forecasting must stay ungrouped and diversity-free: %v", problem.Err)
	}
}

// TestCheckTSCGroupIntegrity previews the two grouped time_series_classification
// validators di#359 added (cli#218): one label per sequence
// (LabelConstantWithinGroupValidator) and per-group ascending time order
// (PerGroupTimeOrderedValidator). Each row of a table asserts the exact
// over/under-reject boundary the ingestor draws.
func TestCheckTSCGroupIntegrity(t *testing.T) {
	const g, lab, tc = "sequence_id", "label", "timestamp"
	numeric := map[string]string{"sequence_id": "VARCHAR(64)", "timestamp": "INT", "label": "INT"}
	dateTyped := map[string]string{"sequence_id": "VARCHAR(64)", "timestamp": "TIMESTAMP", "label": "INT"}

	cases := []struct {
		name       string
		csv        string
		schema     map[string]string
		wantReject bool
		wantSubstr string // required substring when rejecting
	}{
		{
			name:   "clean-multi-sequence",
			csv:    "sequence_id,timestamp,label\np1,1,0\np1,2,0\np2,1,1\np2,2,1\n",
			schema: numeric,
		},
		{
			name:       "label-flip-mid-sequence",
			csv:        "sequence_id,timestamp,label\np1,1,0\np1,2,1\np2,1,0\n",
			schema:     numeric,
			wantReject: true,
			wantSubstr: "change their",
		},
		{
			name:   "single-row-group-not-flagged",
			csv:    "sequence_id,timestamp,label\np1,1,0\np2,1,1\n",
			schema: numeric,
			// each sequence has exactly one row → one label, one timestamp:
			// never a flip, never out of order.
		},
		{
			name:       "per-group-unsorted-timestamps",
			csv:        "sequence_id,timestamp,label\np1,3,1\np1,1,1\np1,2,1\np2,1,0\n",
			schema:     numeric,
			wantReject: true,
			wantSubstr: "out-of-order",
		},
		{
			name:   "ties-are-non-decreasing-ok",
			csv:    "sequence_id,timestamp,label\np1,1,0\np1,1,0\np1,2,0\np2,1,1\n",
			schema: numeric, // equal successive timestamps are allowed (monotonic non-decreasing)
		},
		{
			name:   "interleaved-each-sorted-ok",
			csv:    "sequence_id,timestamp,label\np1,1,0\np2,1,1\np1,2,0\np2,2,1\n",
			schema: numeric, // interleaving sequences is fine; order is per-sequence only
		},
		{
			name:   "numeric-label-collapse-not-a-flip",
			csv:    "sequence_id,timestamp,label\np1,1,1\np1,2,1.0\np2,1,0\n",
			schema: numeric, // "1" and "1.0" under a numeric column are ONE value
		},
		{
			name: "pandas-default-na-sentinel-keeps-label-numeric",
			// p3's sole label "#NA" is a pandas DEFAULT NA token (STR_NA_VALUES)
			// but NOT in the curated coercion.NA_SENTINELS. The ingestor plain-
			// reads the file, so pandas drops it to NaN and the label column is
			// all-numeric — p1's "1"/"1.0" collapse to one value (no flip), so
			// the ingestor ACCEPTS. Before cli#218 the CLI mirrored the coercion
			// set, saw "#NA" as a distinct string, read the column as object, and
			// flagged p1 as a false mid-sequence flip. This pins the accept —
			// reverting naSentinels->pandasDefaultNA makes it fail (mutation
			// proof). Parity twin: cases/tsc-numeric-label-na-sentinel.
			csv:    "sequence_id,timestamp,label\np1,1,1\np1,2,1.0\np2,1,2\np2,2,2\np3,1,#NA\n",
			schema: numeric,
		},
		{
			name: "padded-object-label-flip-still-rejected",
			// object (non-numeric) label column: pandas keeps whitespace on
			// object cells, so " a" and "a" are DISTINCT values — p1 genuinely
			// flips. Pins that dropping the cell-trim (cli#218) did not weaken a
			// real object-label rejection.
			csv:        "sequence_id,timestamp,label\np1,1, a\np1,2,a\np2,1,b\n",
			schema:     numeric,
			wantReject: true,
			wantSubstr: "change their",
		},
		{
			name:       "null-label-mid-sequence-is-a-flip",
			csv:        "sequence_id,timestamp,label\np1,1,1\np1,2,\np2,1,0\n",
			schema:     numeric, // {1, null} within p1 → nunique(dropna=False) = 2
			wantReject: true,
			wantSubstr: "change their",
		},
		{
			name:       "invalid-numeric-timestamp",
			csv:        "sequence_id,timestamp,label\np1,1,0\np1,x,0\np2,1,1\n",
			schema:     numeric,
			wantReject: true,
			wantSubstr: "missing/invalid",
		},
		{
			name: "null-id-rows-dropped-from-grouping",
			// the null-id row carries a different label + out-of-order time,
			// but dropna=True excludes it — CheckSequenceRows owns null ids,
			// so THIS check must not fire on it.
			csv:    "sequence_id,timestamp,label\np1,1,0\n,9,1\np1,2,0\np2,1,1\n",
			schema: numeric,
		},
		{
			name: "date-typed-unsorted-benign-skip",
			// a TIMESTAMP-typed time column that is genuinely unsorted: the
			// numeric branch is not selected, and the CLI deliberately does
			// NOT preview the date branch (documented under-preview, cli#218)
			// — so it must NOT reject here (safe direction).
			csv:    "sequence_id,timestamp,label\np1,2026-01-02,0\np1,2026-01-01,0\np2,2026-01-01,1\n",
			schema: dateTyped,
		},
		{
			name: "absent-columns-benign-skip",
			// no sequence_id column at all → the schema/sequence checks own
			// that diagnostic; this check benign-skips (returns nil).
			csv:    "timestamp,label\n1,0\n2,1\n",
			schema: numeric,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeTmp(t, "data.csv", []byte(c.csv))
			err := CheckTSCGroupIntegrity(p, g, lab, tc, c.schema)
			if c.wantReject {
				if err == nil {
					t.Fatalf("expected reject, got accept")
				}
				if c.wantSubstr != "" && !strings.Contains(err.Error(), c.wantSubstr) {
					t.Errorf("error missing %q: %v", c.wantSubstr, err)
				}
			} else if err != nil {
				t.Fatalf("expected accept, got reject: %v", err)
			}
		})
	}
}

// TestLabelColumnValuesFrom_ReadErrorFailsClosed: a mid-scan read error on the
// label column must abort with an error (fail closed) rather than return a
// PARTIAL class set — a truncated count would false-reject good data or pass a
// bad file. Mirrors the text preflight (#221) and CrossCheckLabels. The trigger
// is I/O, not malformed CSV (LazyQuotes + FieldsPerRecord=-1 parse every bad
// shape), so it's exercised with an injected reader that fails after the header.
func TestLabelColumnValuesFrom_ReadErrorFailsClosed(t *testing.T) {
	sentinel := errors.New("disk gave up mid-read")
	r := csv.NewReader(&failAfterReader{data: []byte("label\n"), err: sentinel})
	r.FieldsPerRecord = -1 // match openCSVReader

	v, err := labelColumnValuesFrom(r, "label", false, false)
	if err == nil {
		t.Fatal("labelColumnValuesFrom returned nil error on a mid-scan read failure; must fail closed")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error should wrap the underlying read failure, got: %v", err)
	}
	if v.Found {
		t.Error("a failed read must not report Found=true (a partial column)")
	}
}

// TestSequenceScanFrom_ReadErrorFailsClosed: the sequence-id scan must surface a
// mid-scan read error (readErr) rather than silently pass a partial scan whose
// unread tail could hide null ids. Same fail-closed contract as #221.
func TestSequenceScanFrom_ReadErrorFailsClosed(t *testing.T) {
	sentinel := errors.New("disk gave up mid-read")
	r := csv.NewReader(&failAfterReader{data: []byte("sequence_id\n"), err: sentinel})
	r.FieldsPerRecord = -1 // match openCSVReader

	_, nullErr, readErr := sequenceScanFrom(r, "sequence_id")
	if readErr == nil {
		t.Fatal("sequenceScanFrom returned nil readErr on a mid-scan read failure; must fail closed")
	}
	if !errors.Is(readErr, sentinel) {
		t.Errorf("readErr should wrap the underlying read failure, got: %v", readErr)
	}
	if nullErr != nil {
		t.Errorf("a read failure must not be reported as a null-id domain error: %v", nullErr)
	}
}

// TestOpenCSVReader_LazyQuotesMatchesPandas: a bare/unescaped quote in a data
// row is tolerated by pandas (the ingestor), so the CLI's mirror-checks must
// read the row rather than error on it. Before openCSVReader set LazyQuotes,
// CheckLabelDiversity/CrossCheckLabels/CheckSequenceRows all errored on such a
// row → false-rejecting a dataset the cluster accepts. Guard against regressing
// to the strict reader.
func TestOpenCSVReader_LazyQuotesMatchesPandas(t *testing.T) {
	// Row 2's filename cell has a bare quote pandas keeps; the label column has
	// two distinct classes across the three rows.
	csvBody := "label,filename\ncat,a.jpg\ndog,b\"x.jpg\ncat,c.jpg\n"
	p := writeTmp(t, "labels.csv", []byte(csvBody))

	// Diversity must NOT false-reject: all 3 rows read → classes {cat,dog}.
	if err := CheckLabelDiversity(p, "label", false, false); err != nil {
		t.Errorf("bare-quote row must be read like pandas, not rejected: %v", err)
	}
	v := ReadLabelValues(p, "label", false, false)
	if !v.Found || v.RowCount != 3 || len(v.Classes) != 2 {
		t.Errorf("bare-quote CSV misread: Found=%v RowCount=%d classes=%v (want 3 rows, 2 classes)",
			v.Found, v.RowCount, v.Classes)
	}
}

// TestCheckMaskPairing mirrors the ingestor's FilePairingValidator for
// semantic_segmentation (sidecar_suffix="_mask"): images↔masks pair by
// <image>_mask.png; a gap in either direction, or a mask not carrying the
// suffix, is reported.
func TestCheckMaskPairing(t *testing.T) {
	cases := []struct {
		name    string
		images  []string
		masks   []string
		wantErr string // substring; "" = no error
	}{
		{"paired", []string{"images/001.jpg", "images/002.jpg"},
			[]string{"masks/001_mask.png", "masks/002_mask.png"}, ""},
		{"image without mask", []string{"images/001.jpg", "images/002.jpg"},
			[]string{"masks/001_mask.png"}, "without a mask"},
		{"mask without image", []string{"images/001.jpg"},
			[]string{"masks/001_mask.png", "masks/002_mask.png"}, "without an image"},
		{"non-conforming mask name (no _mask suffix)", []string{"images/001.jpg"},
			[]string{"masks/001.png"}, "not named <image>_mask.png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckMaskPairing(tc.images, tc.masks)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestCheckMaskIdColumn mirrors the ingestor's MaskIdColumnValidator
// (backend#816): the manifest must declare an exact-lowercase mask_id column and
// populate it on every row (NA-sentinel-aware); a wrong-case column gets a
// rename hint, an absent one an add hint.
func TestCheckMaskIdColumn(t *testing.T) {
	cases := []struct {
		name    string
		csv     string
		wantErr string
	}{
		{"valid", "image_label,filename,mask_id\ncat,001.jpg,001_mask\n", ""},
		{"missing column", "image_label,filename\ncat,001.jpg\n", `needs a "mask_id" column`},
		{"wrong case", "filename,Mask_Id\n001.jpg,001_mask\n", "wrong case"},
		{"empty value on a row", "filename,mask_id\n001.jpg,001_mask\n002.jpg,\n", "empty"},
		{"NA-sentinel value", "filename,mask_id\n001.jpg,NULL\n", "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeTmp(t, "labels.csv", []byte(tc.csv))
			err := CheckMaskIdColumn(p)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
