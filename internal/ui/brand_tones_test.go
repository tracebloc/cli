package ui

import (
	"bytes"
	"strings"
	"testing"
)

// renderTone builds a colorized Printer with a pinned mode/bg (independent of the
// runner's COLORTERM/COLORFGBG) and returns what f writes — so the brand-SGR
// assertions below are deterministic on any CI runner.
func renderTone(m colorMode, bg termBg, f func(*Printer)) string {
	var b bytes.Buffer
	p := New(&b, WithColor(true))
	p.mode, p.bg = m, bg
	f(p)
	return b.String()
}

// TestBrandTones_Truecolor pins the exact 24-bit brand SGR the style system
// emits, through the real reachable methods: Section renders primary cyan
// #01a5cc, a MenuRow command renders secondary lime #91e947, and on a light
// terminal both drop to their deep shades (primary.700 / secondary.700) so they
// stay legible on white. These values come straight from the design-system
// tokens — a drift here is a brand regression, caught in CI not the field.
func TestBrandTones_Truecolor(t *testing.T) {
	cases := []struct {
		name   string
		bg     termBg
		render func(*Printer)
		want   string
	}{
		{"heading · dark · cyan #01a5cc", bgDark, func(p *Printer) { p.Section("x") }, "38;2;1;165;204"},
		{"command · dark · lime #91e947", bgDark, func(p *Printer) { p.MenuRow(3, "cmd", "d") }, "38;2;145;233;71"},
		{"heading · light · deep cyan #01637a", bgLight, func(p *Printer) { p.Section("x") }, "38;2;1;99;122"},
		{"command · light · deep lime #578c2b", bgLight, func(p *Printer) { p.MenuRow(3, "cmd", "d") }, "38;2;87;140;43"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := renderTone(modeTrue, c.bg, c.render); !strings.Contains(out, c.want) {
				t.Errorf("want SGR %q in %q", c.want, out)
			}
		})
	}
}

// TestBrandTones_Fallback16: without truecolor, brand hues degrade to the nearest
// ANSI-16 (cyan → 36, green → 32) and never emit a raw 24-bit escape.
func TestBrandTones_Fallback16(t *testing.T) {
	if out := renderTone(mode16, bgDark, func(p *Printer) { p.Section("x") }); !strings.Contains(out, "36") || strings.Contains(out, "38;2") {
		t.Errorf("16-color heading should use cyan (36), no truecolor, got %q", out)
	}
	if out := renderTone(mode16, bgDark, func(p *Printer) { p.MenuRow(3, "cmd", "d") }); !strings.Contains(out, "32") || strings.Contains(out, "38;2") {
		t.Errorf("16-color command should use green (32), no truecolor, got %q", out)
	}
}
