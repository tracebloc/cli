package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The parent "group" commands (data, cluster, auth, client) are runnable via
// runGroup so a mistyped subcommand is a hard error with a suggestion, instead
// of cobra's non-runnable default of printing help and exiting 0 — which
// silently swallowed typos like `tracebloc data ingst`. See #75.

// execGroup runs the CLI with args and returns the resolved exit code + the
// text the user would see. The root sets SilenceErrors, so (like main.go) the
// error is RETURNED rather than written to the buffer — fold its message in so
// assertions see what main.go prints via `Error: <err>`.
func execGroup(t *testing.T, args ...string) (int, string) {
	t.Helper()
	root := NewRootCmd(BuildInfo{Version: "test"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	seen := out.String()
	if err != nil {
		seen += "\n" + err.Error()
	}
	return ExitCodeFromError(err), seen
}

// A mistyped subcommand under each group must exit non-zero (not 0). This is
// the core #75 regression: before the fix, all of these printed help + exit 0.
func TestGroup_UnknownSubcommand_Errors(t *testing.T) {
	cases := [][]string{
		{"data", "ingst"},
		{"dataset", "pus"}, // the `dataset` alias path from the issue
		{"cluster", "inf"},
		{"auth", "stats"},
		{"client", "lst"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out := execGroup(t, args...)
			if code == 0 {
				t.Fatalf("a mistyped subcommand must not exit 0; got exit 0:\n%s", out)
			}
			if !strings.Contains(out, "unknown command") {
				t.Errorf("expected an \"unknown command\" error, got:\n%s", out)
			}
		})
	}
}

// The nearest-match hint fires for a close typo (Levenshtein <= 2), and the
// suggested name is a real, non-hidden sibling command.
func TestGroup_UnknownSubcommand_Suggests(t *testing.T) {
	cases := []struct {
		args    []string
		suggest string
	}{
		{[]string{"data", "ingst"}, "ingest"},
		{[]string{"cluster", "inf"}, "info"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			_, out := execGroup(t, c.args...)
			if !strings.Contains(out, "Did you mean this?") || !strings.Contains(out, c.suggest) {
				t.Errorf("expected a suggestion of %q, got:\n%s", c.suggest, out)
			}
		})
	}
}

// Hidden subcommands must NOT be offered as suggestions even when the typo is
// one edit away — SuggestionsFor skips unavailable commands. Two are hidden:
// `client list` (Rev-9 §7.10) and `cluster doctor` (now a hidden alias of the
// top-level `doctor`, cli#244).
func TestGroup_HiddenSubcommand_NotSuggested(t *testing.T) {
	if _, out := execGroup(t, "client", "lst"); strings.Contains(out, "list") {
		t.Errorf("hidden `client list` must not be suggested:\n%s", out)
	}
	if _, out := execGroup(t, "cluster", "doctr"); strings.Contains(out, "doctor") {
		t.Errorf("hidden `cluster doctor` must not be suggested:\n%s", out)
	}
}

// A bare group (no subcommand) still prints its help and exits 0 — runnable
// must not change the friendly bare-group behavior.
func TestGroup_Bare_StillHelpsExitZero(t *testing.T) {
	for _, group := range []string{"data", "cluster", "auth", "client"} {
		t.Run(group, func(t *testing.T) {
			code, out := execGroup(t, group)
			if code != 0 {
				t.Fatalf("bare `%s` should exit 0, got %d:\n%s", group, code, out)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("bare `%s` should print help, got empty output", group)
			}
		})
	}
}
