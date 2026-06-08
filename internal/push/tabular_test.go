package push

import (
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
