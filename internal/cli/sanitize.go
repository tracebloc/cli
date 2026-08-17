package cli

import (
	"regexp"
	"strings"
	"unicode"
)

// escSequence matches the two ANSI escape families a terminal actually sends for
// cursor / function keys:
//
//	CSI  ESC '[' <params ∈ [0-9;]> <final ∈ [A-Za-z~]>
//	     ESC[A/B/C/D (arrows), ESC[1;5C (Ctrl+arrow), ESC[3~ (Delete),
//	     ESC[200~ … ESC[201~ (bracketed paste)
//	SS3  ESC 'O' <final ∈ [A-Za-z~]>
//	     ESC OA/OB/OC/OD (arrows), ESC OH/OF (Home/End), ESC OP…OS (F1–F4)
//
// SS3 is what the *same* keys emit once the terminal is in DECCKM
// application-cursor mode — the state vim, less or tmux leave behind on an
// unclean exit (cli#516). Handling only CSI was the remaining hole: an ESC alone
// is a C0 byte and gets dropped below, but 'O' and the final byte are printable,
// so "\x1bOD\x1bOD\x1bOA" survived as the plausible-looking name "ODODOA" and
// minted the immutable namespace "ododoa". CSI residue never did: it cleans to
// empty, fails the non-empty check and re-prompts.
//
// Deliberately broader than submit.stripANSI, which only strips SGR colour codes
// (final byte 'm').
var escSequence = regexp.MustCompile("\x1b(?:\\[[0-9;]*|O)[A-Za-z~]")

// escResidue is the post-sanitise floor's *probe*, not a strip: ESC, any run of
// non-final intermediate bytes, then AT MOST TWO bytes that could be escape final
// bytes. Its only job is to answer one question — "is there an alphanumeric here
// that did NOT come from an escape final byte?" — and its output is never
// returned as a name.
//
// Two, not one and not unbounded. One is too few: it leaves the 'D' of an
// unrecognised SS3-shaped pair behind, and the floor stops firing on exactly the
// family shape this ticket is about. Unbounded is too many: `\x1bNChello` would
// have the whole name swallowed and be refused, while `\x1bNC日本` survives —
// making keep-vs-reject depend on the script the user's name is written in
// (Bugbot, tracebloc/client#736). Two is the honest bound: an escape final is one
// byte, an intro plus a final is two, and every keyboard-input escape family
// (SS2, SS3, the 7-bit C1 forms) fits in that.
var escResidue = regexp.MustCompile("\x1b[^A-Za-z0-9~]*[A-Za-z~]{1,2}")

// sanitizeClientName strips terminal escape sequences and C0 control characters
// from a user-supplied client name or location before it becomes the stored
// display name and is slugified into an immutable Kubernetes namespace.
//
// Defense-in-depth for the name-garble bug (customer-reported 2026-07-20, fixed
// 2026-07-21 in cli#364 / client#362): typing arrow keys at the installer's name
// prompt injected raw ESC[D/ESC[A bytes. The installer strips them at the source,
// but a name can also arrive here directly via --name / $TRACEBLOC_CLIENT_NAME,
// and slug.Slugify would otherwise turn each ESC[<x> run into a "-" — ESC (0x1B)
// survives Slugify's ASCII pass, so "se-\e[D\e[D\e[A\e[A" mints the garbage
// namespace "se-d-d-a-a". Cleaning here, at the CLI boundary, keeps slug.Slugify
// a faithful mirror of the backend's slug.py (which must NOT strip — the backend
// validates exactly what it produces); input hygiene belongs at ingestion, not in
// the shared slug rule.
//
// Escape-derived garbage cannot be caught downstream: is_dns1123_label validates
// by idempotence against the slug rule, so "ododoa" is a perfectly canonical
// label. Form is exactly what this class of input preserves — hence the floor
// below, which is about *content*, not form.
//
// UTF-8 bytes (>= 0x80) are preserved so international names survive.
func sanitizeClientName(s string) string {
	// 1) Whole escape sequences (arrow keys in either cursor mode, function keys,
	//    paste wrappers). ReplaceAll handles consecutive sequences in one pass;
	//    any orphaned ESC left behind is a C0 byte and is removed by step 4.
	s = escSequence.ReplaceAllString(s, "")
	// 2) Post-corruption case: an earlier (buggy) sanitizer dropped the ESC but
	//    left the literal bracketed-paste markers. Only these two well-defined
	//    markers are removed — a generic "[x~" could be real name content.
	s = strings.ReplaceAll(s, "[200~", "")
	s = strings.ReplaceAll(s, "[201~", "")
	// 3) The floor. Steps 1–2 know CSI, SS3 and the paste markers; they cannot
	//    know the escape family nobody has reported yet, and that is precisely how
	//    SS3 got here — one rule, three hand-copied implementations, no member of
	//    the family beyond CSI ever tested. So: if an ESC SURVIVED step 1, this
	//    value carries an escape shape we do not recognise, and the printable
	//    bytes around it are not trustworthy name content. Require at least one
	//    alphanumeric that did not come from an escape final byte (at most two of
	//    them — see escResidue); if there is none, the whole value was residue —
	//    return "" and let the caller auto-name (the same path an omitted --name
	//    takes).
	//
	//    Scoped to "an ESC survived" on purpose. It is the narrowest trigger that
	//    still covers unknown families: a clean name never reaches it, a name with
	//    real content next to an unknown escape keeps that content, and only a
	//    value that is *nothing but* escape residue is rejected. The failure it
	//    chooses is the recoverable one — auto-naming a name is annoying; minting
	//    a permanent namespace from keyboard noise is not.
	if strings.ContainsRune(s, 0x1b) && !hasAlphanumeric(escResidue.ReplaceAllString(s, "")) {
		return ""
	}
	// 4) Drop any remaining C0 control characters and DEL; keep printable ASCII
	//    and all multi-byte UTF-8 (>= 0x80).
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// hasAlphanumeric reports whether s contains any letter or digit, Unicode
// included — a name written entirely in a non-Latin script is real content.
func hasAlphanumeric(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	}) >= 0
}
