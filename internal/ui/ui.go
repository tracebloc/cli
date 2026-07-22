// Package ui renders installer-style terminal output for the tracebloc
// CLI — colored step headers, ✔/⚠/· status lines, and dim hints —
// matching the look of the tracebloc/client one-line installer
// (scripts/lib/common.sh).
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
	"runtime"
	"strconv"
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
	color   bool      // colorized at all? (paint + the spinner gate on this)
	mode    colorMode // none / 16-color / 24-bit truecolor — how brand hues render
	bg      termBg    // dark or light terminal — picks the legible shade of each hue
	verbose bool
}

// colorMode is how much color the terminal supports and wants. It drives whether
// a brand hue renders as exact 24-bit hex, the nearest ANSI-16 fallback, or not
// at all (NO_COLOR / pipe / TERM=dumb / --plain).
type colorMode uint8

const (
	modeNone colorMode = iota // no color
	mode16                    // 16-color ANSI (the terminal's own cyan/green/…)
	modeTrue                  // 24-bit truecolor (exact brand hex)
)

// termBg is the terminal's background, dark or light. Bright brand shades that
// pop on dark are unreadable on white, so light terminals get the deep shades
// (primary.700 / secondary.700) instead. Detected best-effort from COLORFGBG;
// defaults to dark, the developer norm.
type termBg uint8

const (
	bgDark termBg = iota
	bgLight
)

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
	return func(p *Printer) {
		if on {
			// Force color on even for a non-TTY writer (tests, an explicit
			// --color): pick truecolor when the environment advertises it, else
			// 16-color. NO_COLOR/TTY no longer gate — the caller has decided.
			p.mode = envMode()
		} else {
			p.mode = modeNone
		}
		p.color = p.mode != modeNone
	}
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
	m := detectMode(w)
	p := &Printer{w: w, mode: m, color: m != modeNone, bg: detectBg()}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// detectMode reports how to colorize for w. It stays plain (modeNone) when
// NO_COLOR is set, TERM=dumb, or w is not a terminal — a bytes.Buffer (tests),
// a pipe, or a redirect all fail the *os.File + IsTerminal check, the same
// conservative default internal/push.isTTY uses. On a real terminal it upgrades
// to truecolor when the environment advertises it (envMode).
func detectMode(w io.Writer) colorMode {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return modeNone
	}
	if os.Getenv("TERM") == "dumb" {
		return modeNone
	}
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return modeNone
	}
	return envMode()
}

// envMode picks the richest palette the environment advertises, ignoring TTY /
// NO_COLOR (those are decided upstream). COLORTERM=truecolor|24bit is the de-facto
// truecolor signal (set by iTerm2, VS Code, Windows Terminal, most modern terms);
// everything else falls back to the terminal's own 16-color cyan/green.
func envMode() colorMode {
	switch os.Getenv("COLORTERM") {
	case "truecolor", "24bit":
		return modeTrue
	}
	return mode16
}

// detectBg reports the terminal background so brand hues render in a shade that
// stays legible on it. COLORFGBG (set by Konsole, rxvt, iTerm2, …) is "fg;bg"
// where a trailing 7 or 15 is a light background. Unknown → dark, the dev norm.
func detectBg() termBg {
	v := os.Getenv("COLORFGBG")
	if v == "" {
		return bgDark
	}
	parts := strings.Split(v, ";")
	switch parts[len(parts)-1] {
	case "7", "15":
		return bgLight
	}
	return bgDark
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

// ── Brand tones ──────────────────────────────────────────────────────────────
// Each semantic role maps to a tone: the exact 24-bit hex on a dark and on a
// light terminal (from the design-system primary/secondary ramps), plus the
// nearest ANSI-16 attribute for terminals without truecolor. Meaning never rests
// on hue alone — headings/commands also carry Bold and alerts carry a distinct
// glyph — so the output still reads under NO_COLOR or for a colour-blind reader.
type tone struct {
	dark, light string          // 24-bit hex (no '#'); "" = structural, no hue
	c16         color.Attribute // ANSI-16 fallback
	bold        bool
	underline   bool
}

var (
	toneHeading = tone{"01a5cc", "01637a", color.FgCyan, true, false}   // primary — structure/headings
	toneCommand = tone{"91e947", "578c2b", color.FgGreen, true, false}  // secondary — the thing to run
	toneDesc    = tone{"a7ed6c", "578c2b", color.FgGreen, false, false} // soft lime — supporting text (decision B)
	toneAccent  = tone{"01a5cc", "01637a", color.FgCyan, false, false}  // cyan prompt guidance
	toneGo      = tone{"91e947", "578c2b", color.FgGreen, false, false} // ✔ / ● — "good/go", brand lime (decision A)
	toneWarn    = tone{"ffc62b", "8a6a00", color.FgYellow, false, false}
	toneErr     = tone{"f64c4c", "c0271f", color.FgRed, true, false}  // ✖ error (bold)
	toneErrSoft = tone{"f64c4c", "c0271f", color.FgRed, false, false} // ✗ offline (lighter, non-bold)
	toneLabel   = tone{"8e8e8e", "6b6b6b", color.Faint, false, false} // dim metadata labels
)

// hue renders s in a brand tone: exact 24-bit hex (dark or light shade) when the
// terminal supports truecolor, the ANSI-16 fallback otherwise, and plain text when
// color is off. This is the single brand-colour chokepoint.
func (p *Printer) hue(s string, t tone) string {
	if p.mode == modeNone {
		return s
	}
	var c *color.Color
	if p.mode == modeTrue && t.dark != "" {
		h := t.dark
		if p.bg == bgLight {
			h = t.light
		}
		r, g, b := rgbOf(h)
		c = color.RGB(r, g, b)
	} else {
		c = color.New(t.c16)
	}
	if t.bold {
		c.Add(color.Bold)
	}
	if t.underline {
		c.Add(color.Underline)
	}
	c.EnableColor()
	return c.Sprint(s)
}

// rgbOf splits a 6-digit hex ("01a5cc") into r,g,b; a malformed value yields
// black rather than a panic.
func rgbOf(h string) (int, int, int) {
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return 0, 0, 0
	}
	return int(v>>16) & 0xff, int(v>>8) & 0xff, int(v) & 0xff
}

// Command styles a command the user should run in the action tone (lime, bold),
// for use inline in prose — e.g. the "run `tracebloc doctor`" tail of a status
// line. MenuRow already applies this tone to whole command rows.
func (p *Printer) Command(s string) string { return p.hue(s, toneCommand) }

// out is the single write path. The (n, err) result is discarded
// explicitly: a failed write to the terminal (closed pipe, /dev/full)
// can't be acted on mid-render and shouldn't crash the command — the
// exit code is the contract, same rationale as the Fprintf discards in
// internal/cli. errcheck (run by `make ci`) wants the discard explicit.
func (p *Printer) out(format string, a ...any) {
	_, _ = fmt.Fprintf(p.w, format, a...)
}

// Para prints a normal-weight paragraph, each line indented to match
// Section bodies. It splits on embedded newlines so multi-line
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
	head := p.hue(fmt.Sprintf("Step %d/%d", n, total), toneHeading)
	p.out("\n%s  %s\n", head, p.paint(label, color.Bold))
}

// PromptStep prints the header for one guided-setup question: a blank line, then
// "Step n of total · question" in the heading tone (cyan bold) — the dominant
// line. Any supporting hint (Hintf/Infof) and the input prompt render beneath
// it, so the question reads first and the guidance is clearly secondary.
func (p *Printer) PromptStep(n, total int, question string) {
	p.out("\n  %s\n", p.hue(fmt.Sprintf("Step %d of %d · %s", n, total, question), toneHeading))
}

// Successf prints a completed-item line with a green ✔. The trailing
// `f` + (format, args) signature is Go's convention for "takes a format
// string" (cf. fmt.Printf vs fmt.Print).
func (p *Printer) Successf(format string, a ...any) {
	p.out("  %s %s\n", p.hue("✔", toneGo), fmt.Sprintf(format, a...))
}

// Warnf prints a non-blocking warning with a yellow ⚠.
func (p *Printer) Warnf(format string, a ...any) {
	p.out("  %s  %s\n", p.hue("⚠", toneWarn), fmt.Sprintf(format, a...))
}

// Infof prints supplementary detail with a dim · bullet.
func (p *Printer) Infof(format string, a ...any) {
	p.out("  %s %s\n", p.paint("·", color.Faint), fmt.Sprintf(format, a...))
}

// MenuRow prints a home-screen command row: a dim · bullet, the command padded to
// width (normal weight, so it's the primary element), a 4-space gap, then the
// description dimmed — so the command clearly stands out against it. Used by the
// home screen's command buckets.
func (p *Printer) MenuRow(width int, cmd, desc string) {
	p.out("  %s %s    %s\n", p.paint("·", color.Faint), p.hue(fmt.Sprintf("%-*s", width, cmd), toneCommand), p.hue(desc, toneDesc))
}

// CheckLine, CrossLine, and WarnLine render the status-aware home screen's
// two-axis state block ("Signed in as …", "Secure environment … · Online"),
// each led by a colored glyph. They exist alongside Successf/Errorf/Warnf
// (which use the heavier ✔/✖ and, for Warnf, a wider gap) because the home
// screen's locked copy pins the lighter ✓/✗ glyphs and needs all three lines
// single-spaced so they align in one column.

// CheckLine prints an affirmative status line led by a green ✓.
func (p *Printer) CheckLine(format string, a ...any) {
	p.out("  %s %s\n", p.hue("✓", toneGo), fmt.Sprintf(format, a...))
}

// CrossLine prints a negative status line led by a red ✗ (lighter and
// non-bold, unlike Errorf's ✖) — e.g. "not signed in" or an offline environment.
func (p *Printer) CrossLine(format string, a ...any) {
	p.out("  %s %s\n", p.hue("✗", toneErrSoft), fmt.Sprintf(format, a...))
}

// WarnLine prints a caution status line led by a yellow ⚠, single-spaced to
// align with CheckLine/CrossLine (Warnf double-spaces for standalone warnings).
func (p *Printer) WarnLine(format string, a ...any) {
	p.out("  %s %s\n", p.hue("⚠", toneWarn), fmt.Sprintf(format, a...))
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
	p.out("  %s\n", p.hue("✖ "+fmt.Sprintf(format, a...), toneErr))
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
	p.out("\n  %s\n", p.hue(fmt.Sprintf(format, a...), toneAccent))
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

// Section prints a section header preceded by a blank line — the screen's
// structural spine, so it's cyan + bold (the heading tone). Used to group
// related Field rows (e.g. "Target cluster", "Your secure environment").
func (p *Printer) Section(title string) {
	p.out("\n  %s\n", p.hue(title, toneHeading))
}

// Field prints an aligned, dim-labelled key/value row beneath a
// Section: "    label:        value". The label is padded to a fixed
// width so values line up within a section.
func (p *Printer) Field(label, value string) {
	p.out("    %s %s\n", p.hue(fmt.Sprintf("%-14s", label+":"), toneLabel), value)
}

// Stat prints an aligned "label   value" row with a dimmed, fixed-width label,
// so a short block of them lines up. Unlike Field's compact 14-col key, Stat
// fits full-sentence labels (e.g. the resources view's two lines).
func (p *Printer) Stat(label, value string) {
	p.out("  %s  %s\n", p.hue(fmt.Sprintf("%-42s", label), toneLabel), value)
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
	// Static line (no redraw) when we can't animate cleanly: non-color/non-TTY, or
	// Windows — the redraw writes raw \r\033[K + SGR straight to the writer, which
	// fatih/color's Windows VT-enable path never sees, so legacy consoles would show
	// escape garbage every tick. Windows gets the clean one-liner instead.
	if !p.color || runtime.GOOS == "windows" {
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
