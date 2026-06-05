// Package cli wires the cobra command tree for the tracebloc CLI.
//
// The split is deliberate: cmd/tracebloc/main.go owns ONLY process
// entry + ldflags-injected build metadata. Everything testable lives
// in this package so we can drive commands from tests without going
// through the real os.Args / os.Exit path.
package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/ui"
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
		Short: "tracebloc — interactive data ingestion for your cluster",
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

This binary implements the full v0.1 ingestion path: ` + "`dataset push`" + `
(the dominant workflow above), ` + "`ingest validate`" + ` for a local
schema check, ` + "`cluster info`" + ` for discovery diagnostics, plus
` + "`version`" + ` and ` + "`completion`" + `. See
https://github.com/tracebloc/client/issues/147 for the v0.1 roadmap and
what's planned next.`,

		// Silence cobra's auto-printed errors + usage on every error;
		// we already print structured errors in handlers, and the
		// double-printing is noisy. Per-command Use docs stay
		// available via `tracebloc help <cmd>`.
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	// Persistent flags apply to the root and every subcommand. --plain
	// disables color + decorative output; ui.New also auto-plains on a
	// non-TTY and honors $NO_COLOR, so this is the explicit opt-out for
	// CI / log capture where stdout might still look like a terminal.
	root.PersistentFlags().Bool("plain", false,
		"disable color and decorative output (also honors $NO_COLOR)")

	// Subcommands. New phases append here.
	root.AddCommand(newVersionCmd(info))
	root.AddCommand(newIngestCmd())
	root.AddCommand(newClusterCmd())
	root.AddCommand(newDatasetCmd())

	// Bare `tracebloc` (no subcommand) renders a friendly home screen
	// instead of cobra's raw usage dump. Subcommands and --help are
	// unaffected — cobra dispatches those before this RunE runs.
	root.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return cmd.Help() // an arg that wasn't a known subcommand
		}
		p := printerFor(cmd)
		p.Banner("tracebloc", "interactive data ingestion for your cluster")
		p.Section("Get started")
		p.Infof("tracebloc dataset push            — stage + ingest a dataset interactively (or use --help to see flags)")
		p.Infof("tracebloc dataset list            — list datasets ingested in the cluster")
		p.Infof("tracebloc dataset rm <table>      — delete a pushed dataset (its table + files)")
		p.Infof("tracebloc cluster info            — check the CLI can reach your cluster")
		p.Infof("tracebloc ingest validate f.yaml  — validate an ingest.yaml locally")
		p.Newline()
		p.Hintf("Add --help to any command for the full flag list.")
		return nil
	}

	return root
}

// printerFor builds a ui.Printer for a command's stdout, honoring the
// persistent --plain flag. Color / TTY / NO_COLOR auto-detection lives
// in ui.New; --plain just forces it off. Commands call this at the top
// of their RunE.
func printerFor(cmd *cobra.Command) *ui.Printer {
	return printerForWriter(cmd, cmd.OutOrStdout())
}

// printerForWriter is printerFor for an explicit writer — used by
// dataset push's --output-json mode, which routes human output to
// stderr so stdout carries only the JSON result.
func printerForWriter(cmd *cobra.Command, w io.Writer) *ui.Printer {
	if plain, _ := cmd.Flags().GetBool("plain"); plain {
		return ui.New(w, ui.WithColor(false))
	}
	return ui.New(w)
}
