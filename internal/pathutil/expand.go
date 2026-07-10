// Package pathutil holds small filesystem-path helpers shared across
// the CLI. It's a leaf package (no internal imports) so both
// internal/cli and internal/cluster can use it without importing each
// other — the consolidation the two former copies of this expander
// kept promising in their doc comments.
package pathutil

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// ExpandHome expands a leading ~ to a home directory, leaving every
// other path (relative, absolute, empty) untouched:
//
//   - ""                     → "" (callers read empty as "use defaults")
//   - "~" and "~/…"          → the current user's $HOME
//   - "~user" and "~user/…"  → that named user's home (via user.Lookup)
//
// When a home can't be resolved the literal path is returned unchanged,
// so the caller's own path-existence check reports it plainly ("no such
// file or directory: ~bob/data") instead of us silently mangling it into
// a relative path. That covers three cases:
//
//   - no $HOME for the current user,
//   - an unknown or unlookupable ~user (a static CGO-less binary can't
//     read /etc/passwd for a foreign user), and
//   - a resolvable account whose passwd home-directory field is blank —
//     user.Lookup succeeds with an empty HomeDir, and joining "" would
//     yield a relative path, so we treat it like a resolution failure.
func ExpandHome(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	// "~" or "~/…" → the current user's home. path[1:] is "" for "~"
	// (→ home) and "/x" for "~/x" (→ home/x); filepath.Join cleans it.
	if len(path) == 1 || path[1] == '/' {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return path
		}
		return filepath.Join(home, path[1:])
	}
	// "~user" or "~user/…" → the named user's home. Split the username
	// off at the first slash; the remainder (possibly empty) joins onto
	// their home directory.
	name, rest, _ := strings.Cut(path[1:], "/")
	u, err := user.Lookup(name)
	if err != nil || u.HomeDir == "" {
		return path
	}
	return filepath.Join(u.HomeDir, rest)
}
