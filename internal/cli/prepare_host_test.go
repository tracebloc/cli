package cli

import (
	"context"
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

// The installer runs as a `curl | bash -s` pipeline. If we only kill the
// top-level bash on cancel, the curl and the privileged prepare-host child keep
// running detached after the CLI has reported failure and exited. Guard that
// the command is created in its own process group with a group-killing Cancel
// (Bugbot #394).
func TestPrepareHostCmdRunsInOwnProcessGroup(t *testing.T) {
	c := prepareHostCmd(context.Background())
	if c.SysProcAttr == nil || !c.SysProcAttr.Setpgid {
		t.Error("prepareHostCmd must set SysProcAttr.Setpgid so the installer pipeline gets its own process group")
	}
	if c.Cancel == nil {
		t.Error("prepareHostCmd must set Cancel to group-kill the pipeline when the context is cancelled")
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
