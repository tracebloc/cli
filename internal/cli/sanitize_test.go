package cli

import (
	"testing"

	"github.com/tracebloc/cli/internal/slug"
)

func TestSanitizeClientName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"clean name is unchanged", "lukas-macbook", "lukas-macbook"},
		{"spaces are preserved (display name)", "Lukas MacBook", "Lukas MacBook"},
		{"empty stays empty", "", ""},

		// The reported symptom: arrow keys typed at the name prompt.
		{"arrow-key escapes are stripped whole", "se-\x1b[D\x1b[D\x1b[A\x1b[A", "se-"},
		{"only-escapes cleans to empty (→ auto-name)", "\x1b[D\x1b[A", ""},
		{"CSI with params (Ctrl+arrow)", "x\x1b[1;5Dy", "xy"},
		{"Delete key (ESC[3~)", "ab\x1b[3~", "ab"},

		// SS3 (cli#516): the SAME keys once the terminal is in DECCKM
		// application-cursor mode, which vim/less/tmux leave behind on an unclean
		// exit. Unlike CSI residue these used to survive as a plausible name.
		{"SS3 arrows around real content", "na\x1bODme", "name"},
		{"SS3 arrows only cleans to empty (→ auto-name)", "\x1bOD\x1bOD\x1bOD\x1bOA\x1bOA\x1bOA", ""},
		{"SS3 Home/End only", "\x1bOH\x1bOF", ""},
		{"SS3 F1/F2 only", "\x1bOP\x1bOQ", ""},
		{"SS3 and CSI mixed", "a\x1bODb\x1b[Dc", "abc"},
		{"truncated SS3 (ESC O, no final)", "\x1bO", ""},
		{"a bare O is not an escape", "OPTIMUS-01", "OPTIMUS-01"},

		// The floor: an ESC that survived the known families means an escape shape
		// we don't recognise, so what is left has to prove it is real content.
		// SS2 (ESC N <final>) stands in for "the next family" — it is not stripped
		// by escSequence, so these exercise the floor and nothing else.
		{"unknown escape family, nothing but residue (→ auto-name)", "\x1bNB\x1bNC", ""},
		{"unknown escape family beside real content is kept", "box\x1bNC", "boxNC"},
		{"floor counts non-Latin letters as real content", "\x1bNC日本", "NC日本"},
		// The probe's final-byte run is bounded at two, so an ASCII name after an
		// unknown escape is kept just like a non-Latin one — keep-vs-reject must
		// not depend on the script the name is written in (Bugbot,
		// tracebloc/client#736).
		{"floor keeps an ASCII name after an unknown escape", "\x1bNChello", "NChello"},

		// Bracketed paste.
		{"bracketed-paste wrappers", "\x1b[200~hello\x1b[201~", "hello"},
		{"post-corruption literal paste markers", "[200~hello[201~", "hello"},

		// Bare C0 / DEL control characters.
		{"tab/newline/null are dropped", "a\tb\nc\x00", "abc"},
		{"carriage return is dropped", "a\rb", "ab"},
		{"DEL is dropped", "a\x7fb", "ab"},
		{"lone ESC is dropped", "a\x1bb", "ab"},

		// Legitimate content is preserved.
		{"UTF-8 is preserved", "café-münchen", "café-münchen"},
		{"non-escape brackets are kept", "host[1]", "host[1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeClientName(tc.in); got != tc.want {
				t.Fatalf("sanitizeClientName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeClientName_neutralizesSlugGarble documents the exact bug and proves
// the guard fixes it end-to-end: the raw arrow-key input slugifies to the
// customer-reported garbage namespace, and the sanitized input slugifies clean.
func TestSanitizeClientName_neutralizesSlugGarble(t *testing.T) {
	const garbled = "se-\x1b[D\x1b[D\x1b[A\x1b[A"

	// Without the guard, slug.Slugify turns each ESC[<x> run into a "-": the
	// exact "d-d-a-a" symptom the customer saw. (Asserted so a future slug change
	// that alters this is caught here, not in the field.)
	if got := slug.Slugify(garbled); got != "se-d-d-a-a" {
		t.Fatalf("precondition: slug.Slugify(raw) = %q, want the documented garble %q", got, "se-d-d-a-a")
	}

	// With the guard, the escapes are gone before slugification → a clean label.
	if got := slug.Slugify(sanitizeClientName(garbled)); got != "se" {
		t.Fatalf("slug.Slugify(sanitizeClientName(raw)) = %q, want %q", got, "se")
	}
}

// TestSanitizeClientName_ss3SurvivesAsAPlausibleName is the cli#516 half: SS3
// residue was worse than the CSI residue fixed in cli#364 / client#362, because
// it did NOT clean to empty. 'O' and the final byte are printable, so dropping
// only the ESC left a non-empty, plausible-looking name that passed every
// downstream check — is_dns1123_label validates by idempotence against the slug
// rule, and "odododoaoaoa" is a perfectly canonical label. Form is exactly what
// this input preserves, so nothing but this function can refuse it.
func TestSanitizeClientName_ss3SurvivesAsAPlausibleName(t *testing.T) {
	const garbled = "\x1bOD\x1bOD\x1bOD\x1bOA\x1bOA\x1bOA" // ← ← ← ↑ ↑ ↑ in DECCKM mode

	// What the CSI-only sanitizer produced: ESC removed as a C0 byte, the rest
	// kept verbatim — and slugifying that mints a permanent namespace.
	if got := slug.Slugify("ODODODOAOAOA"); got != "odododoaoaoa" {
		t.Fatalf("precondition: slug.Slugify(%q) = %q, want the documented garble %q", "ODODODOAOAOA", got, "odododoaoaoa")
	}

	// With SS3 handled, the value cleans to empty, which client create treats
	// exactly like an omitted --name: it auto-names instead of minting garbage.
	if got := sanitizeClientName(garbled); got != "" {
		t.Fatalf("sanitizeClientName(%q) = %q, want %q (the auto-name path)", garbled, got, "")
	}
}
