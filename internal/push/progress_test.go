package push

import (
	"bytes"
	"os"
	"testing"
)

// TestNewProgress_NonTTYReturnsNoOp: a non-terminal writer (a buffer,
// a redirected file, a CI log) must get the no-op sink — an animated
// bar in non-interactive output is noise. Covers NewProgress's
// dominant branch + NoOpProgress's methods.
func TestNewProgress_NonTTYReturnsNoOp(t *testing.T) {
	var buf bytes.Buffer
	p := NewProgress(&buf, 1000, "Staging x")
	if _, ok := p.(NoOpProgress); !ok {
		t.Fatalf("NewProgress(non-TTY) = %T, want NoOpProgress", p)
	}
	// Must be safe no-ops that write nothing.
	p.Add(500)
	p.Finish()
	if buf.Len() != 0 {
		t.Errorf("NoOpProgress wrote %d bytes to a non-TTY; want 0", buf.Len())
	}
}

// TestIsTTY_False: neither a bytes.Buffer nor a pipe (a non-terminal
// *os.File) is a TTY. Pins the conservative detection that keeps CI
// output clean.
func TestIsTTY_False(t *testing.T) {
	var buf bytes.Buffer
	if isTTY(&buf) {
		t.Error("isTTY(*bytes.Buffer) = true, want false")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	if isTTY(w) {
		t.Error("isTTY(os.Pipe writer) = true, want false (not a terminal)")
	}
}
