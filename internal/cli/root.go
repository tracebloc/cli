// Package cli wires the cobra command tree for the tracebloc CLI.
//
// The split is deliberate: cmd/tracebloc/main.go owns ONLY process
// entry + ldflags-injected build metadata. Everything testable lives
// in this package so we can drive commands from tests without going
// through the real os.Args / os.Exit path.
package cli

import (
	"github.com/spf13/cobra"
)

// BuildInfo carries metadata that main.go pulls from -ldflags. We
// pass it through the constructor rather than reading from a global
// in this package so the cobra command tree stays a pure function of
// its inputs — useful for tests that want to inject a fake version.
type BuildInfo struct {
	Version   string
	GitSHA    string
	BuildDate string
}

// NewRootCmd returns the top-level `tracebloc` command with every
// subcommand wired in. Callers (main.go, tests) execute the returned
// command to dispatch.
//
// We intentionally don't expose a package-level singleton — cobra's
// global state has historically been a source of test interference,
// and constructing fresh trees per call is cheap.
func NewRootCmd(info BuildInfo) *cobra.Command {
	root := &cobra.Command{
		Use:   "tracebloc",
		Short: "tracebloc — declarative data ingestion for your cluster",
		Long: `tracebloc is the customer-facing CLI for the tracebloc declarative
ingestion path. It wraps the same POST /internal/submit-ingestion-run
protocol the tracebloc/ingestor Helm chart uses, so any cluster running
the parent tracebloc/client chart can be targeted directly from a
developer's workstation.

The dominant workflow:

  tracebloc dataset push ./my-data \
    --table cats_dogs_train \
    --category image_classification \
    --intent train \
    --label-column label

The CLI handles cluster discovery (via kubeconfig), staging the data
on the cluster's shared PVC, submitting the ingestion request,
watching the resulting Job, and reporting the outcome. Customers never
touch Helm, never edit YAML, never run kubectl cp manually.

Today this binary implements only ` + "`version`" + ` and ` + "`completion`" + ` —
see https://github.com/tracebloc/client/issues/147 for the v0.1
roadmap. Subsequent phases land subcommands incrementally.`,

		// Silence cobra's auto-printed errors + usage on every error;
		// we already print structured errors in handlers, and the
		// double-printing is noisy. Per-command Use docs stay
		// available via `tracebloc help <cmd>`.
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	// Subcommands. New phases append here.
	root.AddCommand(newVersionCmd(info))

	return root
}
