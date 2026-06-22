package slug

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"My Client":             "my-client",
		"café":                  "cafe", // NFKD transliteration, matches slug.py
		"  spaces  ":            "spaces",
		"UPPER_Case":            "upper-case",
		"a--b":                  "a-b", // collapse consecutive dashes
		"--trim--":              "trim",
		"":                      "",
		"!!!":                   "",
		strings.Repeat("a", 70): strings.Repeat("a", 63), // 63-char cap
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDerive(t *testing.T) {
	if got, err := Derive("My Client", nil, ""); err != nil || got != "my-client" {
		t.Errorf("no collision: got %q, err %v", got, err)
	}
	if got, _ := Derive("My Client", []string{"my-client"}, ""); got != "my-client-2" {
		t.Errorf("collision: got %q, want my-client-2", got)
	}
	if got, _ := Derive("My Client", []string{"my-client", "my-client-2"}, ""); got != "my-client-3" {
		t.Errorf("double collision: got %q, want my-client-3", got)
	}
	// CJK slugifies to empty → uses fallback
	if got, _ := Derive("世界", nil, "client-abc"); got != "client-abc" {
		t.Errorf("fallback: got %q, want client-abc", got)
	}
	// empty slug + no fallback → error (never a silent-empty namespace)
	if _, err := Derive("!!!", nil, ""); err == nil {
		t.Error("expected an error for empty slug with no fallback")
	}
}
