package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/spf13/cobra"
)

// prepareHostUserRe validates the researcher username before we pass it to the
// installer as TB_PREPARE_USER. Conservative Linux-username shape: starts
// alphanumeric, then letters/digits/._- (usermod quotes it, but reject nonsense
// early with a clear error rather than a confusing failure deep in the installer).
var prepareHostUserRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,31}$`)

// prepareHostInstallerCmd runs the official installer's admin-only prepare-host
// step. Like `tracebloc upgrade`, this deliberately delegates to the verified
// installer (cosign-checked) instead of re-implementing any privileged host prep
// in the CLI — the privileged surface stays in one audited place.
//
// We download the installer to a temp file and run THAT, rather than
// `curl | bash -s`. Two reasons, both Bugbot #394:
//   - stdin: with `curl | bash -s`, the inner bash reads its *program* from the
//     pipe, so the installer's stdin is no longer the terminal. Any interactive
//     prompt in prepare-host (e.g. which non-admin user gets runtime access)
//     would get EOF. Running a downloaded file leaves stdin on the TTY.
//   - fail-closed: `set -e` + `curl -o` makes a failed download (network/DNS/HTTP
//     error) abort with a non-zero status instead of silently running nothing.
//     (`curl | bash` swallowed this — bash read empty stdin and exited 0.)
//
// The temp file is removed on exit.
const prepareHostInstallerCmd = `set -e
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -fsSL https://tracebloc.io/i.sh -o "$tmp"
bash "$tmp" prepare-host`

// prepareHostManualHint is the copy-pasteable command we show if the automated
// run fails. Built from installCmd (doctor.go) — the single shared bootstrap
// idiom — so a URL/idiom change updates every hint at once (Bugbot #394); we
// only append the prepare-host subcommand. installCmd uses process substitution
// (bash <(curl …)), which keeps stdin on the terminal for interactive prompts.
const prepareHostManualHint = installCmd + " prepare-host"

// prepareHostCmd builds the exec.Cmd that runs the installer.
//
// It deliberately does NOT put the installer in its own process group. The
// installer is interactive (prepare-host may prompt, e.g. which non-admin user
// gets runtime access), and stdin is the TTY — a child in a *background* process
// group that reads the terminal gets SIGTTIN and hangs (Bugbot #394). Staying in
// the CLI's foreground group means prompts work AND a terminal Ctrl-C delivers
// SIGINT to the whole pipeline (the `bash -c`, the `curl`, and the `bash "$tmp"`
// prepare-host child) in one go — no orphaned privileged work.
//
// WaitDelay bounds teardown on a *programmatic* cancel (parent shutdown / a
// SIGTERM to the CLI alone): CommandContext SIGKILLs the process and, after the
// delay, force-closes the I/O pipes so Wait can't block forever behind a child
// that traps signals. We rely on the default SIGKILL rather than a custom
// SIGINT-only Cancel (which a privileged child could ignore, hanging Wait).
func prepareHostCmd(ctx context.Context) *exec.Cmd {
	c := exec.CommandContext(ctx, "bash", "-c", prepareHostInstallerCmd)
	c.WaitDelay = 5 * time.Second
	return c
}

// prepareHostInterrupted reports whether the installer run ended because the user
// aborted, so the caller can exit quietly (130) instead of framing it as a failed
// install. ctx.Err() catches a cancel the signal handler already propagated — but
// on a terminal Ctrl-C the child can die and c.Run() can return BEFORE
// NotifyContext flips ctx.Err() (a race), so also treat bash's 130 (128+SIGINT)
// exit as an interrupt (Bugbot #394).
func prepareHostInterrupted(ctx context.Context, runErr error) bool {
	if ctx.Err() != nil {
		return true
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return ee.ExitCode() == exitInterrupted
	}
	return false
}

// newPrepareHostCmd builds `tracebloc prepare-host` — the one-time administrator
// step that readies a machine so a non-admin user can then install tracebloc
// with no root at all.
func newPrepareHostCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prepare-host [researcher-username]",
		Short: "Prepare this machine so a non-admin user can install tracebloc (run once, as an administrator)",
		Long: `Prepares a host that a non-admin user can't install on directly.

Run this ONCE, as an administrator, on a machine where the person who will use
tracebloc has no root or sudo — a shared server, an HPC login node. It installs
the container runtime and its prerequisites.

Pass that person's username to also grant them container-runtime (docker-group)
access, so they can then install tracebloc at Tier 0 with no administrator
rights at all:

    sudo tracebloc prepare-host alice

Without a username it installs only the runtime + prerequisites and tells you
how to grant a user access afterwards. NOTE: the username is the RESEARCHER who
will use tracebloc — not you, the admin running this.

It re-runs the official installer's prepare-host step (verified with cosign). It
does NOT create your secure environment or sign you in — it only prepares the
host, so it's safe to run on a shared machine. Safe to re-run.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := printerFor(cmd)
			p.Newline()
			ctx := cmd.Context()
			c := prepareHostCmd(ctx)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if len(args) == 1 {
				user := args[0]
				if !prepareHostUserRe.MatchString(user) {
					return &exitError{code: exitBadInput, err: fmt.Errorf("invalid username %q — expected a Linux username (letters, digits, '.', '_', '-')", user)}
				}
				// The installer reads TB_PREPARE_USER to pick who gets docker-group
				// access. Pass it through the environment (not the command string), so
				// it can't be shell-interpreted; the installer quotes it for usermod.
				c.Env = append(os.Environ(), "TB_PREPARE_USER="+user)
				p.Para(fmt.Sprintf("Preparing this host and granting %s container-runtime access — re-running the installer's prepare-host step (needs administrator rights once).", user))
			} else {
				p.Para("Preparing this host — re-running the installer's prepare-host step (installs the container runtime and prerequisites; needs administrator rights once). Pass a researcher's username to also grant them access: tracebloc prepare-host <username>")
			}
			p.Newline()
			if err := c.Run(); err != nil {
				// User aborted (Ctrl-C) or the parent context was cancelled: exit
				// quietly with 130 like the other cancellable paths, not a scary
				// "prepare-host didn't complete — retry" (Bugbot #394).
				if prepareHostInterrupted(ctx, err) {
					return &exitError{code: exitInterrupted}
				}
				return &exitError{code: exitFailure, err: fmt.Errorf("prepare-host didn't complete (%w). You can run the installer directly:\n    %s", err, prepareHostManualHint)}
			}
			return nil
		},
	}
}
