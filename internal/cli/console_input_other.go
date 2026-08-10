//go:build !windows

package cli

// enableKeyEventInput is a no-op off Windows.
//
// The problem it solves is specific to the Windows console: survey navigates from
// VK_UP / VK_DOWN virtual-key records and breaks when ENABLE_VIRTUAL_TERMINAL_INPUT
// makes the console deliver arrows as ANSI escape sequences instead. On POSIX the
// terminal is put into raw mode and survey parses those escape sequences itself,
// which is the normal, working path — there is nothing to adjust.
//
// Returns a non-nil func so callers can `defer restore()` unconditionally.
func enableKeyEventInput() (restore func()) { return func() {} }
