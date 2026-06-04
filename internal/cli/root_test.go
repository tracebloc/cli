package cli

import (
	"bytes"
	"strings"
	"testing"
)

// Smoke test that the root command tree builds without panicking
// and that `tracebloc --help` mentions the binary name. This is the
// cheapest signal that the cobra wiring isn't broken.
func TestRootCmd_HelpMentionsBinary(t *testing.T) {
	root := NewRootCmd(BuildInfo{Version: "test"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("--help failed: %v", err)
	}

	got := out.String()
	for _, want := range []string{"tracebloc", "version"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected help text to mention %q, got:\n%s", want, got)
		}
	}
}

// TestRootCmd_HomeScreen: a bare `tracebloc` (no subcommand) renders
// the branded home screen pointing at the key commands, rather than
// erroring or dumping raw usage.
func TestRootCmd_HomeScreen(t *testing.T) {
	root := NewRootCmd(BuildInfo{Version: "test"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{})

	if err := root.Execute(); err != nil {
		t.Fatalf("bare root failed: %v\n%s", err, out.String())
	}
	for _, want := range []string{"tracebloc", "dataset push", "dataset list", "dataset rm", "cluster info"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("home screen missing %q:\n%s", want, out.String())
		}
	}
}

// `completion` is auto-registered by cobra, but it's load-bearing
// for shell autocomplete UX. Verify it's reachable so a future
// refactor that accidentally disables it (e.g. by setting
// DisableSuggestions or DisableCompletion on root) fails the build.
func TestRootCmd_CompletionAvailable(t *testing.T) {
	root := NewRootCmd(BuildInfo{Version: "test"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"completion", "bash"})

	if err := root.Execute(); err != nil {
		t.Fatalf("completion subcommand failed: %v\noutput: %s", err, out.String())
	}
	if out.Len() == 0 {
		t.Fatal("completion produced no output")
	}
}
