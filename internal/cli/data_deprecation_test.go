package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// TestDataDeprecationNotices_AliasPaths is the source-of-truth table test for
// the #879 deprecation notices. It drives the REAL root command (so the
// PersistentPreRunE on `data` fires exactly as in production) for every
// alias/canonical path, captures stderr, and asserts the right notice appears —
// and NONE on canonical invocations.
//
// The commands themselves fail fast after the pre-run (a nonexistent dataset
// path for ingest, a bad kubeconfig for delete), so no network / cluster work
// happens; the notice is printed by the pre-run BEFORE that failure.
//
// Detection uses cobra's exported Command.CalledAs() on the EXECUTED command, so
// it warns for the deprecated verbs `push`/`rm` (the leaf) and for a bare
// `dataset` (the `data` group is itself the executed command via its RunE). It
// intentionally does NOT warn `dataset <canonical-verb>` (e.g. `dataset ingest`)
// — cobra doesn't expose an ancestor's invoked-as name without reaching into its
// internals, and the verb notices already point at the full `data <verb>` form,
// which nudges the group rename. That accepted gap is pinned below.
func TestDataDeprecationNotices_AliasPaths(t *testing.T) {
	const notice = "is deprecated and will be removed"
	badKC := "--kubeconfig=/tmp/tracebloc-cli-dep-nonexistent-" + t.Name()
	noPath := "/tmp/tracebloc-cli-dep-no-such-dir-" + t.Name()

	cases := []struct {
		name string
		args []string
		// want maps each deprecated alias token expected to be warned to its
		// canonical replacement. Empty = no notice.
		want map[string]string
	}{
		{
			name: "bare dataset — group alias warns",
			args: []string{"dataset"},
			want: map[string]string{"dataset": "data"},
		},
		{
			name: "data push — verb alias warns (full canonical form)",
			args: []string{"data", "push", noPath, badKC},
			want: map[string]string{"push": "data ingest"},
		},
		{
			name: "dataset push — verb alias warns (group nudge folded into it)",
			args: []string{"dataset", "push", noPath, badKC},
			want: map[string]string{"push": "data ingest"},
		},
		{
			name: "data rm — verb alias warns",
			args: []string{"data", "rm", "sometable", "--yes", badKC},
			want: map[string]string{"rm": "data delete"},
		},
		{
			name: "dataset rm — verb alias warns",
			args: []string{"dataset", "rm", "sometable", "--yes", badKC},
			want: map[string]string{"rm": "data delete"},
		},
		{
			name: "dataset ingest — group alias + canonical verb: accepted gap, no notice",
			args: []string{"dataset", "ingest", noPath, badKC},
			want: nil,
		},
		{
			name: "data ingest — canonical, no notice",
			args: []string{"data", "ingest", noPath, badKC},
			want: nil,
		},
		{
			name: "data delete — canonical, no notice",
			args: []string{"data", "delete", "sometable", "--yes", badKC},
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := NewRootCmd(BuildInfo{Version: "test"})
			var so, se bytes.Buffer
			root.SetOut(&so)
			root.SetErr(&se)
			root.SetArgs(c.args)
			// The command may error (bad path / kubeconfig) or print help; the
			// pre-run notice fires first, which is all we assert on here.
			_ = root.Execute()

			stderr := se.String()
			if got := strings.Count(stderr, notice); got != len(c.want) {
				t.Fatalf("notice count = %d, want %d\nstderr:\n%s", got, len(c.want), stderr)
			}
			for alias, canonical := range c.want {
				line := fmt.Sprintf("%q is deprecated and will be removed in a future release — use %q instead.", alias, canonical)
				if !strings.Contains(stderr, line) {
					t.Errorf("stderr missing exact notice %q\nstderr:\n%s", line, stderr)
				}
			}
			// A notice must never leak to stdout — scripts parse stdout.
			if strings.Contains(so.String(), notice) {
				t.Errorf("deprecation notice leaked to stdout:\n%s", so.String())
			}
		})
	}
}
