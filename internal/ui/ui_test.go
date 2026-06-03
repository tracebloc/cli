package ui

import (
	"bytes"
	"strings"
	"testing"
)

// esc is the ANSI SGR prefix every color escape starts with. Its
// presence/absence in the output is how we assert color vs plain
// without pinning specific color codes (which could change).
const esc = "\x1b["

// TestNew_BufferDefaultsToPlain: a non-*os.File writer (a bytes.Buffer)
// can't be a terminal, so New auto-detects no-color. This is the path
// every test + CI run takes.
func TestNew_BufferDefaultsToPlain(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf)
	p.Successf("done")

	if strings.Contains(buf.String(), esc) {
		t.Errorf("default Printer on a buffer emitted ANSI codes: %q", buf.String())
	}
	for _, want := range []string{"✔", "done"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("plain output missing %q: %q", want, buf.String())
		}
	}
}

// TestWithColorFalse_OmitsANSI: forcing color off yields clean text
// across the helper set, while still emitting the structural content.
func TestWithColorFalse_OmitsANSI(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, WithColor(false))
	p.Banner("tracebloc", "declarative ingestion")
	p.Step(1, 3, "Discover cluster")
	p.Warnf("PVC is %s", "ReadWriteOnce")
	p.Hintf("pass --namespace to override")

	if strings.Contains(buf.String(), esc) {
		t.Errorf("WithColor(false) still emitted ANSI: %q", buf.String())
	}
	for _, want := range []string{"tracebloc", "Step 1/3", "Discover cluster", "ReadWriteOnce"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output missing %q: %q", want, buf.String())
		}
	}
}

// TestWithColorTrue_EmitsANSI: forcing color on wraps text in SGR codes
// even though the writer (a buffer) is not a real terminal — proving
// p.color is authoritative over fatih/color's package-level auto-detect.
func TestWithColorTrue_EmitsANSI(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, WithColor(true))
	p.Successf("staged %d files", 5)

	out := buf.String()
	if !strings.Contains(out, esc) {
		t.Errorf("WithColor(true) emitted no ANSI codes: %q", out)
	}
	if !strings.Contains(out, "staged 5 files") {
		t.Errorf("colored output missing message: %q", out)
	}
}

// TestNoColorEnv_DefaultsPlain exercises the NO_COLOR branch of
// autoColor: with it set, a freshly-constructed Printer stays plain.
// (t.Setenv restores the prior value when the test ends.)
func TestNoColorEnv_DefaultsPlain(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	New(&buf).Successf("ok")

	if strings.Contains(buf.String(), esc) {
		t.Errorf("NO_COLOR set but ANSI emitted: %q", buf.String())
	}
}
