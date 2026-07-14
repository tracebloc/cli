package schema

import (
	"errors"
	"fmt"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// TestErrorsAsAndUnwrap covers the two tiny error-chain helpers directly: unwrap
// (wrapping → inner, non-wrapping → nil) and errors_as walking a chain that
// holds no *ValidationError (→ false), exercising the full unwrap walk to nil.
func TestErrorsAsAndUnwrap(t *testing.T) {
	inner := errors.New("in")
	if got := unwrap(fmt.Errorf("w: %w", inner)); got != inner {
		t.Errorf("unwrap(%%w-wrapped) = %v, want the inner error", got)
	}
	if got := unwrap(errors.New("plain")); got != nil {
		t.Errorf("unwrap(non-wrapping) = %v, want nil", got)
	}

	var target *jsonschema.ValidationError
	chain := fmt.Errorf("a: %w", fmt.Errorf("b: %w", errors.New("c")))
	if errors_as(chain, &target) {
		t.Error("errors_as must be false when the chain holds no *ValidationError")
	}
}
