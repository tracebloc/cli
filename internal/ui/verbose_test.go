package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestDetailfVerboseGating: Detailf is silent by default and prints only under
// WithVerbose — the --verbose contract (RFC-0001 §8.5: the default stays quiet).
func TestDetailfVerboseGating(t *testing.T) {
	var quiet bytes.Buffer
	New(&quiet).Detailf("hidden %d", 1)
	if quiet.Len() != 0 {
		t.Errorf("Detailf must be silent without WithVerbose, got %q", quiet.String())
	}

	var loud bytes.Buffer
	p := New(&loud, WithVerbose(true))
	if !p.Verbose() {
		t.Error("Verbose() should report true under WithVerbose")
	}
	p.Detailf("shown %d", 2)
	if !strings.Contains(loud.String(), "shown 2") {
		t.Errorf("verbose Detailf should print, got %q", loud.String())
	}
}
