// Package ui renders installer-style terminal output for the tracebloc
// CLI — colored step headers, ✔/⚠/· status lines, dim hints, and a
// branded banner — matching the look of the tracebloc/client one-line
// installer (scripts/lib/common.sh).
//
// Everything goes through a Printer, constructed with New. A Printer
// colorizes only when its writer is a real terminal and NO_COLOR is
// unset; pass WithColor to force the decision (e.g. a --plain flag).
// This mirrors internal/push.NewProgress, which likewise degrades on
// non-TTY output so piped/CI logs stay clean.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fatih/color"
	"golang.org/x/term"
)

// Printer writes installer-style status output to w. Construct it with
// New — the zero value is unusable (w would be nil). Its methods are
// the Go counterparts of common.sh's step/success/warn/info/hint
// helpers.
//
// A Printer is read-only after construction, so it's safe to pass down
// a command's call tree; it is not safe for concurrent writes to the
// same underlying writer (neither is fmt.Fprintf).
type Printer struct {
	w     io.Writer
	color bool
}

// Option customizes a Printer at construction. This is the functional-
// options pattern: rather than a widening New(w, color, ...) signature
// or a separate Config struct, each knob is a small func that mutates
// the Printer. Callers pass only what they care about, and we can add
// options later without breaking New's signature.
type Option func(*Printer)

// WithColor forces colorized output on or off, overriding New's auto-
// detection. Wire a --plain flag to WithColor(false); NO_COLOR is
// already honored by the default detection.
func WithColor(on bool) Option {
	return func(p *Printer) { p.color = on }
}

// New returns a Printer writing to w. By default it colorizes only when
// w is a real terminal and the NO_COLOR env var is unset
// (https://no-color.org). Options are applied after auto-detection, so
// WithColor wins over it.
func New(w io.Writer, opts ...Option) *Printer {
	p := &Printer{w: w, color: autoColor(w)}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// autoColor reports whether to colorize for w: only when NO_COLOR is
// unset AND w is an *os.File pointing at a terminal. A bytes.Buffer
// (tests), a pipe, or a redirect to a file all fail the *os.File +
// IsTerminal check and get plain output — the conservative default,
// same test internal/push.isTTY uses.
func autoColor(w io.Writer) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// paint wraps s in the given SGR attributes when this Printer is in
// color mode, otherwise returns s unchanged. We force-enable the
// fatih/color instance (EnableColor) so OUR p.color decision is
// authoritative — fatih/color's package-level auto-detect keys off
// os.Stdout, not our writer, which would be wrong under tests/pipes.
func (p *Printer) paint(s string, attrs ...color.Attribute) string {
	if !p.color || len(attrs) == 0 {
		return s
	}
	c := color.New(attrs...)
	c.EnableColor()
	return c.Sprint(s)
}

// out is the single write path. The (n, err) result is discarded
// explicitly: a failed write to the terminal (closed pipe, /dev/full)
// can't be acted on mid-render and shouldn't crash the command — the
// exit code is the contract, same rationale as the Fprintf discards in
// internal/cli. errcheck (run by `make ci`) wants the discard explicit.
func (p *Printer) out(format string, a ...any) {
	_, _ = fmt.Fprintf(p.w, format, a...)
}

// Banner prints the branded intro block: a bold-cyan title, a dim rule,
// and an optional subtitle. Mirrors common.sh print_banner.
func (p *Printer) Banner(title, subtitle string) {
	p.out("\n  %s\n", p.paint(title, color.FgCyan, color.Bold))
	p.out("  %s\n", p.paint("────────────────────────────────────────", color.Faint))
	if subtitle != "" {
		p.out("  %s\n", subtitle)
	}
	p.out("\n")
}

// Para prints a normal-weight paragraph, each line indented to match
// Banner/Section bodies. It splits on embedded newlines so multi-line
// prose keeps the indent. Use for explanatory prose — distinct from
// Hintf (dim one-liners) and Infof (· bullets).
func (p *Printer) Para(text string) {
	for _, line := range strings.Split(text, "\n") {
		p.out("  %s\n", line)
	}
}

// Step prints a major-step header: "Step n/total  label" in bold cyan.
// Mirrors common.sh step().
func (p *Printer) Step(n, total int, label string) {
	head := p.paint(fmt.Sprintf("Step %d/%d", n, total), color.FgCyan, color.Bold)
	p.out("\n%s  %s\n", head, p.paint(label, color.Bold))
}

// Successf prints a completed-item line with a green ✔. The trailing
// `f` + (format, args) signature is Go's convention for "takes a format
// string" (cf. fmt.Printf vs fmt.Print).
func (p *Printer) Successf(format string, a ...any) {
	p.out("  %s %s\n", p.paint("✔", color.FgGreen), fmt.Sprintf(format, a...))
}

// Warnf prints a non-blocking warning with a yellow ⚠.
func (p *Printer) Warnf(format string, a ...any) {
	p.out("  %s  %s\n", p.paint("⚠", color.FgYellow), fmt.Sprintf(format, a...))
}

// Infof prints supplementary detail with a dim · bullet.
func (p *Printer) Infof(format string, a ...any) {
	p.out("  %s %s\n", p.paint("·", color.Faint), fmt.Sprintf(format, a...))
}

// Errorf prints a bold-red ✖ error line. Unlike common.sh's error(),
// it does NOT exit — surfacing the message is the UI's job; the command
// still returns an *exitError so main() owns the process exit code.
func (p *Printer) Errorf(format string, a ...any) {
	p.out("  %s\n", p.paint("✖ "+fmt.Sprintf(format, a...), color.FgRed, color.Bold))
}

// Hintf prints dim contextual help (e.g. the line under a prompt).
func (p *Printer) Hintf(format string, a ...any) {
	p.out("  %s\n", p.paint(fmt.Sprintf(format, a...), color.Faint))
}

// PromptHint prints guidance for an interactive prompt: a leading blank
// line for separation, then the hint in cyan so it stands out directly
// above the prompt. Distinct from Hintf (dim) — prompt guidance is meant
// to be read, not skimmed past.
func (p *Printer) PromptHint(format string, a ...any) {
	p.out("\n  %s\n", p.paint(fmt.Sprintf(format, a...), color.FgCyan))
}

// Newline emits a single blank line. Used to detach a closing line or
// call-to-action (e.g. cluster info's "Ready" line, a dry-run / deletion
// result) from the field block above it, so it doesn't get lost.
func (p *Printer) Newline() {
	p.out("\n")
}

// PromptHeader prints a bold-white label before a user-input prompt.
func (p *Printer) PromptHeader(label string) {
	p.out("\n  %s\n", p.paint(label, color.Bold, color.FgWhite))
}

// Section prints a bold section header preceded by a blank line. Used
// to group related Field rows (e.g. "Target cluster").
func (p *Printer) Section(title string) {
	p.out("\n  %s\n", p.paint(title, color.Bold))
}

// Field prints an aligned, dim-labelled key/value row beneath a
// Section: "    label:        value". The label is padded to a fixed
// width so values line up within a section.
func (p *Printer) Field(label, value string) {
	p.out("    %s %s\n", p.paint(fmt.Sprintf("%-14s", label+":"), color.Faint), value)
}
