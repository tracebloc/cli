package cli

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// A failed download must abort rather than run an empty script: with the old
// `curl | bash`, a curl failure left bash reading empty stdin and exiting 0, so
// the command reported success while prepare-host never ran. `set -e` + `curl
// -o <file>` makes the failure propagate — guard both against removal (Bugbot
// #394).
func TestPrepareHostCmdFailsClosedOnDownloadError(t *testing.T) {
	if !strings.Contains(prepareHostInstallerCmd, "set -e") {
		t.Fatalf("prepareHostInstallerCmd must `set -e` so a failed download aborts; got: %q", prepareHostInstallerCmd)
	}
	if !strings.Contains(prepareHostInstallerCmd, "curl") || !strings.Contains(prepareHostInstallerCmd, "-o ") {
		t.Fatalf("prepareHostInstallerCmd must download the installer to a file (curl -o) so curl's exit is checked; got: %q", prepareHostInstallerCmd)
	}
	if !strings.Contains(prepareHostInstallerCmd, "prepare-host") {
		t.Fatalf("prepareHostInstallerCmd should invoke the installer's prepare-host step; got: %q", prepareHostInstallerCmd)
	}
}

// The installer must NOT be fed to bash over a pipe: `curl | bash -s` makes the
// inner bash read its program from the pipe, stealing the installer's stdin so
// any interactive prompt in prepare-host gets EOF. We download and run a file
// instead, leaving stdin on the TTY (Bugbot #394).
func TestPrepareHostCmdDoesNotPipeIntoBash(t *testing.T) {
	if strings.Contains(prepareHostInstallerCmd, "| bash") || strings.Contains(prepareHostInstallerCmd, "|bash") {
		t.Errorf("prepareHostInstallerCmd must not pipe the script into bash (steals the installer's stdin); got: %q", prepareHostInstallerCmd)
	}
}

// The installer must run in the CLI's foreground process group (NOT its own):
// it's interactive and stdin is the TTY, so a backgrounded group would get
// SIGTTIN and hang on any prompt. And WaitDelay must be positive so a child that
// traps signals can't hang Wait forever after a programmatic cancel (Bugbot
// #394). SysProcAttr==nil is portable (the field is *syscall.SysProcAttr on
// every OS), so this stays a single cross-platform test.
func TestPrepareHostCmdStaysInForegroundGroup(t *testing.T) {
	c := prepareHostCmd(context.Background())
	if c.SysProcAttr != nil {
		t.Error("prepareHostCmd must not set SysProcAttr — a separate/background process group breaks interactive TTY prompts (SIGTTIN)")
	}
	if c.WaitDelay <= 0 {
		t.Error("prepareHostCmd must set a positive WaitDelay so Wait can't hang forever after a cancel")
	}
}

// A user abort must be detected as an interrupt even when NotifyContext hasn't
// flipped ctx.Err() yet — bash exits 130 on SIGINT and c.Run() can return first
// (Bugbot #394). A genuine failure (exit 1) must NOT be treated as an interrupt.
func TestPrepareHostInterrupted(t *testing.T) {
	// Cancelled context → interrupt regardless of the run error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !prepareHostInterrupted(ctx, errors.New("boom")) {
		t.Error("a cancelled context must be treated as an interrupt")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available for the exit-code cases")
	}
	// Live context + bash exit 130 (128+SIGINT) → interrupt (the Ctrl-C race).
	if err := exec.Command("bash", "-c", "exit 130").Run(); err == nil {
		t.Fatal("expected a non-nil error from exit 130")
	} else if !prepareHostInterrupted(context.Background(), err) {
		t.Error("exit 130 with a live context must be treated as an interrupt")
	}
	// Live context + a normal failure (exit 1) → NOT an interrupt.
	if err := exec.Command("bash", "-c", "exit 1").Run(); err == nil {
		t.Fatal("expected a non-nil error from exit 1")
	} else if prepareHostInterrupted(context.Background(), err) {
		t.Error("exit 1 must NOT be treated as an interrupt")
	}
}

func TestPrepareHostCmdMetadata(t *testing.T) {
	c := newPrepareHostCmd()
	if c.Use != "prepare-host" {
		t.Errorf("Use = %q, want prepare-host", c.Use)
	}
	// NoArgs: prepare-host takes no positional arguments.
	if err := c.Args(c, []string{"unexpected"}); err == nil {
		t.Error("prepare-host should reject positional arguments (cobra.NoArgs)")
	}
}
