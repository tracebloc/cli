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
	"os"

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
	err := cli.NewRootCmd(cli.BuildInfo{
		Version:   version,
		GitSHA:    gitSHA,
		BuildDate: buildDate,
	}).Execute()
	if err == nil {
		return
	}

	// Map command-defined exit codes through. Handlers that want a
	// specific exit code (e.g. `ingest validate` returns 2 for
	// schema violations, 3 for parse errors) return a *cli.ExitError
	// the package exports; everything else gets the default 1.
	os.Exit(cli.ExitCodeFromError(err))
}
