//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the installer pipeline in its own process group and,
// on context cancel (Ctrl-C / parent shutdown), signals the WHOLE group rather
// than just the top-level `bash` (Bugbot #394). Without this, cancelling the CLI
// killed only the immediate `bash -c`, leaving the `curl` and the `bash "$tmp"`
// prepare-host child — which performs privileged host prep — running detached
// after the CLI had already reported failure and exited.
//
// Unix-only: syscall.Setpgid / syscall.Kill (POSIX process groups) don't exist
// on Windows, so the Windows build gets the no-op in prepare_host_windows.go.
func configureProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		// Negative PID → signal the entire process group we created above.
		return syscall.Kill(-c.Process.Pid, syscall.SIGINT)
	}
}
