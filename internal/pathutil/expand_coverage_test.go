package pathutil

import "testing"

// TestExpandHome_FallbackBranches covers ExpandHome's two "give up and return the
// path unchanged" arms: an unresolvable ~/ (no home) and an unknown ~user.
func TestExpandHome_FallbackBranches(t *testing.T) {
	// "~/x" with no home → UserHomeDir errors → path returned unchanged.
	t.Setenv("HOME", "")
	if got := ExpandHome("~/x"); got != "~/x" {
		t.Errorf("ExpandHome(~/x) with no home = %q, want it unchanged", got)
	}
	// "~<unknown user>/x" → user.Lookup fails → path returned unchanged.
	const p = "~nosuchuser_zzz_qqq/data"
	if got := ExpandHome(p); got != p {
		t.Errorf("ExpandHome(%q) = %q, want it unchanged (unknown user)", p, got)
	}
}
