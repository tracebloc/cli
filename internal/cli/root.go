// Package cli wires the cobra command tree for the tracebloc CLI.
//
// The split is deliberate: cmd/tracebloc/main.go owns ONLY process
// entry + ldflags-injected build metadata. Everything testable lives
// in this package so we can drive commands from tests without going
// through the real os.Args / os.Exit path.
package cli

import (
	"errors"
	"fmt"
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
	// Top-level `doctor` — the environment health sweep the home screen and the
	// env-status lines point at. Shares its RunE with the hidden `cluster
	// doctor` alias (see newDoctorCmd), so there's one diagnostic code path.
	root.AddCommand(newDoctorCmd(false))
	root.AddCommand(newDataCmd())
	// cli#143: one-knob view of how much of this machine tracebloc may use.
	root.AddCommand(newResourcesCmd())
	// RFC-0001 (backend#830): browser sign-in + client provisioning.
	root.AddCommand(newLoginCmd())
	root.AddCommand(newLogoutCmd())
	root.AddCommand(newAuthCmd())
	root.AddCommand(newClientCmd())
	// Top-level offboarding — the inverse of install (RFC-0001 §7.10). NOT under
	// `client` and NOT `client delete --uninstall`: one machine owns one client,
	// so this removes tracebloc from the host and avoids colliding with `data delete`.
	root.AddCommand(newDeleteCmd())
	// F1: apply an update (re-runs the verified installer). The update-check
	// nudge (update_check.go) and the 426 "too old" error both point here.
	root.AddCommand(newUpgradeCmd())

	// Bare `tracebloc` (no subcommand) renders a status-aware home screen —
	// where you stand (signed in? environment live?) then the commands —
	// instead of cobra's raw usage dump. Subcommands and --help are unaffected:
	// cobra dispatches those before this RunE runs. Detection is best-effort and
	// time-bounded (see home.go); it never errors, so bare `tracebloc` exits 0.
	root.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return cmd.Help() // an arg that wasn't a known subcommand
		}
		// The home screen shows a `resources` row only when that command is
		// actually wired on the root — gate on the live tree, never a hardcode,
		// so we never advertise a command that isn't there. #237 (resources) is
		// now merged, so the row renders; the gate keeps us honest if that changes.
		renderHomeScreen(cmd.Context(), printerFor(cmd), hasTopLevelCommand(cmd.Root(), "resources"))
		return nil
	}

	return root
}

// hasTopLevelCommand reports whether a command with the given name is registered
// directly on the root. The home screen uses it to gate rows on commands that
// may not be wired yet (e.g. `resources`, #237), so a row never names a
// command that doesn't exist.
func hasTopLevelCommand(cmd *cobra.Command, name string) bool {
	for _, c := range cmd.Root().Commands() {
		if c.Name() == name {
			return true
		}
	}
	return false
}

// runGroup is the RunE for a parent "group" command (data, cluster, auth,
// client). A bare `tracebloc <group>` prints its help and exits 0; a mistyped
// subcommand (`tracebloc data ingst`) is a hard error (exit 1) with a
// nearest-match suggestion.
//
// Why a RunE at all: a parent command with subcommands but no Run/RunE is
// "not runnable", and cobra short-circuits a non-runnable command to
// flag.ErrHelp BEFORE it validates args (command.go: `if !c.Runnable() {
// return flag.ErrHelp }` precedes ValidateArgs). So `data ingst` printed help
// and exited 0, silently swallowing the typo. Giving the group a RunE makes it
// runnable, so the unknown token reaches here instead. (`cobra.NoArgs` does
// NOT fix this — it's an arg validator, never reached on a non-runnable
// command.) The root command needs none of this: as the parent-less command
// its default legacyArgs already errors on an unknown token. See #75.
func runGroup(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	// Mirror cobra's own "unknown command" wording (what the root emits) so a
	// group typo reads identically. Suggestions come from SuggestionsFor, which
	// skips hidden commands (e.g. the hidden `client list`) and keys off the
	// group's SuggestionsMinimumDistance.
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?"
		for _, s := range suggestions {
			msg += "\n\t" + s
		}
	}
	msg += fmt.Sprintf("\n\nRun '%s --help' for the available commands.", cmd.CommandPath())
	return &exitError{code: exitFailure, err: errors.New(msg)}
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
