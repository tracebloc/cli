package schema

import (
	"strings"
	"testing"
)

// TestFormatErrors_Tiebreak covers FormatErrors' secondary sort key: when two
// violations share a Path, ordering falls to Message (deterministic output).
func TestFormatErrors_Tiebreak(t *testing.T) {
	out := FormatErrors([]ValidationError{
		{Path: "spec.x", Message: "zeta"},
		{Path: "spec.x", Message: "alpha"},
	})
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "zeta") {
		t.Fatalf("both messages should render:\n%s", out)
	}
	if strings.Index(out, "alpha") > strings.Index(out, "zeta") {
		t.Errorf("same-Path violations must sort by Message (alpha before zeta):\n%s", out)
	}
}

// TestFlattenValidationError_Nil covers the empty-tree guard.
func TestFlattenValidationError_Nil(t *testing.T) {
	if got := flattenValidationError(nil); got != nil {
		t.Errorf("flattenValidationError(nil) = %v, want nil", got)
	}
}
