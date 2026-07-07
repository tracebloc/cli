// Package cli wires the cobra command tree for the tracebloc CLI.
//
// The split is deliberate: cmd/tracebloc/main.go owns ONLY process
// entry + ldflags-injected build metadata. Everything testable lives
// in this package so we can drive commands from tests without going
// through the real os.Args / os.Exit path.
package cli

import (
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/api"
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
	// Record the CLI version for the User-Agent sent on every backend request
	// (RFC-0001 §14 R11 / backend#888): "tracebloc-cli/<ver> (<os>/<arch>)".
	api.SetUserAgent(info.Version)

	root := &cobra.Command{
		Use:   "tracebloc",
		Short: "tracebloc — connect this machine to tracebloc and manage its data",
		Long: `The tracebloc CLI connects machines to tracebloc as clients and
manages the datasets that models train on. Your data stays on your
infrastructure — models from other collaborators come to it, once you
approve them.

Two kinds of commands:

  Your account (sign in first):   login, logout, auth, client
  This machine's client:          data, cluster

A typical first session:

  tracebloc login                  # sign in or create your account (browser)
  tracebloc data ingest ./my-data  # stage a dataset into your client
  tracebloc data list              # see what's in the cluster

The CLI finds your cluster through your kubeconfig, stages data onto
the cluster's shared storage, and reports progress as it goes. No
Helm, no YAML, no kubectl needed.`,

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
	// --verbose streams the per-step detail (device-flow → provision → install)
	// that's hidden by default; also enabled by $TRACEBLOC_LOG_LEVEL=debug. The
	// default output stays quiet — a handful of ✔ lines (RFC-0001 §8.5).
	root.PersistentFlags().Bool("verbose", false,
		"stream detailed step-by-step progress (also via $TRACEBLOC_LOG_LEVEL=debug)")

	// Subcommands. New phases append here.
	root.AddCommand(newVersionCmd(info))
	root.AddCommand(newIngestCmd())
	root.AddCommand(newClusterCmd())
	root.AddCommand(newDataCmd())
	// RFC-0001 (backend#830): browser sign-in + client provisioning.
	root.AddCommand(newLoginCmd())
	root.AddCommand(newLogoutCmd())
	root.AddCommand(newAuthCmd())
	root.AddCommand(newClientCmd())
	// Top-level offboarding — the inverse of install (RFC-0001 §7.10). NOT under
	// `client` and NOT `client delete --uninstall`: one machine owns one client,
	// so this removes tracebloc from the host and avoids colliding with `data delete`.
	root.AddCommand(newDeleteCmd())

	// Bare `tracebloc` (no subcommand) renders a friendly home screen
	// instead of cobra's raw usage dump. Subcommands and --help are
	// unaffected — cobra dispatches those before this RunE runs.
	root.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return cmd.Help() // an arg that wasn't a known subcommand
		}
		p := printerFor(cmd)
		p.Banner("tracebloc", "connect this machine to tracebloc and manage its data")
		p.Section("Get started")
		p.Infof("tracebloc login                  — sign in to tracebloc (browser)")
		p.Infof("tracebloc data ingest ./data     — stage a dataset into your client")
		p.Infof("tracebloc data list              — datasets in the cluster")
		p.Infof("tracebloc data delete <table>    — delete an ingested dataset")
		p.Infof("tracebloc client list            — your clients and their status")
		p.Infof("tracebloc cluster doctor         — diagnose connection issues")
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
	var opts []ui.Option
	if plain, _ := cmd.Flags().GetBool("plain"); plain {
		opts = append(opts, ui.WithColor(false))
	}
	if verboseRequested(cmd) {
		opts = append(opts, ui.WithVerbose(true))
	}
	return ui.New(w, opts...)
}

// verboseRequested reports whether the user asked for verbose output, via the
// --verbose flag or $TRACEBLOC_LOG_LEVEL (debug/trace/verbose). The flag wins;
// the env var lets a headless / scripted run opt in without editing the command.
func verboseRequested(cmd *cobra.Command) bool {
	if v, err := cmd.Flags().GetBool("verbose"); err == nil && v {
		return true
	}
	switch strings.ToLower(os.Getenv("TRACEBLOC_LOG_LEVEL")) {
	case "debug", "trace", "verbose":
		return true
	}
	return false
}
