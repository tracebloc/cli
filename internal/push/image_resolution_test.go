package push

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateMaskResolution mirrors TestValidateImages for the semseg "Mask
// Resolution Validator" preview (cli#352): masks at the target size pass; a
// mask whose resolution differs is rejected (naming it and both sizes, which
// proves the dimensions were decoded — not merely that the file is unreadable);
// a zero-byte / corrupt mask is rejected; the min-size floor applies; and an
// empty set or a 0-expected size is a no-op.
func TestValidateMaskResolution(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, body []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("a_mask.png", pngBytes(t, 32, 32))
	wrong := write("b_mask.png", pngBytes(t, 48, 48))
	zero := write("z_mask.png", nil)

	if err := ValidateMaskResolution(nil, 32, 32, 0, 0); err != nil {
		t.Errorf("empty mask set must pass: %v", err)
	}
	if err := ValidateMaskResolution([]string{good}, 32, 32, 0, 0); err != nil {
		t.Errorf("32x32 mask against a 32x32 target rejected: %v", err)
	}

	// Wrong resolution: rejected, naming the file and BOTH sizes — the decode
	// happened, so this is a genuine size mismatch, not a broken-file fallback.
	err := ValidateMaskResolution([]string{good, wrong}, 32, 32, 0, 0)
	if err == nil {
		t.Fatal("48x48 mask against a 32x32 target must be rejected (cli#352)")
	}
	for _, want := range []string{"b_mask.png", "48x48", "32x32", "resolution"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("mismatch error missing %q: %v", want, err)
		}
	}

	// Zero-byte mask can't be ingested.
	if err := ValidateMaskResolution([]string{good, zero}, 32, 32, 0, 0); err == nil {
		t.Fatal("zero-byte mask must be rejected")
	} else if !strings.Contains(err.Error(), "0 bytes") {
		t.Errorf("zero-byte diagnosis missing: %v", err)
	}

	// 0-expected size skips the resolution comparison (auto-detect path).
	if err := ValidateMaskResolution([]string{good, wrong}, 0, 0, 0, 0); err != nil {
		t.Errorf("no expected size → no resolution rejection: %v", err)
	}

	// Min-size floor applies to masks too (same as images): a below-floor mask
	// is rejected even with no target size.
	tiny := write("tiny_mask.png", pngBytes(t, 16, 16))
	if err := ValidateMaskResolution([]string{tiny}, 0, 0, 32, 32); err == nil {
		t.Fatal("below-floor mask must be rejected")
	} else if !strings.Contains(err.Error(), "minimum") {
		t.Errorf("floor diagnosis missing: %v", err)
	}
}
