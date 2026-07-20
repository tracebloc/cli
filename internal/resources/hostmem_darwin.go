//go:build darwin

package resources

import "golang.org/x/sys/unix"

// hostMemBytes reads the Mac's total physical memory via the hw.memsize sysctl.
func hostMemBytes() int64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(n)
}
