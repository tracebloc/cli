// Command tracebloc is the customer-facing CLI for the tracebloc
// declarative ingestion path.
//
// The entire command tree lives in internal/cli; main.go is a thin
// wrapper so the build tags + version-injection via -ldflags stay in
// one place and the testable command code stays importable.
//
// Build with version metadata injected:
//
//	go build -ldflags "-X main.version=0.1.0 \
//	  -X main.gitSHA=$(git rev-parse --short HEAD) \
//	  -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
//	  ./cmd/tracebloc
//
// Without -ldflags the binary still works; it just reports
// "dev" / "unknown" / "unknown" for the three fields. That's the right
// signal during local `go run`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tracebloc/cli/internal/cli"
)

// Populated at build time via -ldflags. Default values are what shows
// up when a developer runs `go build ./cmd/tracebloc` without any
// -ldflags — useful for local hacking but distinct from a release
// build so support can tell them apart.
var (
	version   = "dev"
	gitSHA    = "unknown"
	buildDate = "unknown"
)

func main() {
	// Wire SIGINT / SIGTERM into the cobra root command's context.
	// Long-running operations (e.g. push.Stage in `dataset push`)
	// propagate ctx down through every k8s API call, so cancelling
	// here triggers ctx.Done() → in-flight HTTP cancels → defers
	// fire in normal stack unwind → orphan Pod gets cleaned up.
	//
	// Without this wire, Ctrl-C goes straight through Go's runtime
	// signal handler and exits the process WITHOUT running defers
	// — which silently broke the SIGINT-safe cleanup contract in
	// push.Stage's docstring. Bugbot flagged this on PR-b.
	//
	// signal.NotifyContext (Go 1.16+) is the stdlib pattern for
	// this. The stop func unregisters the handler so a *second*
	// SIGINT does the normal hard-kill — important for the case
	// where the cleanup itself hangs (e.g. API server unreachable
	// even past the 30s cleanup deadline). The customer's second
	// Ctrl-C should always work.
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	executed, err := cli.NewRootCmd(cli.BuildInfo{
		Version:   version,
		GitSHA:    gitSHA,
		BuildDate: buildDate,
	}).ExecuteContextC(ctx)

	// F1: after the command runs, a quiet once-a-day nudge if a newer release
	// exists (best-effort; silent on dev builds, off a terminal, in CI, or with
	// TRACEBLOC_NO_UPDATE_CHECK). Printed before the error handling so on success
	// it's the last line, and on failure the error stays the most prominent one.
	// Skipped right after `tracebloc upgrade`: that command swaps this very
	// binary, so the still-running old process would otherwise nudge about the
	// release it just installed.
	if !cli.SkipUpdateNudge(executed) {
		cli.MaybeNotifyUpdate(version, os.Stderr)
	}

	if err == nil {
		return
	}

	// Print the error to stderr before exiting. The root command
	// sets SilenceErrors: true to keep cobra from prepending its
	// own "Error: ..." line on top of structured handler output
	// — but that puts the burden on us to surface the error
	// message ourselves. Without this, every non-schema-violation
	// failure (file-read errors, YAML parse errors, schema-compile
	// errors) exits non-zero with NO message to the customer.
	//
	// Handlers that have already printed their own diagnostic
	// (e.g. `ingest validate` prints per-violation lines) signal
	// "silent" by returning an exitError with a nil inner — see
	// cli.IsSilentError for the contract.
	if !cli.IsSilentError(err) {
		// Explicit-discard the writer error: if stderr itself is
		// gone (closed pipe, redirected to /dev/full, etc.) we
		// still need to exit non-zero — the error message is
		// best-effort, the exit code is the contract.
		_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
	}

	// Map command-defined exit codes through. Handlers that want a
	// specific exit code (e.g. `ingest validate` returns 2 for
	// schema violations, 3 for parse errors) return a *cli.ExitError
	// the package exports; everything else gets the default 1.
	os.Exit(cli.ExitCodeFromError(err))
}
