package pathutil

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// TestExpandHome_Basics pins the non-lookup contract: empty, relative,
// absolute, and non-tilde paths pass through untouched; "~" and "~/…"
// resolve under the current user's $HOME.
func TestExpandHome_Basics(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("no home dir: %v", err)
	}
	cases := []struct{ in, want string }{
		{"", ""},
		{"relative/path", "relative/path"},
		{"/absolute/path", "/absolute/path"},
		{"./x", "./x"},
		{"~", home},
		{"~/", home},
		{"~/x", filepath.Join(home, "x")},
		{"~/a/b", filepath.Join(home, "a", "b")},
	}
	for _, c := range cases {
		if got := ExpandHome(c.in); got != c.want {
			t.Errorf("ExpandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExpandHome_NamedUser covers the ~user form (#181): "~user" and
// "~user/…" resolve under that user's home. We look up the CURRENT user by
// name so the test doesn't depend on a fixed account, and an unknown ~user
// is left literal so the caller's path-existence check surfaces it.
func TestExpandHome_NamedUser(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		t.Skipf("no current user: %v", err)
	}
	looked, err := user.Lookup(u.Username)
	if err != nil {
		t.Skipf("user.Lookup(%q) unsupported here: %v", u.Username, err)
	}
	if looked.HomeDir == "" {
		t.Skipf("current user has a blank home dir")
	}
	home := looked.HomeDir

	cases := []struct{ in, want string }{
		{"~" + u.Username, home},
		{"~" + u.Username + "/data", filepath.Join(home, "data")},
		{"~" + u.Username + "/a/b", filepath.Join(home, "a", "b")},
	}
	for _, c := range cases {
		if got := ExpandHome(c.in); got != c.want {
			t.Errorf("ExpandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// An unknown user can't be resolved: the literal is returned unchanged.
	const unknown = "~nsuchuser-tracebloc-181/x"
	if got := ExpandHome(unknown); got != unknown {
		t.Errorf("ExpandHome(%q) = %q, want it left literal", unknown, got)
	}
}
