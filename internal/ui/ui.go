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
	"time"

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
	w       io.Writer
	color   bool
	verbose bool
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

// WithVerbose enables verbose output: Detailf lines print only when on. Wire it
// to a --verbose flag / $TRACEBLOC_LOG_LEVEL so the default happy path stays
// quiet (~6 status lines) while a streamed step-by-step view is opt-in.
func WithVerbose(on bool) Option {
	return func(p *Printer) { p.verbose = on }
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

// Detailf prints an indented, dim step-detail line — but ONLY in verbose mode
// (WithVerbose). Use for the streamed device-flow → provision → install trace
// (§8.5 R-verbose) that would be noise by default; the quiet path skips it.
func (p *Printer) Detailf(format string, a ...any) {
	if !p.verbose {
		return
	}
	p.out("    %s %s\n", p.paint("·", color.Faint), fmt.Sprintf(format, a...))
}

// Verbose reports whether this Printer is in verbose mode, so a caller can guard
// expensive detail (e.g. formatting a large value) it would otherwise build then
// discard.
func (p *Printer) Verbose() bool { return p.verbose }

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

// Action prints an imperative instruction row — a bold verb label and its value,
// with no trailing colon: "    Open    https://…". Used for the device-flow
// sign-in steps (Open the URL / Enter the code), where the label is a thing to
// DO, not a field to read (contrast Field, which is dim + colon-terminated).
func (p *Printer) Action(label, value string) {
	p.out("    %s  %s\n", p.paint(fmt.Sprintf("%-5s", label), color.Bold), value)
}

// spinnerFrames are braille cells; cycled, they read as a smooth rotation and
// match the ⠴ vocabulary the tracebloc/client installer already uses.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner is a single self-redrawing status line for a blocking wait: a braille
// frame, a message, and mm:ss elapsed. Start it with Printer.Spinner and end it
// with Stop, which clears the animated line so the caller can print a final ✔/✖
// in its place.
//
// It animates ONLY when the Printer colorizes (a real terminal, no --plain, no
// NO_COLOR). Otherwise — piped output, --plain, CI — it prints one static line
// and never redraws, so logs stay free of `\r`/ANSI. Concurrency: the spinner
// owns the writer between Spinner() and Stop(); the caller must not print in
// between, and Stop must be called from the same goroutine.
type Spinner struct {
	p     *Printer
	msg   string
	hint  string // optional trailing "(hint)", e.g. "Ctrl-C to cancel"
	start time.Time
	stop  chan struct{}
	done  chan struct{}
}

// Spinner starts a live wait line reading "<frame> <msg>  M:SS   (hint)". Pass
// hint == "" to omit the parenthetical. See the Spinner type for the
// animate-vs-static contract; the returned handle is always safe to Stop.
func (p *Printer) Spinner(msg, hint string) *Spinner {
	s := &Spinner{p: p, msg: msg, hint: hint, start: time.Now()}
	if !p.color {
		// Static: one line, no redraw, no clock (nothing would ever update it).
		p.out("  %s %s\n", "·", msg)
		return s
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go s.run()
	return s
}

func (s *Spinner) run() {
	defer close(s.done)
	t := time.NewTicker(120 * time.Millisecond)
	defer t.Stop()
	frame := 0
	s.draw(spinnerFrames[0])
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			frame = (frame + 1) % len(spinnerFrames)
			s.draw(spinnerFrames[frame])
		}
	}
}

func (s *Spinner) draw(frame string) {
	elapsed := time.Since(s.start)
	mm := int(elapsed / time.Minute)
	ss := int(elapsed/time.Second) % 60
	hint := ""
	if s.hint != "" {
		hint = "   (" + s.hint + ")"
	}
	// \r returns to column 0; \033[K clears to end-of-line so a line that shrank
	// (e.g. 10:00 → 9:59 never does, but the hint/msg could) leaves no residue.
	s.p.out("\r\033[K  %s %s  %d:%02d%s",
		s.p.paint(frame, color.FgCyan), s.msg, mm, ss, s.p.paint(hint, color.Faint))
}

// Stop ends the animation and clears the spinner line (animated mode); on a
// static spinner it's a no-op. Idempotent. After Stop the cursor sits at column
// 0 of a cleared line, so the caller's next Successf/Errorf prints in its place.
func (s *Spinner) Stop() {
	if s.stop == nil {
		return // static, or already stopped
	}
	close(s.stop)
	<-s.done
	s.stop = nil
	s.p.out("\r\033[K") // clear the line; the caller prints the outcome next
}
