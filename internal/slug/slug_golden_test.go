package slug

import "testing"

// goldenPairs are (display name → DNS-1123 slug) pairs verified BYTE-IDENTICAL
// against the canonical Python slugify_dns1123 (backend common/utils/slug.py)
// with a cross-language harness. They lock the NFKD transliteration — the one
// place a Go port can silently drift from the backend validator that rejects
// what this produces. If slug.py ever changes, re-run the harness and update
// these rather than hand-editing.
var goldenPairs = []struct{ name, want string }{
	{"My Client", "my-client"},
	{"café", "cafe"}, // canonical NFKD: é → e + accent (dropped)
	{"CAFÉ", "cafe"}, // + lowercase
	{"Müller GmbH", "muller-gmbh"},
	{"Straße 42", "strae-42"}, // ß does not decompose → dropped (matches Python)
	{"naïve", "naive"},
	{"São Paulo", "sao-paulo"},
	{"Zürich-Lab", "zurich-lab"},
	{"北京 client", "client"},           // CJK dropped
	{"client🚀rocket", "clientrocket"}, // emoji dropped
	{"über_cool", "uber-cool"},
	{"piñata", "pinata"},
	{"Ω omega", "omega"},
	{"ﬁ-ligature", "fi-ligature"},  // compat NFKD: ﬁ ligature → "fi"
	{"①②③ circled", "123-circled"}, // circled digits → "123"
	{"½ half", "12-half"},          // vulgar fraction → "1" + fraction-slash(dropped) + "2"
	{"ＦＵＬＬ width", "full-width"},   // fullwidth → ASCII
	{"trailing---dashes---", "trailing-dashes"},
	{"UPPER MixED", "upper-mixed"},
	{"a.b.c-d_e f", "a-b-c-d-e-f"},
	{"2001: A Space Odyssey", "2001-a-space-odyssey"},
	{"Ⅻ roman", "xii-roman"}, // roman numeral → "XII" → "xii"
	{"²cubed³", "2cubed3"},   // super/subscripts → digits
	{"Hello @ World #1", "hello-world-1"},
}

func TestSlugifyGoldenParity(t *testing.T) {
	for _, p := range goldenPairs {
		if got := Slugify(p.name); got != p.want {
			t.Errorf("Slugify(%q) = %q, want %q (golden parity with slug.py)", p.name, got, p.want)
		}
	}
}
