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
// We download-then-execute the installer (installerRunScript, shared with
// prepare-host) rather than `curl … | bash`: piping makes the inner bash read
// its program from the pipe, stealing the installer's stdin so its interactive
// prompts (sign-in, etc.) can't read the TTY. The URL is derived from
// installerURL (doctor.go) so it can't drift from the other installer paths
// (Bugbot #397).
//
// Windows is different: we do NOT self-exec there. A running .exe is locked, so
// install.ps1's Move-Item can't overwrite the very binary we're running, and
// install.ps1 is CLI-only (no environment). So on Windows we print the command
// for the user to run in a fresh shell instead of pretending to self-update.
const upgradeInstallerCmdWindows = "irm https://github.com/tracebloc/cli/releases/latest/download/install.ps1 | iex"

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
	// Download-then-execute the verified installer (installerRunScript, shared
	// with prepare-host): its `set -e`+`curl -o` fails closed on a bad download,
	// and running a file (not a pipe) keeps the installer's stdin on the TTY. The
	// manual hint reuses installCmd (doctor.go), the shared bootstrap idiom, so
	// the URL has a single source.
	return upgradePlan{
		exec:   true,
		name:   "bash",
		args:   []string{"-c", installerRunScript("")},
		manual: installCmd,
	}
}

// isUpgradeCommand reports whether the executed command is `tracebloc upgrade`,
// so main can skip the post-command update nudge (the still-running process
// carries its old compile-time version after swapping its own binary, and would
// otherwise nudge about the very release it just installed).
func isUpgradeCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == upgradeCmdName
}

// SkipUpdateNudge reports whether the post-command update nudge (and the cache
// write it triggers) must be suppressed for the command that just ran. Exported
// for main, which owns the nudge call. Suppressed after:
//   - upgrade: the running process swapped its own binary and now carries the
//     stale compile-time version — it would nudge about the release it just
//     installed.
//   - delete: offboarding removed the CLI and (on the default path) wiped
//     ~/.tracebloc, so the nudge is nonsensical and its cache write would
//     resurrect the just-offboarded host data dir (Bugbot #397).
func SkipUpdateNudge(cmd *cobra.Command) bool {
	return isUpgradeCommand(cmd) || isDeleteCommand(cmd)
}

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
			ctx := cmd.Context()
			c := exec.CommandContext(ctx, plan.name, plan.args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				// User aborted (Ctrl-C) or the parent context was cancelled: exit
				// quietly with 130 like prepare-host, not a scary "upgrade didn't
				// complete — retry" (Bugbot #397).
				if installerRunInterrupted(ctx, err) {
					return &exitError{code: exitInterrupted}
				}
				return &exitError{code: exitFailure, err: fmt.Errorf(
					"upgrade didn't complete (%w). You can run the installer directly:\n    %s",
					err, plan.manual)}
			}
			return nil
		},
	}
}
