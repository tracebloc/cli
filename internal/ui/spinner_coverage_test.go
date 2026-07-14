package ui

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewline(t *testing.T) {
	var b bytes.Buffer
	New(&b).Newline()
	if b.String() != "\n" {
		t.Errorf("Newline = %q, want a single newline", b.String())
	}
}

func TestAutoColor(t *testing.T) {
	orig, had := os.LookupEnv("NO_COLOR")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("NO_COLOR", orig)
		} else {
			_ = os.Unsetenv("NO_COLOR")
		}
	})

	// NO_COLOR set → false regardless of the writer.
	_ = os.Setenv("NO_COLOR", "1")
	if autoColor(os.Stdout) {
		t.Error("NO_COLOR set → autoColor must be false")
	}

	// NO_COLOR unset from here on.
	_ = os.Unsetenv("NO_COLOR")
	// A non-*os.File writer (a buffer) → false.
	if autoColor(&bytes.Buffer{}) {
		t.Error("a non-*os.File writer → false")
	}
	// An *os.File that isn't a terminal (a regular temp file) → false.
	f, err := os.CreateTemp(t.TempDir(), "ui-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if autoColor(f) {
		t.Error("a non-terminal *os.File → false")
	}
}

// TestSpinner_Static: color off → the one-line static spinner + a no-op Stop.
func TestSpinner_Static(t *testing.T) {
	var b bytes.Buffer
	s := New(&b).Spinner("working", "hint")
	s.Stop() // static → no-op, idempotent
	if !strings.Contains(b.String(), "working") {
		t.Errorf("static spinner should print its message, got %q", b.String())
	}
}

// TestSpinner_Animated: color on → the animated goroutine. Sleeping past one
// 120ms tick covers run's tick arm (frame advance + redraw); Stop drains the
// goroutine before we read, so it's race-safe under -race.
func TestSpinner_Animated(t *testing.T) {
	var b bytes.Buffer
	s := New(&b, WithColor(true)).Spinner("loading", "please wait")
	time.Sleep(150 * time.Millisecond)
	s.Stop()
	if !strings.Contains(b.String(), "loading") {
		t.Errorf("animated spinner should draw its message, got %q", b.String())
	}
}
