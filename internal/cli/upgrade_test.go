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

// TestUpgradeCommandFor_PerOS: Windows is a shipped platform with no bash, so it
// must run install.ps1 via PowerShell — never the Unix `bash i.sh` installer,
// which fails there while the 426 "too old" error tells users to run upgrade.
func TestUpgradeCommandFor_PerOS(t *testing.T) {
	name, args, human := upgradeCommandFor("windows")
	if name != "powershell" {
		t.Errorf("windows upgrade name = %q, want powershell", name)
	}
	joined := strings.Join(args, " ") + " " + human
	if strings.Contains(joined, "bash") || strings.Contains(joined, "i.sh") {
		t.Errorf("windows upgrade must not use the Unix installer: %q", joined)
	}
	if !strings.Contains(joined, "install.ps1") {
		t.Errorf("windows upgrade must run install.ps1: %q", joined)
	}

	for _, goos := range []string{"linux", "darwin"} {
		name, args, human := upgradeCommandFor(goos)
		if name != "bash" {
			t.Errorf("%s upgrade name = %q, want bash", goos, name)
		}
		if len(args) != 2 || args[0] != "-c" || !strings.Contains(args[1], "i.sh") {
			t.Errorf("%s upgrade args = %v, want bash -c '…i.sh…'", goos, args)
		}
		if human != upgradeInstallerCmdUnix {
			t.Errorf("%s human hint = %q, want %q", goos, human, upgradeInstallerCmdUnix)
		}
	}
}

// TestSkipUpdateNudge: the nudge must be suppressed right after `tracebloc
// upgrade` (the running process is stale-by-design once it swaps its own binary)
// but fire for any other command.
func TestSkipUpdateNudge(t *testing.T) {
	if !SkipUpdateNudge(newUpgradeCmd()) {
		t.Error("upgrade command must skip the update nudge")
	}
	if SkipUpdateNudge(newDeleteCmd()) {
		t.Error("non-upgrade command must not skip the nudge")
	}
	if SkipUpdateNudge(nil) {
		t.Error("nil command must not skip the nudge")
	}
}
