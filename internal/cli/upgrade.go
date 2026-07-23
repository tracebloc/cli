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

// The one verified update path per OS: re-run the official installer. It
// re-downloads + cosign-verifies the release, replaces the CLI, and upgrades the
// secure environment's services to match — so we never re-implement (and risk
// diverging from) the installer's signature verification here. Unix uses the
// short `curl … | bash` form (not `bash <(curl …)`) so it works from any shell
// (see the shell-safety fix in the docs); Windows re-runs install.ps1 via
// PowerShell, matching the documented Windows install command in the README.
const (
	upgradeInstallerCmdUnix    = "curl -fsSL https://tracebloc.io/i.sh | bash"
	upgradeInstallerCmdWindows = "irm https://github.com/tracebloc/cli/releases/latest/download/install.ps1 | iex"
)

// upgradeCommand returns the exec name+args that re-run the installer for the
// current OS, plus the copy-paste command to show a user if it fails.
func upgradeCommand() (name string, args []string, human string) {
	return upgradeCommandFor(runtime.GOOS)
}

// upgradeCommandFor is upgradeCommand split out by GOOS so it's testable on any
// host. Windows is a shipped platform (install.ps1) and has no bash, so it must
// not be handed the Unix installer.
func upgradeCommandFor(goos string) (name string, args []string, human string) {
	if goos == "windows" {
		return "powershell", []string{"-NoProfile", "-Command", upgradeInstallerCmdWindows}, upgradeInstallerCmdWindows
	}
	return "bash", []string{"-c", upgradeInstallerCmdUnix}, upgradeInstallerCmdUnix
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
		Short: "Update tracebloc (CLI + your secure environment) to the latest release",
		Long: `Updates tracebloc to the latest release by re-running the official
installer. It verifies signatures (cosign), replaces the CLI, and upgrades your
secure environment's services to match — the CLI and the environment move as a
set, so they never drift apart. Safe to run anytime; safe to re-run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := printerFor(cmd)
			p.Newline()
			p.Para("Updating tracebloc — re-running the installer (verifies signatures, then updates the CLI and your secure environment).")
			p.Newline()

			name, args, human := upgradeCommand()
			// Stream the installer straight to the user's terminal, and keep
			// stdin wired so its interactive prompts (sign-in, etc.) still work.
			c := exec.CommandContext(cmd.Context(), name, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return &exitError{code: exitFailure, err: fmt.Errorf(
					"upgrade didn't complete (%w). You can run the installer directly:\n    %s",
					err, human)}
			}
			return nil
		},
	}
}
