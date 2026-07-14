package slug

import (
	"strings"
	"testing"
)

// TestDerive_FallbackAndTruncation covers Derive's fallback + collision-suffix
// branches (the paths TestDerive doesn't reach): empty-slug error, fallback use,
// both-empty → raw fallback, and the max-length truncation when a numbered
// suffix must still fit within MaxLabelLength.
func TestDerive_FallbackAndTruncation(t *testing.T) {
	if _, err := Derive("", nil, ""); err == nil {
		t.Error("empty slug + empty fallback must error")
	}
	if got, err := Derive("", nil, "fallback"); err != nil || got != "fallback" {
		t.Errorf("Derive(empty, fallback) = %q, %v; want fallback", got, err)
	}
	// name AND fallback both slugify to empty → base becomes the raw fallback.
	if got, err := Derive("!!!", nil, "@@@"); err != nil || got != "@@@" {
		t.Errorf("Derive(both-empty-slug) = %q, %v; want the raw fallback @@@", got, err)
	}
	// Short-base collision → numbered suffix (end>len(base) clamp).
	if got, err := Derive("Lab", []string{"lab"}, ""); err != nil || got != "lab-2" {
		t.Errorf("Derive collision = %q, %v; want lab-2", got, err)
	}
	// Max-length-base collision → the base is truncated so "-2" fits within cap.
	big := strings.Repeat("a", MaxLabelLength+10)
	got, err := Derive(big, []string{Slugify(big)}, "")
	if err != nil {
		t.Fatalf("Derive(max-length collision): %v", err)
	}
	if len(got) > MaxLabelLength {
		t.Errorf("Derive must keep the handle within MaxLabelLength (%d), got %d: %q", MaxLabelLength, len(got), got)
	}
	if !strings.HasSuffix(got, "-2") {
		t.Errorf("Derive(collision) should carry a -2 suffix, got %q", got)
	}
}
