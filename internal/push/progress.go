package push

import (
	"io"
	"os"

	"github.com/schollz/progressbar/v3"
	"golang.org/x/term"
)

// Progress is the narrow interface push.Stream + the stage
// orchestrator use to surface per-byte transfer progress to the
// customer. The real implementation wraps schollz/progressbar/v3
// (NewTTYProgress); tests pass a no-op (NoOpProgress) since CI
// runs aren't TTYs and decoupling the test layer from any
// progressbar internals keeps test output clean.
//
// The interface is deliberately byte-oriented (not file-count
// oriented). Tar header bytes flow through too — they're a few
// hundred bytes per file, which makes the progress bar slightly
// over-count the "useful" bytes, but they're real bytes on the
// wire that the customer pays for anyway. The alternative
// (counting only file-body bytes) requires the progress sink to
// know what a tar header looks like, which couples it to the
// streaming format.
type Progress interface {
	// Add records that n bytes have been transferred. Safe to
	// call from any goroutine.
	Add(n int)

	// Finish marks the transfer done. Calling Finish before all
	// bytes have been added is fine — the bar just won't fill to
	// 100%, which is a useful visual signal that something cut
	// the stream short.
	Finish()
}

// NewProgress returns a Progress sink appropriate for the given
// output writer:
//
//   - If out is a TTY (interactive terminal), return a
//     progressbar/v3 that renders an animated bar with bytes/sec
//   - ETA + percentage.
//   - Otherwise return a no-op (CI, redirected output, piped to
//     a file). A spinning bar in a CI log is noise, not a
//     feature.
//
// totalBytes is the expected transfer size (from LocalLayout's
// TotalBytes plus a small tar-overhead margin). The bar normalizes
// against this; if actual transferred bytes overshoot, the bar
// caps at 100% rather than wrapping.
//
// `description` shows up to the left of the bar; "Staging cats_dogs"
// is a typical value.
func NewProgress(out io.Writer, totalBytes int64, description string) Progress {
	if !isTTY(out) {
		return NoOpProgress{}
	}
	return &ttyProgress{
		bar: progressbar.NewOptions64(totalBytes,
			progressbar.OptionSetWriter(out),
			progressbar.OptionSetDescription(description),
			progressbar.OptionShowBytes(true),
			progressbar.OptionShowCount(),
			progressbar.OptionThrottle(100), // ms — keep CPU low
			// Render nothing until the first byte flows. The caller
			// builds this bar up front (data.go), but Stage then prints
			// several setup lines ("Opened a secure channel…",
			// "Preparing the copy…") and waits up to StagePodReadyTimeout
			// for the Pod before a single byte moves. A blank-state 0%
			// bar painted at construction would (a) sit frozen through
			// that multi-minute wait, reading as "stuck", and (b) get
			// clobbered mid-line by those setup lines — schollz redraws
			// with \r while the setup lines are plain \n writes to the
			// same terminal, so the two collide on one line
			// ("…[0s:0s]Opened a secure channel…"). Deferring the first
			// render to the first Add() — which happens inside
			// StreamLayout, after every setup line has printed — lets the
			// bar own its own line cleanly.
			progressbar.OptionSetRenderBlankState(false),
			progressbar.OptionSetWidth(40),
			progressbar.OptionClearOnFinish(),
		),
	}
}

// isTTY reports whether out points at a real terminal. We test for
// *os.File AND golang.org/x/term IsTerminal — a *bytes.Buffer in
// tests isn't *os.File so we return false fast. Wrappers like
// io.MultiWriter that aren't *os.File also return false, which is
// the conservative choice.
func isTTY(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// ttyProgress is the real-bar implementation. The progressbar
// library is itself concurrency-safe under Add(), so the wrapper
// can be a thin pass-through.
type ttyProgress struct {
	bar *progressbar.ProgressBar
}

func (p *ttyProgress) Add(n int) {
	// Explicit-discard the error — progressbar's Add returns an
	// error if the write to the underlying writer fails, which we
	// can't do anything about mid-stream (the customer's terminal
	// going away during a push doesn't affect the staging
	// outcome). Same rationale as the Fprintf discards elsewhere.
	_ = p.bar.Add(n)
}

func (p *ttyProgress) Finish() {
	_ = p.bar.Finish()
}

// NoOpProgress satisfies Progress without doing anything. Used in
// tests and when output is non-TTY. Exported so tests in OTHER
// packages (internal/cli) can also use it.
type NoOpProgress struct{}

func (NoOpProgress) Add(_ int) {}
func (NoOpProgress) Finish()   {}

// progressWriter is an io.Writer that funnels write counts into a
// Progress. Used to wrap the stdin pipe-writer so every byte
// piped to the exec stream counts toward the bar.
//
// Not exported — it's an implementation detail of Stream. Tests
// validate the contract end-to-end (transferred bytes match
// expected) rather than poking the wrapper directly.
type progressWriter struct {
	w io.Writer
	p Progress
}

func (pw *progressWriter) Write(b []byte) (int, error) {
	n, err := pw.w.Write(b)
	if n > 0 {
		pw.p.Add(n)
	}
	return n, err
}
