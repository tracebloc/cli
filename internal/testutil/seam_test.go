package testutil

import "testing"

// The seam must hold the stub for the test body and be restored by Cleanup —
// including nested swaps of the SAME seam, which must unwind LIFO back to the
// original.
func TestSwapSeam_SwapsAndRestores(t *testing.T) {
	seam := func() string { return "original" }

	t.Run("inner", func(t *testing.T) {
		SwapSeam(t, &seam, func() string { return "outer-stub" })
		if got := seam(); got != "outer-stub" {
			t.Fatalf("seam() = %q after swap, want %q", got, "outer-stub")
		}

		t.Run("nested", func(t *testing.T) {
			SwapSeam(t, &seam, func() string { return "inner-stub" })
			if got := seam(); got != "inner-stub" {
				t.Fatalf("seam() = %q after nested swap, want %q", got, "inner-stub")
			}
		})

		// The nested subtest's Cleanup has run: back to the outer stub.
		if got := seam(); got != "outer-stub" {
			t.Fatalf("seam() = %q after nested cleanup, want %q (LIFO restore broken)", got, "outer-stub")
		}
	})

	if got := seam(); got != "original" {
		t.Fatalf("seam() = %q after all cleanups, want %q (restore broken)", got, "original")
	}
}

// Non-function seams (durations, limits) are first-class too — T is any.
func TestSwapSeam_NonFunctionValue(t *testing.T) {
	limit := 100

	t.Run("inner", func(t *testing.T) {
		SwapSeam(t, &limit, 5)
		if limit != 5 {
			t.Fatalf("limit = %d after swap, want 5", limit)
		}
	})

	if limit != 100 {
		t.Fatalf("limit = %d after cleanup, want 100", limit)
	}
}
