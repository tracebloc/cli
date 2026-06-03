package push

import (
	"os"
	"path/filepath"
	"testing"
)

// mkODDir builds an object_detection dataset dir: labels.csv +
// images/001.jpg, plus annotations/001.xml when withAnnotations.
func mkODDir(t *testing.T, withAnnotations bool) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "labels.csv", "image_label,filename\ncat,001.jpg\n")
	imgs := filepath.Join(dir, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	// JPEG magic bytes; Discover checks extension + size, not decode.
	if err := os.WriteFile(filepath.Join(imgs, "001.jpg"), []byte("\xff\xd8\xff\xe0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if withAnnotations {
		ann := filepath.Join(dir, "annotations")
		if err := os.MkdirAll(ann, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ann, "001.xml"), []byte("<annotation/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestDiscoverObjectDetection: a valid OD layout yields images +
// the annotations sidecar, staged together.
func TestDiscoverObjectDetection(t *testing.T) {
	layout, err := DiscoverObjectDetection(mkODDir(t, true))
	if err != nil {
		t.Fatalf("DiscoverObjectDetection: %v", err)
	}
	if len(layout.Images) != 1 {
		t.Errorf("images = %d, want 1", len(layout.Images))
	}
	if len(layout.Sidecars["annotations"]) != 1 {
		t.Errorf("annotations = %d, want 1", len(layout.Sidecars["annotations"]))
	}
	if got := layout.FileCount(); got != 3 { // labels.csv + image + xml
		t.Errorf("FileCount = %d, want 3", got)
	}
}

// TestDiscoverObjectDetection_MissingAnnotations: OD without an
// annotations/ directory is a clear error.
func TestDiscoverObjectDetection_MissingAnnotations(t *testing.T) {
	if _, err := DiscoverObjectDetection(mkODDir(t, false)); err == nil {
		t.Error("DiscoverObjectDetection without annotations/ returned nil error")
	}
}
