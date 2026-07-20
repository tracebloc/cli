//go:build !darwin && !linux

package resources

// hostMemBytes has no portable implementation on other platforms (Windows etc.)
// yet — return 0 so the "Your machine" line shows CPU and omits memory rather
// than a fabricated value. A per-OS implementation can be added later.
func hostMemBytes() int64 { return 0 }
