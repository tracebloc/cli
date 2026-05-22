package push

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// imgcDir builds a valid image_classification layout under t.TempDir()
// and returns its absolute path. Used as the happy-path baseline that
// individual negative-case tests mutate.
func imgcDir(t *testing.T, withImages ...string) string {
	t.Helper()
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "labels.csv"),
		[]byte("image_id,label\n001.jpg,cat\n002.jpg,dog\n"), 0o644); err != nil {
		t.Fatalf("write labels.csv: %v", err)
	}
	imagesDir := filepath.Join(root, "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatalf("mkdir images/: %v", err)
	}
	if len(withImages) == 0 {
		withImages = []string{"001.jpg", "002.jpg"}
	}
	for _, name := range withImages {
		// 100 bytes of stub data per image — keeps the total-bytes
		// math predictable in TestDiscover_TotalBytesSum without
		// generating real image headers.
		if err := os.WriteFile(filepath.Join(imagesDir, name),
			make([]byte, 100), 0o644); err != nil {
			t.Fatalf("write image %s: %v", name, err)
		}
	}
	return root
}

func TestDiscover_HappyPath(t *testing.T) {
	root := imgcDir(t)
	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Root == "" || !filepath.IsAbs(got.Root) {
		t.Errorf("Root = %q, want non-empty absolute path", got.Root)
	}
	if filepath.Base(got.LabelsCSV) != "labels.csv" {
		t.Errorf("LabelsCSV basename = %q, want labels.csv", filepath.Base(got.LabelsCSV))
	}
	if len(got.Images) != 2 {
		t.Errorf("len(Images) = %d, want 2", len(got.Images))
	}
}

func TestDiscover_TotalBytesSum(t *testing.T) {
	// Two 100-byte images + the inline labels.csv string (39 bytes:
	// "image_id,label\n001.jpg,cat\n002.jpg,dog\n"). 100+100+39 = 239.
	// This pins the pre-cluster size summary the dry-run output
	// prints — if we ever undercount, customers see "0 bytes"
	// pre-push and panic.
	root := imgcDir(t)
	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	const want = int64(100 + 100 + 39)
	if got.TotalBytes != want {
		t.Errorf("TotalBytes = %d, want %d", got.TotalBytes, want)
	}
}

func TestDiscover_AcceptsAllImageExtensions(t *testing.T) {
	// Mirror the chart's FileTypeValidator(images) defaults — if a
	// customer's image-set has .png + .webp, both should stage.
	root := imgcDir(t, "a.jpg", "b.jpeg", "c.png", "d.webp", "e.JPG")
	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got.Images) != 5 {
		t.Errorf("len(Images) = %d, want 5 (case-insensitive); names=%v",
			len(got.Images), got.Images)
	}
}

func TestDiscover_SkipsNonImageFiles(t *testing.T) {
	// .DS_Store, thumbnails.db, sibling .yaml etc. should be
	// silently skipped — not error-out, not stage. The "no image
	// files" diagnostic only fires when *zero* accepted files
	// remain after filtering.
	root := imgcDir(t, "real.jpg")
	if err := os.WriteFile(filepath.Join(root, "images", ".DS_Store"),
		make([]byte, 50), 0o644); err != nil {
		t.Fatalf("write .DS_Store: %v", err)
	}
	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got.Images) != 1 {
		t.Errorf("len(Images) = %d, want 1; .DS_Store should be filtered", len(got.Images))
	}
}

func TestDiscover_MissingLabelsCSV(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "images"), 0o755); err != nil {
		t.Fatalf("mkdir images: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "images", "a.jpg"),
		make([]byte, 100), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	_, err := Discover(root)
	if err == nil {
		t.Fatal("Discover returned nil error; expected missing-labels error")
	}
	if !strings.Contains(err.Error(), "missing labels.csv") {
		t.Errorf("error = %q, want it to mention missing labels.csv", err)
	}
}

func TestDiscover_MissingImagesDir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "labels.csv"),
		[]byte("x"), 0o644); err != nil {
		t.Fatalf("write labels.csv: %v", err)
	}
	_, err := Discover(root)
	if err == nil {
		t.Fatal("Discover returned nil error; expected missing-images-dir error")
	}
	if !strings.Contains(err.Error(), "missing images/") {
		t.Errorf("error = %q, want it to mention missing images/", err)
	}
}

func TestDiscover_NoAcceptedImageExtensions(t *testing.T) {
	// images/ exists but only contains .gif and .bmp — neither
	// in our accept-set. Customer should see "no image files"
	// pointing at the accepted extensions, not a successful walk
	// with len(Images)==0 that then succeeds the dry-run.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "labels.csv"),
		[]byte("x"), 0o644); err != nil {
		t.Fatalf("write labels.csv: %v", err)
	}
	imagesDir := filepath.Join(root, "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatalf("mkdir images: %v", err)
	}
	for _, n := range []string{"old.gif", "old.bmp"} {
		if err := os.WriteFile(filepath.Join(imagesDir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	_, err := Discover(root)
	if err == nil {
		t.Fatal("Discover returned nil error; expected no-images error")
	}
	if !strings.Contains(err.Error(), "no image files") {
		t.Errorf("error = %q, want it to mention no image files", err)
	}
}

func TestDiscover_LabelsCSVIsDirectory(t *testing.T) {
	// A directory literally named "labels.csv" — os.Stat succeeds,
	// so without the IsDir guard the pre-flight would accept it and
	// PR-b's tar stream would fail confusingly. Symmetric with the
	// images/ check. Bugbot flagged the missing guard on PR #8.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "labels.csv"), 0o755); err != nil {
		t.Fatalf("mkdir labels.csv/: %v", err)
	}
	imagesDir := filepath.Join(root, "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatalf("mkdir images/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imagesDir, "a.jpg"),
		make([]byte, 100), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	_, err := Discover(root)
	if err == nil {
		t.Fatal("Discover returned nil error; expected labels.csv-is-a-directory error")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("error = %q, want it to mention 'is a directory'", err)
	}
}

func TestDiscover_NotADirectory(t *testing.T) {
	// Customer passes a path to a single file instead of a dir.
	// This is a common autocomplete-mistake (tab-completing
	// into the file). Should be caught early with a clear error.
	root := t.TempDir()
	filePath := filepath.Join(root, "looks-like-data.tar")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := Discover(filePath)
	if err == nil {
		t.Fatal("Discover returned nil error; expected not-a-directory error")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error = %q, want it to mention not a directory", err)
	}
}

func TestDiscover_OverSingleFileCap(t *testing.T) {
	// Use a fake-size pattern: create a real small file but assert
	// the cap logic by exercising the human-readable error format
	// at the boundary. We can't easily generate a 500MB+ file in
	// CI without slowing the suite — instead pin the human-bytes
	// formatter (which is what the customer sees) via its own
	// boundary test below, and exercise sizeError() directly.
	got := sizeError("images/big.jpg", 600*1024*1024, MaxSingleFileBytes).Error()
	for _, want := range []string{"images/big.jpg", "600.00 MiB", "500.00 MiB", "v0.2", "cloud-source"} {
		if !strings.Contains(got, want) {
			t.Errorf("sizeError missing %q in: %s", want, got)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	// Boundary check: the formatter is what surfaces in every
	// diagnostic, so a regression here makes the error messages
	// unreadable for the customer. Pin a few representative values.
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{1024 * 1024, "1.00 MiB"},
		{1024 * 1024 * 1024, "1.00 GiB"},
		{500 * 1024 * 1024, "500.00 MiB"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.in); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
