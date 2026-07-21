package cli

import (
	"regexp"
	"strings"
)

// csiSequence matches an ANSI CSI sequence: ESC '[' <params ∈ [0-9;]> <final ∈
// [A-Za-z~]>. This is what a terminal emits for arrow keys / cursor moves
// (ESC[A/B/C/D, ESC[1;5C, ESC[3~ …) and for bracketed-paste wrappers
// (ESC[200~ … ESC[201~). It is deliberately broader than submit.stripANSI,
// which only strips SGR colour codes (final byte 'm').
var csiSequence = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z~]")

// sanitizeClientName strips terminal escape sequences and C0 control characters
// from a user-supplied client name or location before it becomes the stored
// display name and is slugified into an immutable Kubernetes namespace.
//
// Defense-in-depth for the name-garble bug (customer-reported 2026-07-20): typing
// arrow keys at the installer's name prompt injected raw ESC[D/ESC[A bytes. The
// installer now strips them at the source, but a name can also arrive here
// directly via --name / $TRACEBLOC_CLIENT_NAME, and slug.Slugify would otherwise
// turn each ESC[<x> run into a "-" — ESC (0x1B) survives Slugify's ASCII pass, so
// "se-\e[D\e[D\e[A\e[A" mints the garbage namespace "se-d-d-a-a". Cleaning here,
// at the CLI boundary, keeps slug.Slugify a faithful mirror of the backend's
// slug.py (which must NOT strip — the backend validates exactly what it produces);
// input hygiene belongs at ingestion, not in the shared slug rule.
//
// UTF-8 bytes (>= 0x80) are preserved so international names survive.
func sanitizeClientName(s string) string {
	// 1) Whole CSI sequences (arrow keys, paste wrappers). ReplaceAll handles
	//    consecutive sequences in one pass; any orphaned ESC left behind is a C0
	//    byte and is removed by step 3.
	s = csiSequence.ReplaceAllString(s, "")
	// 2) Post-corruption case: an earlier (buggy) sanitizer dropped the ESC but
	//    left the literal bracketed-paste markers. Only these two well-defined
	//    markers are removed — a generic "[x~" could be real name content.
	s = strings.ReplaceAll(s, "[200~", "")
	s = strings.ReplaceAll(s, "[201~", "")
	// 3) Drop any remaining C0 control characters and DEL; keep printable ASCII
	//    and all multi-byte UTF-8 (>= 0x80).
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
