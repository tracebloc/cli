package cli

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// fixedBuildInfo gives tests a deterministic payload regardless of
// whether the test binary was built with -ldflags. Using the same
// values across tests keeps assertions copy-paste-able when adding
// new ones.
func fixedBuildInfo() BuildInfo {
	return BuildInfo{
		Version:   "1.2.3-test",
		GitSHA:    "deadbeef",
		BuildDate: "2026-05-21T00:00:00Z",
	}
}

// execVersion runs the version subcommand against a fresh root tree
// and captures stdout. Tests should never share a *cobra.Command
// across cases — cobra holds state on the command (flag values,
// parsed args) that survives Execute(), so a stale tree leaks one
// test's flags into the next.
func execVersion(t *testing.T, args ...string) string {
	t.Helper()

	root := NewRootCmd(fixedBuildInfo())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)

	root.SetArgs(append([]string{"version"}, args...))
	if err := root.Execute(); err != nil {
		t.Fatalf("version command failed: %v\noutput: %s", err, out.String())
	}
	return out.String()
}

func TestVersion_HumanReadable(t *testing.T) {
	out := execVersion(t)

	// We pin the substring rather than the full line so future
	// formatting tweaks (e.g. adding a build OS field) don't fail
	// the test for a cosmetic-only change.
	for _, want := range []string{
		"tracebloc",
		"1.2.3-test",
		"deadbeef",
		"2026-05-21T00:00:00Z",
		runtime.GOOS,
		runtime.GOARCH,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got: %s", want, out)
		}
	}
}

func TestVersion_JSONOutput(t *testing.T) {
	out := execVersion(t, "--output-json")

	var got versionPayload
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\nraw: %s", err, out)
	}

	// Pinning exact field values so a refactor that accidentally
	// shadowed the injected build info would fail loudly here.
	want := versionPayload{
		Version:   "1.2.3-test",
		GitSHA:    "deadbeef",
		BuildDate: "2026-05-21T00:00:00Z",
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if got != want {
		t.Errorf("payload mismatch.\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestVersion_RejectsExtraArgs(t *testing.T) {
	root := NewRootCmd(fixedBuildInfo())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version", "stowaway"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("expected error from positional arg, got nil. output: %s", out.String())
	}
	if !strings.Contains(err.Error(), "unknown command") &&
		!strings.Contains(err.Error(), "accepts 0 arg") &&
		!strings.Contains(err.Error(), "unexpected") {
		// Cobra's error wording has shifted across versions; accept
		// any of the historical phrasings so a cobra bump doesn't
		// break the assertion for no functional reason.
		t.Errorf("expected an 'unknown command' / 'no args' style error, got: %v", err)
	}
}
