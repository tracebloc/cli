package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
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

// TestSectionAndField_Plain: the aligned key/value rendering used by
// cluster info + dataset push pre-flight stays clean (no ANSI) and
// surfaces both label and value when color is off.
func TestSectionAndField_Plain(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, WithColor(false))
	p.Section("Target cluster")
	p.Field("release", "ingdemo (chart 1.4.2)")

	out := buf.String()
	if strings.Contains(out, esc) {
		t.Errorf("plain Section/Field emitted ANSI: %q", out)
	}
	for _, want := range []string{"Target cluster", "release:", "ingdemo (chart 1.4.2)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %q", want, out)
		}
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

// TestAction_Plain: the imperative Open/Enter rows surface label + value with no
// ANSI when color is off.
func TestAction_Plain(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, WithColor(false))
	p.Action("Open", "https://x/activate")
	p.Action("Enter", "WDJB-MJHT")

	out := buf.String()
	if strings.Contains(out, esc) {
		t.Errorf("Action emitted ANSI with color off: %q", out)
	}
	for _, want := range []string{"Open", "https://x/activate", "Enter", "WDJB-MJHT"} {
		if !strings.Contains(out, want) {
			t.Errorf("Action output missing %q: %q", want, out)
		}
	}
}

// TestSpinner_StaticWhenPlain: on a non-animating Printer the spinner prints ONE
// static line — no carriage return, no ANSI — and Stop is a no-op.
func TestSpinner_StaticWhenPlain(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, WithColor(false))
	s := p.Spinner("Waiting for your browser…", "Ctrl-C to cancel")
	s.Stop()

	out := buf.String()
	if strings.Contains(out, "\r") || strings.Contains(out, esc) {
		t.Errorf("static spinner emitted \\r or ANSI: %q", out)
	}
	if !strings.Contains(out, "Waiting for your browser…") {
		t.Errorf("static spinner missing message: %q", out)
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("static spinner should print exactly one line, got %d: %q", n, out)
	}
}

// TestSpinner_DrawFormatsElapsed exercises the animated redraw directly (without
// the goroutine, to stay race-free): it renders mm:ss elapsed, the message, and
// the \r + clear-to-EOL control so the line self-overwrites.
func TestSpinner_DrawFormatsElapsed(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, WithColor(true))
	s := &Spinner{p: p, msg: "Waiting for your browser…", hint: "Ctrl-C to cancel",
		start: time.Now().Add(-65 * time.Second)}
	s.draw(spinnerFrames[0])

	out := buf.String()
	if !strings.HasPrefix(out, "\r\x1b[K") {
		t.Errorf("draw should start with a carriage return + clear-to-EOL, got: %q", out)
	}
	if !strings.Contains(out, "1:05") {
		t.Errorf("draw should render 65s as 1:05, got: %q", out)
	}
	if !strings.Contains(out, "Waiting for your browser…") {
		t.Errorf("draw missing message: %q", out)
	}
}
