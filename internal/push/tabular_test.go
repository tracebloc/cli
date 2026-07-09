package push

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDiscoverTabular_SingleCSV: a directory with exactly one CSV
// resolves to a layout whose LabelsCSV is that file and whose Images
// slice is empty (so the existing tar/stream machinery stages just
// the CSV).
func TestDiscoverTabular_SingleCSV(t *testing.T) {
	dir := t.TempDir()
	csv := writeFile(t, dir, "data.csv", "a,b\n1,2\n")

	layout, err := DiscoverTabular(dir)
	if err != nil {
		t.Fatalf("DiscoverTabular: %v", err)
	}
	if layout.LabelsCSV != csv {
		t.Errorf("LabelsCSV = %q, want %q", layout.LabelsCSV, csv)
	}
	if len(layout.Images) != 0 {
		t.Errorf("Images = %v, want empty (tabular has no sidecar files)", layout.Images)
	}
	if layout.TotalBytes == 0 {
		t.Errorf("TotalBytes = 0, want the CSV's size")
	}
}

// TestDiscoverTabular_BareCSVFile: a bare .csv file (not a directory) is
// accepted for tabular (#181). It resolves to a layout whose LabelsCSV is
// that file, Root is the file's parent directory, and Images is empty — the
// SAME shape a single-CSV directory produces, so the tar/stream machinery
// stages it identically (as labels.csv under the dataset).
func TestDiscoverTabular_BareCSVFile(t *testing.T) {
	dir := t.TempDir()
	csv := writeFile(t, dir, "churn.csv", "age,churned\n30,yes\n40,no\n")

	layout, err := DiscoverTabular(csv)
	if err != nil {
		t.Fatalf("DiscoverTabular(bare .csv): %v", err)
	}
	if layout.LabelsCSV != csv {
		t.Errorf("LabelsCSV = %q, want %q", layout.LabelsCSV, csv)
	}
	if layout.Root != dir {
		t.Errorf("Root = %q, want the file's parent dir %q", layout.Root, dir)
	}
	if len(layout.Images) != 0 {
		t.Errorf("Images = %v, want empty", layout.Images)
	}
	if layout.TotalBytes == 0 {
		t.Errorf("TotalBytes = 0, want the CSV's size")
	}
}

// TestDiscoverTabular_BareFileVsDirSameStaging: a bare .csv and a directory
// holding that same CSV must stage byte-for-identically — both land the CSV
// as labels.csv at the dataset root — so bare-file support is a pure CLI-side
// input convenience and never changes what the ingestor reads.
func TestDiscoverTabular_BareFileVsDirSameStaging(t *testing.T) {
	body := "age,churned\n30,yes\n40,no\n"
	fileDir := t.TempDir()
	bare := writeFile(t, fileDir, "churn.csv", body)
	// Directory holding the same single CSV.
	someDir := t.TempDir()
	writeFile(t, someDir, "churn.csv", body)

	fileL, err := DiscoverTabular(bare)
	if err != nil {
		t.Fatalf("DiscoverTabular(file): %v", err)
	}
	dirL, err := DiscoverTabular(someDir)
	if err != nil {
		t.Fatalf("DiscoverTabular(dir): %v", err)
	}

	var fileTar, dirTar bytes.Buffer
	if err := writeLayoutTar(&fileTar, fileL); err != nil {
		t.Fatalf("writeLayoutTar(file): %v", err)
	}
	if err := writeLayoutTar(&dirTar, dirL); err != nil {
		t.Fatalf("writeLayoutTar(dir): %v", err)
	}
	if !bytes.Equal(fileTar.Bytes(), dirTar.Bytes()) {
		t.Error("bare-file and single-CSV-dir produced different staged tars; they must be identical")
	}
	// And the one entry is labels.csv.
	tr := tar.NewReader(&fileTar)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("reading tar entry: %v", err)
	}
	if hdr.Name != "labels.csv" {
		t.Errorf("staged entry = %q, want labels.csv", hdr.Name)
	}
}

// TestDiscoverTabular_BareNonCSVFile: a bare file that isn't a .csv is a
// clear error — tabular data is a single CSV, so we say so rather than
// letting a downstream reader choke.
func TestDiscoverTabular_BareNonCSVFile(t *testing.T) {
	dir := t.TempDir()
	txt := writeFile(t, dir, "notes.txt", "hello")
	if _, err := DiscoverTabular(txt); err == nil {
		t.Error("DiscoverTabular(bare .txt) returned nil error, want a clear .csv-required error")
	}
}

// TestDiscoverTabular_NoCSV and _MultipleCSV: the layout requires
// exactly one CSV; zero or many is a clear, actionable error rather
// than a guess.
func TestDiscoverTabular_NoCSV(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "notes.txt", "hello")
	if _, err := DiscoverTabular(dir); err == nil {
		t.Error("DiscoverTabular with no .csv returned nil error")
	}
}

func TestDiscoverTabular_MultipleCSV(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.csv", "x\n1\n")
	writeFile(t, dir, "b.csv", "y\n2\n")
	if _, err := DiscoverTabular(dir); err == nil {
		t.Error("DiscoverTabular with two .csv files returned nil error")
	}
}

// TestInferSchema covers the INT / FLOAT / VARCHAR inference from a
// CSV header + sample rows. Integer-only columns → INT, numeric (with
// a decimal) → FLOAT, anything else → VARCHAR(255).
func TestInferSchema(t *testing.T) {
	dir := t.TempDir()
	csv := writeFile(t, dir, "data.csv",
		"count,age,price,name\n1,30,9.99,alice\n2,40,19.5,bob\n")

	schema, _, _, err := InferSchema(csv)
	if err != nil {
		t.Fatalf("InferSchema: %v", err)
	}
	want := map[string]string{
		"count": "INT",
		"age":   "INT",
		"price": "FLOAT",
		"name":  "VARCHAR(255)",
	}
	for col, typ := range want {
		if schema[col] != typ {
			t.Errorf("schema[%q] = %q, want %q (full: %v)", col, schema[col], typ, schema)
		}
	}
}

// TestInferSchema_EmptyColumnIsFloat: a column with no non-empty sampled
// value can't be typed from data; it's returned as a nullable FLOAT (not
// VARCHAR — an all-NULL VARCHAR is what the ingestor's string validator
// rejects) and reported in the `empty` return so the caller can warn.
func TestInferSchema_EmptyColumnIsFloat(t *testing.T) {
	dir := t.TempDir()
	csv := writeFile(t, dir, "data.csv", "filled,empty\n1,\n2,\n")
	schema, _, empty, err := InferSchema(csv)
	if err != nil {
		t.Fatalf("InferSchema: %v", err)
	}
	if schema["empty"] != "FLOAT" {
		t.Errorf("schema[empty] = %q, want FLOAT", schema["empty"])
	}
	if schema["filled"] != "INT" {
		t.Errorf("schema[filled] = %q, want INT", schema["filled"])
	}
	if len(empty) != 1 || empty[0] != "empty" {
		t.Errorf("empty = %v, want [empty]", empty)
	}
}

// TestInferSchema_SkipsReservedColumns: a CSV with an `id` (or other
// framework-managed) column must NOT produce a schema that includes
// it — data-ingestors' create_table rejects such collisions (the
// #135b guard). The reserved columns come back in the skipped list.
func TestInferSchema_SkipsReservedColumns(t *testing.T) {
	dir := t.TempDir()
	csv := writeFile(t, dir, "data.csv", "id,feature_00,label\n1,1.5,0\n2,2.5,1\n")

	schema, skipped, _, err := InferSchema(csv)
	if err != nil {
		t.Fatalf("InferSchema: %v", err)
	}
	if _, present := schema["id"]; present {
		t.Errorf("schema includes reserved column id: %v", schema)
	}
	if schema["feature_00"] != "FLOAT" || schema["label"] != "INT" {
		t.Errorf("schema = %v, want feature_00:FLOAT, label:INT", schema)
	}
	foundID := false
	for _, s := range skipped {
		if s == "id" {
			foundID = true
		}
	}
	if !foundID {
		t.Errorf("skipped = %v, want it to contain id", skipped)
	}
}

func TestParseSchema(t *testing.T) {
	got, err := ParseSchema("age:INT, price:FLOAT ,name:VARCHAR(255)")
	if err != nil {
		t.Fatalf("ParseSchema: %v", err)
	}
	want := map[string]string{"age": "INT", "price": "FLOAT", "name": "VARCHAR(255)"}
	if len(got) != len(want) {
		t.Fatalf("ParseSchema len = %d, want %d (%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("ParseSchema[%q] = %q, want %q", k, got[k], v)
		}
	}

	for _, bad := range []string{"", "age", "age:", ":INT", "age=INT"} {
		if _, err := ParseSchema(bad); err == nil {
			t.Errorf("ParseSchema(%q) = nil error, want rejection", bad)
		}
	}
}
