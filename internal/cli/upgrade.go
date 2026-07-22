package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// upgradeInstallerCmd is the one verified update path: re-run the official
// installer. It re-downloads + cosign-verifies the release, replaces the CLI,
// and upgrades the secure environment's services to match — so we never
// re-implement (and risk diverging from) the installer's signature
// verification here. `curl … | bash` (not `bash <(curl …)`) so it works from
// any shell (see the shell-safety fix in the docs).
const upgradeInstallerCmd = "curl -fsSL https://tracebloc.io/i.sh | bash"

// newUpgradeCmd implements `tracebloc upgrade` — the apply step the update
// nudge (update_check.go) and the 426 "too old" error both point at.
func newUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
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

			// Stream the installer straight to the user's terminal, and keep
			// stdin wired so its interactive prompts (sign-in, etc.) still work.
			c := exec.CommandContext(cmd.Context(), "bash", "-c", upgradeInstallerCmd)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return &exitError{code: exitFailure, err: fmt.Errorf(
					"upgrade didn't complete (%w). You can run the installer directly:\n    %s",
					err, upgradeInstallerCmd)}
			}
			return nil
		},
	}
}
