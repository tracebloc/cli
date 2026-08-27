package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracebloc/cli/internal/installer"
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
// the verified installer via the shared download-then-execute script, never
// `curl | bash` (which would steal the installer's stdin), and reuses
// installer.Cmd for the manual hint so the URL and idiom have one source
// (Bugbot #397, cli#396).
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
		// Download-then-execute, not `curl | bash`: piping steals the installer's
		// stdin so its interactive prompts can't read the TTY (Bugbot #397).
		if strings.Contains(joined, "| bash") || strings.Contains(joined, "|bash") {
			t.Errorf("%s upgrade must not pipe the installer into bash (steals its stdin): %q", goos, joined)
		}
		// `set -e` + `curl -o <file>` fails closed on a bad download instead of
		// running an empty script and reporting a phantom success.
		if !strings.Contains(joined, "set -e") || !strings.Contains(joined, "curl") || !strings.Contains(joined, "-o ") {
			t.Errorf("%s upgrade must download the installer to a file (set -e + curl -o): %q", goos, joined)
		}
		if !strings.Contains(joined, "i.sh") {
			t.Errorf("%s upgrade must run i.sh: %q", goos, joined)
		}
		// backend#2253: the run must carry the explicit-upgrade flag, or the
		// installer's healthy fast-path updates nothing and the CLI stays behind
		// latest — the exact loop this command is supposed to break.
		if !strings.Contains(joined, upgradeCLIEnvAssign) {
			t.Errorf("%s upgrade must carry %q into the installer: %q", goos, upgradeCLIEnvAssign, joined)
		}
		// Manual hint reuses the same built command (single source for URL *and*
		// idiom), not a re-hardcoded string — and it's the same string we exec, so
		// the hint we print after a failed run is exactly what failed, and a
		// copy-pasted retry still upgrades the CLI (cli#396, backend#2253).
		if p.manual != upgradeInstallerCmd {
			t.Errorf("%s manual hint = %q, want upgradeInstallerCmd %q", goos, p.manual, upgradeInstallerCmd)
		}
		if !strings.Contains(p.manual, upgradeCLIEnvAssign) {
			t.Errorf("%s manual hint must carry %q so a pasted retry still upgrades: %q", goos, upgradeCLIEnvAssign, p.manual)
		}
	}
}

// TestUpgradeInstallerCmdIsExplicitUpgrade pins the fix for backend#2253 at the
// command level: `tracebloc upgrade` must NOT run the bare installer (installer.Cmd,
// whose healthy fast-path leaves a below-latest CLI untouched) — it runs the
// installer WITH TB_UPGRADE_CLI=1, the flag the installer reads to update the CLI
// even on an otherwise-healthy machine.
func TestUpgradeInstallerCmdIsExplicitUpgrade(t *testing.T) {
	if !strings.Contains(upgradeInstallerCmd, upgradeCLIEnvAssign) {
		t.Errorf("upgradeInstallerCmd must carry %q; got %q", upgradeCLIEnvAssign, upgradeInstallerCmd)
	}
	// Still the shared, verified bootstrap idiom — not a re-implemented download.
	if !strings.Contains(upgradeInstallerCmd, "i.sh") || !strings.Contains(upgradeInstallerCmd, "curl") {
		t.Errorf("upgradeInstallerCmd must be the shared installer bootstrap; got %q", upgradeInstallerCmd)
	}
	// The bare installer (no explicit-upgrade flag) is precisely what left the CLI
	// behind — guard that we no longer run it verbatim.
	if upgradeInstallerCmd == installer.Cmd {
		t.Errorf("upgrade must not run the bare installer.Cmd (its healthy fast-path updates nothing); got %q", upgradeInstallerCmd)
	}
}

// TestUpgradeEnvStripsAmbientLatest: TB_CLI_LATEST is the installer's decision
// input, so a stray value pre-set in the user's shell must not leak through and
// drive it — upgradeEnv strips it before re-adding the value WE resolved (mirrors
// prepareHostEnv stripping TB_PREPARE_USER; Bugbot #394). Point the release URL
// at a server that refuses, so the fresh fetch returns "" and adds nothing — this
// test is about the strip, not the add, and must not touch the network.
func TestUpgradeEnvStripsAmbientLatest(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer bad.Close()
	swapURL(t, bad.URL)
	t.Setenv("TRACEBLOC_CONFIG_DIR", filepath.Join(t.TempDir(), "nope"))
	t.Setenv("TB_CLI_LATEST", "9.9.9-attacker")
	for _, kv := range upgradeEnv() {
		if kv == "TB_CLI_LATEST=9.9.9-attacker" {
			t.Fatalf("ambient TB_CLI_LATEST leaked into the installer env: %q", kv)
		}
	}
}

// TestUpgradeEnvCarriesResolvedLatest: the OTHER half — when the fetch succeeds,
// upgradeEnv hands the latest to the installer as TB_CLI_LATEST, so it updates
// the CLI only when actually behind (not on every upgrade) (backend#2253).
func TestUpgradeEnvCarriesResolvedLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v0.10.8"}`))
	}))
	defer srv.Close()
	swapURL(t, srv.URL)
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	found := false
	for _, kv := range upgradeEnv() {
		if kv == "TB_CLI_LATEST=0.10.8" {
			found = true
		}
	}
	if !found {
		t.Error("upgradeEnv must pass the freshly-resolved latest release as TB_CLI_LATEST=0.10.8")
	}
}

// TestUpgradeEnvIgnoresStaleCache is the regression guard for the Bugbot High
// finding: a stale 24h cache must NOT drive the installer's decision. Seed the
// cache with an OLD latest (what a user might be running), but have the live
// endpoint report a NEWER release — upgradeEnv must pass the NEWER one, or
// `tracebloc upgrade` would tell the installer "already current" and skip a real
// update (worst right after a 426).
func TestUpgradeEnvIgnoresStaleCache(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := writeUpdateCache(updateCachePath(), updateCache{CheckedAt: time.Now(), Latest: "0.10.5"}); err != nil {
		t.Fatalf("seed stale cache: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v0.10.8"}`)) // GitHub has a newer release
	}))
	defer srv.Close()
	swapURL(t, srv.URL)
	got := ""
	for _, kv := range upgradeEnv() {
		if strings.HasPrefix(kv, "TB_CLI_LATEST=") {
			got = strings.TrimPrefix(kv, "TB_CLI_LATEST=")
		}
	}
	if got != "0.10.8" {
		t.Errorf("upgradeEnv passed TB_CLI_LATEST=%q; want 0.10.8 (fresh release, not the stale cache 0.10.5)", got)
	}
}

// TestSkipUpdateNudge: the nudge must be suppressed right after `tracebloc
// upgrade` (the running process is stale-by-design once it swaps its own binary)
// AND after `tracebloc delete` (offboarding removed the CLI and wiped
// ~/.tracebloc — the nudge's cache write would resurrect the dir; Bugbot #397),
// but fire for any other command.
func TestSkipUpdateNudge(t *testing.T) {
	if !SkipUpdateNudge(newUpgradeCmd()) {
		t.Error("upgrade command must skip the update nudge")
	}
	if !SkipUpdateNudge(newDeleteCmd()) {
		t.Error("delete command must skip the update nudge (its cache write would resurrect the wiped ~/.tracebloc)")
	}
	// `data delete` shares the leaf name "delete" but does NOT offboard/wipe — it
	// must still get the nudge. Guards the name-collision fix (Bugbot #404): the
	// skip is driven by an annotation, not by cmd.Name().
	if SkipUpdateNudge(newDataDeleteCmd()) {
		t.Error("`data delete` must NOT skip the nudge — it doesn't wipe ~/.tracebloc or remove the CLI")
	}
	if SkipUpdateNudge(newDoctorCmd(false)) {
		t.Error("an ordinary command (doctor) must not skip the nudge")
	}
	if SkipUpdateNudge(nil) {
		t.Error("nil command must not skip the nudge")
	}
}
