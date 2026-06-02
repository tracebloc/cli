package push

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// TestParseTargetSize covers the --target-size flag parser: the
// documented WxH form, the W,H convenience form, and the rejection
// cases (missing dimension, non-integer, non-positive, wrong arity).
func TestParseTargetSize(t *testing.T) {
	cases := []struct {
		in      string
		w, h    int
		wantErr bool
	}{
		{"512x512", 512, 512, false},
		{"640x480", 640, 480, false},
		{"512,512", 512, 512, false},
		{"1x1", 1, 1, false},
		{"512", 0, 0, true},
		{"512x", 0, 0, true},
		{"x512", 0, 0, true},
		{"0x512", 0, 0, true},
		{"-4x512", 0, 0, true},
		{"512x512x512", 0, 0, true},
		{"abcxdef", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, c := range cases {
		w, h, err := ParseTargetSize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseTargetSize(%q) = (%d,%d,nil), want error", c.in, w, h)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTargetSize(%q) unexpected error: %v", c.in, err)
			continue
		}
		if w != c.w || h != c.h {
			t.Errorf("ParseTargetSize(%q) = (%d,%d), want (%d,%d)", c.in, w, h, c.w, c.h)
		}
	}
}

// TestDetectImageSize_PNG: a real (generated) PNG's header is decoded
// to its true dimensions. Pins the auto-detect path used when the
// customer doesn't pass --target-size.
func TestDetectImageSize_PNG(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "img.png")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 320, 200))); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	w, h, err := DetectImageSize(p)
	if err != nil {
		t.Fatalf("DetectImageSize: %v", err)
	}
	if w != 320 || h != 200 {
		t.Errorf("DetectImageSize = (%d,%d), want (320,200)", w, h)
	}
}

// TestDetectImageSize_Unsupported: a non-image (or unregistered
// format) returns an error so the caller falls back to the ingestor
// default + advises --target-size, rather than silently using a
// bogus size.
func TestDetectImageSize_Unsupported(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(p, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DetectImageSize(p); err == nil {
		t.Error("DetectImageSize on non-image returned nil error; want a decode error")
	}
}
