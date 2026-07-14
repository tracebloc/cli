package push

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPreflightCSVValidators_FileErrors covers the file-open error arm of every
// path-taking CSV validator (a missing file → the wrapped read error).
func TestPreflightCSVValidators_FileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.csv")
	if _, err := HasBOM(missing); err == nil {
		t.Error("HasBOM(missing) must error")
	}
	if _, err := ReadCSVHeader(missing); err == nil {
		t.Error("ReadCSVHeader(missing) must error")
	}
	if err := CheckTabularBOM(missing); err == nil {
		t.Error("CheckTabularBOM(missing) must error")
	}
	if err := CheckHasDataRows(missing); err == nil {
		t.Error("CheckHasDataRows(missing) must error")
	}
	if err := CheckCSVEncoding(missing); err == nil {
		t.Error("CheckCSVEncoding(missing) must error")
	}
}

// TestReadCSVHeader_EmptyFileIsEOF covers the io.EOF (no-header) arm.
func TestReadCSVHeader_EmptyFileIsEOF(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.csv")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCSVHeader(empty); err == nil {
		t.Error("ReadCSVHeader(empty) must error on an empty file (no header)")
	}
}

// TestHasBOM_ShortFile covers the "fewer than 3 bytes" arm: a short file can't
// carry a 3-byte UTF-8 BOM, so HasBOM is false without erroring.
func TestHasBOM_ShortFile(t *testing.T) {
	short := filepath.Join(t.TempDir(), "short.csv")
	if err := os.WriteFile(short, []byte("ab"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := HasBOM(short)
	if err != nil {
		t.Fatalf("HasBOM(short): %v", err)
	}
	if got {
		t.Error("a <3-byte file cannot have a BOM")
	}
}
