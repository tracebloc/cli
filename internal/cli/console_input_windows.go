//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableKeyEventInput makes arrow keys work in the guided prompts on Windows.
//
// The bug it fixes: pressing ↓ in a Select typed "[B" into the filter box and never
// moved the selection.
//
// Why that happens. survey's Windows rune reader (terminal/runereader_windows.go)
// reads console records with ReadConsoleInputW and recognises navigation from the
// VIRTUAL KEY codes VK_UP / VK_DOWN. Before reading it clears ENABLE_ECHO_INPUT,
// ENABLE_LINE_INPUT and ENABLE_PROCESSED_INPUT — but NOT
// ENABLE_VIRTUAL_TERMINAL_INPUT. With that flag set the console stops delivering
// arrows as VK_* events and delivers them as ANSI escape sequences (ESC '[' 'B')
// instead, so survey sees three ordinary runes: the ESC is swallowed and "[B" lands
// in the filter. Navigation can never fire because no VK_DOWN ever arrives.
//
// Why it can appear "suddenly": console input mode is state on the console handle,
// not something the CLI chooses. A terminal that opts into VT input, or any program
// in the session that sets the flag and does not restore it, changes this with no
// change on our side. So clearing the flag for the duration of a prompt is the fix
// regardless of who turned it on.
//
// Contract: returns a restore func that is ALWAYS safe to call (never nil). If the
// mode cannot be read or set — stdin redirected to a pipe or file, no console
// attached, a hardened environment that refuses the call — everything is a no-op and
// prompting proceeds exactly as before. Interactive niceness must never be able to
// break a run.
func enableKeyEventInput() (restore func()) {
	noop := func() {}

	h := windows.Handle(os.Stdin.Fd())

	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		// Not a console (piped/redirected stdin, or no console at all).
		return noop
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_INPUT == 0 {
		// Already delivering VK_* key events — survey's path works, leave it alone.
		return noop
	}

	want := mode &^ windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(h, want); err != nil {
		return noop
	}

	// Restore the caller's mode. The user's shell owns this state, so we put back
	// exactly what we found rather than a value we think is right — leaving VT input
	// off would change how the terminal behaves after the CLI exits.
	return func() { _ = windows.SetConsoleMode(h, mode) }
}
