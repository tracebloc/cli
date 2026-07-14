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

// mkSemsegDir builds a semantic_segmentation dataset dir: labels.csv (with the
// required mask_id column) + images/001.jpg, plus masks/001_mask.png when
// withMasks (the shipped <image>_mask.png convention, #196).
func mkSemsegDir(t *testing.T, withMasks bool) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "labels.csv", "image_label,filename,mask_id\ncat,001.jpg,001_mask\n")
	imgs := filepath.Join(dir, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgs, "001.jpg"), []byte("\xff\xd8\xff\xe0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if withMasks {
		masks := filepath.Join(dir, "masks")
		if err := os.MkdirAll(masks, 0o755); err != nil {
			t.Fatal(err)
		}
		// PNG magic bytes; discoverSidecarFiles keys on the extension, not decode.
		if err := os.WriteFile(filepath.Join(masks, "001_mask.png"), []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestDiscoverSemanticSegmentation: a valid semseg layout yields images + the
// masks sidecar, staged together.
func TestDiscoverSemanticSegmentation(t *testing.T) {
	layout, err := DiscoverSemanticSegmentation(mkSemsegDir(t, true))
	if err != nil {
		t.Fatalf("DiscoverSemanticSegmentation: %v", err)
	}
	if len(layout.Images) != 1 {
		t.Errorf("images = %d, want 1", len(layout.Images))
	}
	if len(layout.Sidecars["masks"]) != 1 {
		t.Errorf("masks = %d, want 1", len(layout.Sidecars["masks"]))
	}
	if got := layout.FileCount(); got != 3 { // labels.csv + image + mask
		t.Errorf("FileCount = %d, want 3", got)
	}
}

// TestDiscoverSemanticSegmentation_MissingMasks: semseg without a masks/
// directory is a clear error.
func TestDiscoverSemanticSegmentation_MissingMasks(t *testing.T) {
	if _, err := DiscoverSemanticSegmentation(mkSemsegDir(t, false)); err == nil {
		t.Error("DiscoverSemanticSegmentation without masks/ returned nil error")
	}
}
