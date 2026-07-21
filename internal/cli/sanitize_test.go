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
