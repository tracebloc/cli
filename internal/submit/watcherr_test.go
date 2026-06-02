package submit

import (
	"errors"
	"fmt"
	"testing"
)

// TestWatchError_UnwrapAndClassify pins WatchError's wrapping: it
// reports the inner message, unwraps to the cause (so errors.Is works
// through it), and IsWatchError recognizes it while rejecting other
// errors. This drives the orchestrator's exit-9 (ingest-side) vs
// exit-8 (submit-side) mapping, so the classification must be exact.
func TestWatchError_UnwrapAndClassify(t *testing.T) {
	inner := errors.New("pod log stream broke")
	we := &WatchError{Err: inner}

	if we.Error() != inner.Error() {
		t.Errorf("Error() = %q, want %q", we.Error(), inner.Error())
	}
	// errors.Is traverses Unwrap — covers WatchError.Unwrap.
	if !errors.Is(we, inner) {
		t.Error("errors.Is(WatchError, inner) = false; Unwrap not wired")
	}
	// And through an extra wrap layer.
	wrapped := fmt.Errorf("watching ingestor Job: %w", we)
	if !IsWatchError(wrapped) {
		t.Error("IsWatchError(wrapped WatchError) = false, want true")
	}
	if IsWatchError(errors.New("unrelated")) {
		t.Error("IsWatchError(plain error) = true, want false")
	}
}
