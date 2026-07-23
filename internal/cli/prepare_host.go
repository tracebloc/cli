package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"
)

// prepareHostInstallerCmd runs the official installer's admin-only prepare-host
// step. Like `tracebloc upgrade`, this deliberately delegates to the verified
// installer (cosign-checked) instead of re-implementing any privileged host prep
// in the CLI — the privileged surface stays in one audited place.
// `set -o pipefail` is essential: without it, if curl fails (network/DNS/HTTP
// error) the downstream `bash -s` still gets empty stdin and exits 0, so the
// whole pipeline — and c.Run() — succeeds while prepare-host never ran (Bugbot
// #394). With pipefail, curl's non-zero propagates and we surface the failure.
const prepareHostInstallerCmd = "set -o pipefail; curl -fsSL https://tracebloc.io/i.sh | bash -s -- prepare-host"

// prepareHostCmd builds the exec.Cmd that runs the installer pipeline. It puts
// the pipeline in its own process group (Setpgid) and, on context cancel
// (Ctrl-C / parent shutdown), signals the WHOLE group rather than just the
// top-level `bash` (Bugbot #394). Without this, cancelling the CLI killed only
// the immediate `bash -c`, leaving the `curl` and the `bash -s` prepare-host
// child — which does privileged host prep — running detached after the CLI had
// already reported failure and exited. Group-signalling stops the privileged
// work when the user aborts.
func prepareHostCmd(ctx context.Context) *exec.Cmd {
	c := exec.CommandContext(ctx, "bash", "-c", prepareHostInstallerCmd)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		// Negative PID → signal the entire process group we created above.
		return syscall.Kill(-c.Process.Pid, syscall.SIGINT)
	}
	return c
}

// newPrepareHostCmd builds `tracebloc prepare-host` — the one-time administrator
// step that readies a machine so a non-admin user can then install tracebloc
// with no root at all.
func newPrepareHostCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prepare-host",
		Short: "Prepare this machine so a non-admin user can install tracebloc (run once, as an administrator)",
		Long: `Prepares a host that a non-admin user can't install on directly.

Run this ONCE, as an administrator, on a machine where the person who will use
tracebloc has no root or sudo — a shared server, an HPC login node. It installs
the container runtime and its prerequisites and grants that user access to it;
afterwards they install tracebloc with no administrator rights at all.

It re-runs the official installer's prepare-host step (verified with cosign). It
does NOT create your secure environment or sign you in — it only prepares the
host, so it's safe to run on a shared machine. Safe to re-run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := printerFor(cmd)
			p.Newline()
			p.Para("Preparing this host — re-running the installer's prepare-host step (installs the container runtime and prerequisites; needs administrator rights once).")
			p.Newline()
			c := prepareHostCmd(cmd.Context())
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return &exitError{code: exitFailure, err: fmt.Errorf("prepare-host didn't complete (%w). You can run the installer directly:\n    %s", err, prepareHostInstallerCmd)}
			}
			return nil
		},
	}
}
