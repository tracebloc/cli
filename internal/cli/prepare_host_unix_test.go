//go:build !windows

package cli

import (
	"context"
	"testing"
)

// On cancel we must group-signal the whole installer pipeline, not just the
// top-level bash, or the curl and the privileged prepare-host child keep running
// detached after the CLI has reported failure and exited. Guard that the command
// is created in its own process group with a group-killing Cancel (Bugbot #394).
// Unix-only: SysProcAttr.Setpgid doesn't exist on Windows (see
// prepare_host_windows.go, where configureProcessGroup is a no-op).
func TestPrepareHostCmdRunsInOwnProcessGroup(t *testing.T) {
	c := prepareHostCmd(context.Background())
	if c.SysProcAttr == nil || !c.SysProcAttr.Setpgid {
		t.Error("prepareHostCmd must set SysProcAttr.Setpgid so the installer pipeline gets its own process group")
	}
	if c.Cancel == nil {
		t.Error("prepareHostCmd must set Cancel to group-kill the pipeline when the context is cancelled")
	}
}
