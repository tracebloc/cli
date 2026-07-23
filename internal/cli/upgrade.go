package cli

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
)

// upgradeCmdName is the command that re-runs the installer; the update nudge
// skips itself after this command (the running process swaps its own binary and
// is stale by design afterwards — see MaybeNotifyUpdate / main).
const upgradeCmdName = "upgrade"

// The verified update command per OS. On Linux/macOS we re-run the official
// installer ourselves: it re-downloads + cosign-verifies the release, replaces
// the CLI, and upgrades the secure environment's services to match — so we never
// re-implement (and risk diverging from) the installer's signature verification.
// The short `curl … | bash` form (not `bash <(curl …)`) works from any shell
// (see the shell-safety fix in the docs).
//
// Windows is different: we do NOT self-exec there. A running .exe is locked, so
// install.ps1's Move-Item can't overwrite the very binary we're running, and
// install.ps1 is CLI-only (no environment). So on Windows we print the command
// for the user to run in a fresh shell instead of pretending to self-update.
const (
	upgradeInstallerCmdUnix    = "curl -fsSL https://tracebloc.io/i.sh | bash"
	upgradeInstallerCmdWindows = "irm https://github.com/tracebloc/cli/releases/latest/download/install.ps1 | iex"
)

// upgradePlan is how `upgrade` proceeds on a given OS: either exec the installer
// (Unix) or just show the user a command to run (Windows). manual is the
// copy-paste command in both cases.
type upgradePlan struct {
	exec   bool     // run the installer ourselves (Unix) vs. print instructions (Windows)
	name   string   // exec program (when exec)
	args   []string // exec args (when exec)
	manual string   // command to show the user
}

// upgradePlanFor returns the upgrade plan for a GOOS. Split out from runtime.GOOS
// so it's testable on any host.
func upgradePlanFor(goos string) upgradePlan {
	if goos == "windows" {
		return upgradePlan{exec: false, manual: upgradeInstallerCmdWindows}
	}
	// `-o pipefail` so a failed `curl` fails the whole pipeline: without it the
	// trailing `bash` exits 0 on empty stdin and we'd report a successful upgrade
	// having installed nothing.
	return upgradePlan{
		exec:   true,
		name:   "bash",
		args:   []string{"-o", "pipefail", "-c", upgradeInstallerCmdUnix},
		manual: upgradeInstallerCmdUnix,
	}
}

// isUpgradeCommand reports whether the executed command is `tracebloc upgrade`,
// so main can skip the post-command update nudge (the still-running process
// carries its old compile-time version after swapping its own binary, and would
// otherwise nudge about the very release it just installed).
func isUpgradeCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == upgradeCmdName
}

// SkipUpdateNudge reports whether the update nudge should be suppressed for the
// command that just ran. Exported for main, which owns the nudge call.
func SkipUpdateNudge(cmd *cobra.Command) bool { return isUpgradeCommand(cmd) }

// newUpgradeCmd implements `tracebloc upgrade` — the apply step the update
// nudge (update_check.go) and the 426 "too old" error both point at.
func newUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   upgradeCmdName,
		Short: "Update tracebloc to the latest release",
		Long: `Updates tracebloc to the latest release by re-running the official
installer. It verifies signatures (cosign) and replaces the CLI; on Linux/macOS
it also upgrades your secure environment's services to match, so the CLI and the
environment never drift apart. On Windows it prints the command to update the
CLI (a running executable can't replace itself, so you run it in a fresh shell).
Safe to run anytime; safe to re-run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := printerFor(cmd)
			plan := upgradePlanFor(runtime.GOOS)
			p.Newline()

			if !plan.exec {
				// Windows: guide, don't self-exec (see upgradePlanFor). Running
				// the installer in a fresh shell means no tracebloc process holds
				// the binary, so install.ps1 can replace it.
				p.Para("To update tracebloc on Windows, run this in a new PowerShell window:")
				p.Para("    " + plan.manual)
				p.Newline()
				return nil
			}

			p.Para("Updating tracebloc — re-running the installer (verifies signatures, then updates the CLI and your secure environment).")
			p.Newline()

			// Stream the installer straight to the user's terminal, and keep
			// stdin wired so its interactive prompts (sign-in, etc.) still work.
			c := exec.CommandContext(cmd.Context(), plan.name, plan.args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return &exitError{code: exitFailure, err: fmt.Errorf(
					"upgrade didn't complete (%w). You can run the installer directly:\n    %s",
					err, plan.manual)}
			}
			return nil
		},
	}
}
