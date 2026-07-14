package push

import (
	"strings"
	"testing"
)

// TestCheckFileSize pins the single-file cap boundary that the stream, tabular,
// and text paths now share via checkFileSize. The `exactly at cap` case is the
// one that matters: the guard is `>` (not `>=`), so a file of exactly
// MaxSingleFileBytes is allowed — and asserting that kills the `>`→`>=`
// boundary mutant that survived at all four original call sites, WITHOUT
// materializing a 500 MB fixture (the reason the boundary was untestable before
// this extraction).
func TestCheckFileSize(t *testing.T) {
	cases := []struct {
		name    string
		size    int64
		wantErr bool
	}{
		{"zero", 0, false},
		{"one under cap", MaxSingleFileBytes - 1, false},
		{"exactly at cap is allowed", MaxSingleFileBytes, false},
		{"one over cap is rejected", MaxSingleFileBytes + 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkFileSize("f.bin", c.size)
			if c.wantErr && err == nil {
				t.Fatalf("size=%d: want a size error, got nil", c.size)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("size=%d: want nil, got %v", c.size, err)
			}
		})
	}

	// The over-cap error is the customer-facing sizeError: it must name the
	// offending file and describe the cap.
	err := checkFileSize("big.bin", MaxSingleFileBytes+1)
	if err == nil {
		t.Fatal("over-cap must return an error")
	}
	for _, want := range []string{"big.bin", "cap"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("over-cap error missing %q: %v", want, err)
		}
	}
}
