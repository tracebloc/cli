package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestPrinter_ColorMatrix pins the home-screen render surface — CheckLine,
// CrossLine, WarnLine, Errorf, MenuRow, Infof, PromptHeader, PromptHint — which
// had 0% OWN coverage (the merged profile hid it: internal/cli tests hit these
// transitively but assert only plain substrings, never the glyph or the color).
// For each, in BOTH color modes: the glyph + text must render, and ANSI escapes
// must be present iff color is on. Reuses `esc` from ui_test.go.
func TestPrinter_ColorMatrix(t *testing.T) {
	cases := []struct {
		name    string
		render  func(*Printer)
		present []string // glyph + text, required in both modes
	}{
		{"CheckLine", func(p *Printer) { p.CheckLine("environment online") }, []string{"✓", "environment online"}},
		{"CrossLine", func(p *Printer) { p.CrossLine("can't reach it") }, []string{"✗", "can't reach it"}},
		{"WarnLine", func(p *Printer) { p.WarnLine("starting up") }, []string{"⚠", "starting up"}},
		{"Errorf", func(p *Printer) { p.Errorf("boom %d", 7) }, []string{"✖", "boom 7"}},
		{"MenuRow", func(p *Printer) { p.MenuRow(12, "data ingest", "stage a dataset") }, []string{"·", "data ingest", "stage a dataset"}},
		{"Infof", func(p *Printer) { p.Infof("note %s", "here") }, []string{"·", "note here"}},
		{"PromptHeader", func(p *Printer) { p.PromptHeader("Sign in") }, []string{"Sign in"}},
		{"PromptHint", func(p *Printer) { p.PromptHint("press %s", "enter") }, []string{"press enter"}},
	}
	for _, c := range cases {
		render := func(color bool) string {
			var b bytes.Buffer
			c.render(New(&b, WithColor(color)))
			return b.String()
		}
		t.Run(c.name+"/plain", func(t *testing.T) {
			out := render(false)
			for _, want := range c.present {
				if !strings.Contains(out, want) {
					t.Errorf("plain %s missing %q in %q", c.name, want, out)
				}
			}
			if strings.Contains(out, esc) {
				t.Errorf("plain %s must not emit ANSI escapes: %q", c.name, out)
			}
		})
		t.Run(c.name+"/colored", func(t *testing.T) {
			out := render(true)
			for _, want := range c.present {
				if !strings.Contains(out, want) {
					t.Errorf("colored %s missing %q in %q", c.name, want, out)
				}
			}
			if !strings.Contains(out, esc) {
				t.Errorf("colored %s must emit ANSI escapes, got %q", c.name, out)
			}
		})
	}
}

// TestPrinter_ParaMultiline pins Para's strings.Split multi-line branch
// (0% own): every line is indented two spaces, one per input line.
func TestPrinter_ParaMultiline(t *testing.T) {
	var b bytes.Buffer
	New(&b).Para("line one\nline two")
	got := b.String()
	if got != "  line one\n  line two\n" {
		t.Fatalf("Para multi-line indent = %q, want two 2-space-indented lines", got)
	}
}
