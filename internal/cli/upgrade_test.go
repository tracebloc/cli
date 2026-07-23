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

// TestUpgradePlanFor_PerOS: Windows must NOT self-exec (a running .exe is locked
// and install.ps1 is CLI-only) — it only prints the manual command. Unix runs
// the installer with pipefail so a failed curl fails the whole pipeline.
func TestUpgradePlanFor_PerOS(t *testing.T) {
	win := upgradePlanFor("windows")
	if win.exec {
		t.Error("windows upgrade must not self-exec (running .exe is locked)")
	}
	if !strings.Contains(win.manual, "install.ps1") {
		t.Errorf("windows manual command must run install.ps1: %q", win.manual)
	}
	if strings.Contains(win.manual, "i.sh") || strings.Contains(win.manual, "bash") {
		t.Errorf("windows must not point at the Unix installer: %q", win.manual)
	}

	for _, goos := range []string{"linux", "darwin"} {
		p := upgradePlanFor(goos)
		if !p.exec || p.name != "bash" {
			t.Errorf("%s upgrade should exec bash, got exec=%v name=%q", goos, p.exec, p.name)
		}
		joined := strings.Join(p.args, " ")
		if !strings.Contains(joined, "-o pipefail") {
			t.Errorf("%s upgrade must set pipefail so a failed curl fails the run: %q", goos, joined)
		}
		if !strings.Contains(joined, "i.sh") {
			t.Errorf("%s upgrade must run i.sh: %q", goos, joined)
		}
		if p.manual != upgradeInstallerCmdUnix {
			t.Errorf("%s manual hint = %q, want %q", goos, p.manual, upgradeInstallerCmdUnix)
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
