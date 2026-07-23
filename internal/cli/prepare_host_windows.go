//go:build windows

package cli

import "os/exec"

// configureProcessGroup is a no-op on Windows: POSIX process groups
// (syscall.Setpgid / syscall.Kill with a negative PID) don't exist there. That's
// acceptable — prepare-host is a Linux host operation that shells out to bash, so
// the Windows path is only reachable under WSL/Git-bash, and exec.CommandContext
// already terminates the top-level process on cancel. The richer group-signalling
// that stops detached privileged children lives in prepare_host_unix.go (Bugbot
// #394).
func configureProcessGroup(c *exec.Cmd) {}
