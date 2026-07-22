package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestUpgradeCmd_Metadata pins the command's shape without running it (RunE
// shells out to the installer — never invoked in a test). Passing an argument
// must be rejected by cobra's Args check BEFORE RunE, so this never triggers a
// real installer run.
func TestUpgradeCmd_Metadata(t *testing.T) {
	c := newUpgradeCmd()
	if c.Use != "upgrade" {
		t.Errorf("Use = %q, want upgrade", c.Use)
	}
	if c.Short == "" {
		t.Error("Short must be set")
	}

	c.SetArgs([]string{"unexpected-arg"})
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	if err := c.Execute(); err == nil {
		t.Error("upgrade takes no args — an extra arg must error (before RunE)")
	}
}

// TestUpgradeCmd_HelpMentionsVerified: --help renders (no RunE) and states that
// the update is signature-verified, so the copy catalog + users see the safety
// property.
func TestUpgradeCmd_HelpMentionsVerified(t *testing.T) {
	c := newUpgradeCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--help"})
	if err := c.Execute(); err != nil {
		t.Fatalf("upgrade --help: %v", err)
	}
	got := out.String()
	for _, want := range []string{"latest release", "signatures"} {
		if !strings.Contains(got, want) {
			t.Errorf("upgrade --help missing %q in:\n%s", want, got)
		}
	}
}
