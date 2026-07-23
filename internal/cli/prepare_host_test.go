package cli

import (
	"context"
	"strings"
	"testing"
)

// The prepare-host command shells out to `bash -c prepareHostInstallerCmd`. If
// curl fails, a plain `curl | bash` pipeline still exits 0 (bash reads empty
// stdin), so the command would report success while nothing ran. `set -o
// pipefail` is what makes curl's failure propagate — guard it against removal
// (Bugbot #394).
func TestPrepareHostCmdUsesPipefail(t *testing.T) {
	if !strings.HasPrefix(prepareHostInstallerCmd, "set -o pipefail;") {
		t.Fatalf("prepareHostInstallerCmd must start with `set -o pipefail;` so a curl failure isn't swallowed; got: %q", prepareHostInstallerCmd)
	}
	if !strings.Contains(prepareHostInstallerCmd, "prepare-host") {
		t.Fatalf("prepareHostInstallerCmd should invoke the installer's prepare-host step; got: %q", prepareHostInstallerCmd)
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
