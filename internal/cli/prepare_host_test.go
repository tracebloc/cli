package cli

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// A failed download must abort rather than run an empty script: with the old
// `curl | bash`, a curl failure left bash reading empty stdin and exiting 0, so
// the command reported success while prepare-host never ran. `set -e` + `curl
// -o <file>` makes the failure propagate — guard both against removal (Bugbot
// #394).
func TestPrepareHostCmdFailsClosedOnDownloadError(t *testing.T) {
	if !strings.Contains(prepareHostInstallerCmd, "set -e") {
		t.Fatalf("prepareHostInstallerCmd must `set -e` so a failed download aborts; got: %q", prepareHostInstallerCmd)
	}
	if !strings.Contains(prepareHostInstallerCmd, "curl") || !strings.Contains(prepareHostInstallerCmd, "-o ") {
		t.Fatalf("prepareHostInstallerCmd must download the installer to a file (curl -o) so curl's exit is checked; got: %q", prepareHostInstallerCmd)
	}
	if !strings.Contains(prepareHostInstallerCmd, "prepare-host") {
		t.Fatalf("prepareHostInstallerCmd should invoke the installer's prepare-host step; got: %q", prepareHostInstallerCmd)
	}
}

// The installer must NOT be fed to bash over a pipe: `curl | bash -s` makes the
// inner bash read its program from the pipe, stealing the installer's stdin so
// any interactive prompt in prepare-host gets EOF. We download and run a file
// instead, leaving stdin on the TTY (Bugbot #394).
func TestPrepareHostCmdDoesNotPipeIntoBash(t *testing.T) {
	if strings.Contains(prepareHostInstallerCmd, "| bash") || strings.Contains(prepareHostInstallerCmd, "|bash") {
		t.Errorf("prepareHostInstallerCmd must not pipe the script into bash (steals the installer's stdin); got: %q", prepareHostInstallerCmd)
	}
}

// The installer must run in the CLI's foreground process group (NOT its own):
// it's interactive and stdin is the TTY, so a backgrounded group would get
// SIGTTIN and hang on any prompt. And WaitDelay must be positive so a child that
// traps signals can't hang Wait forever after a programmatic cancel (Bugbot
// #394). SysProcAttr==nil is portable (the field is *syscall.SysProcAttr on
// every OS), so this stays a single cross-platform test.
func TestPrepareHostCmdStaysInForegroundGroup(t *testing.T) {
	c := prepareHostCmd(context.Background())
	if c.SysProcAttr != nil {
		t.Error("prepareHostCmd must not set SysProcAttr — a separate/background process group breaks interactive TTY prompts (SIGTTIN)")
	}
	if c.WaitDelay <= 0 {
		t.Error("prepareHostCmd must set a positive WaitDelay so Wait can't hang forever after a cancel")
	}
}

// A user abort must be detected as an interrupt even when NotifyContext hasn't
// flipped ctx.Err() yet — bash exits 130 on SIGINT and c.Run() can return first
// (Bugbot #394). A genuine failure (exit 1) must NOT be treated as an interrupt.
func TestPrepareHostInterrupted(t *testing.T) {
	// Cancelled context → interrupt regardless of the run error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !prepareHostInterrupted(ctx, errors.New("boom")) {
		t.Error("a cancelled context must be treated as an interrupt")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available for the exit-code cases")
	}
	// Live context + bash exit 130 (128+SIGINT) → interrupt (the Ctrl-C race).
	if err := exec.Command("bash", "-c", "exit 130").Run(); err == nil {
		t.Fatal("expected a non-nil error from exit 130")
	} else if !prepareHostInterrupted(context.Background(), err) {
		t.Error("exit 130 with a live context must be treated as an interrupt")
	}
	// Live context + a normal failure (exit 1) → NOT an interrupt.
	if err := exec.Command("bash", "-c", "exit 1").Run(); err == nil {
		t.Fatal("expected a non-nil error from exit 1")
	} else if prepareHostInterrupted(context.Background(), err) {
		t.Error("exit 1 must NOT be treated as an interrupt")
	}
}

func TestPrepareHostCmdMetadata(t *testing.T) {
	c := newPrepareHostCmd()
	if !strings.HasPrefix(c.Use, "prepare-host") {
		t.Errorf("Use = %q, want it to start with prepare-host", c.Use)
	}
	// MaximumNArgs(1): the optional researcher username. Zero or one arg is fine;
	// two is rejected (Bugbot / Divya #377: name the researcher to grant access).
	if err := c.Args(c, []string{}); err != nil {
		t.Errorf("prepare-host must accept zero args: %v", err)
	}
	if err := c.Args(c, []string{"alice"}); err != nil {
		t.Errorf("prepare-host must accept one username arg: %v", err)
	}
	if err := c.Args(c, []string{"alice", "bob"}); err == nil {
		t.Error("prepare-host should reject more than one positional argument")
	}
}

// The researcher username is passed to the installer as TB_PREPARE_USER, so it
// must be validated: accept real Linux usernames, reject shell-metacharacter /
// empty / overlong input (Divya #377).
func TestPrepareHostUserValidation(t *testing.T) {
	valid := []string{"alice", "bob123", "a.b_c-d", "R2D2", "svc_account"}
	for _, u := range valid {
		if !prepareHostUserRe.MatchString(u) {
			t.Errorf("username %q should be valid", u)
		}
	}
	invalid := []string{"", "-leading", ".dot", "has space", "semi;colon", "a/b", "$(whoami)", "a`b`", "toolong_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	for _, u := range invalid {
		if prepareHostUserRe.MatchString(u) {
			t.Errorf("username %q should be rejected", u)
		}
	}
}

// The no-username path promises it grants no access, so a pre-set ambient
// TB_PREPARE_USER must be stripped from the child env; the username path sets
// exactly that user (replacing any ambient value, not duplicating it) — Bugbot #394.
func TestPrepareHostEnv_StripsAmbientAndSetsUser(t *testing.T) {
	t.Setenv("TB_PREPARE_USER", "ambient-attacker")

	for _, kv := range prepareHostEnv("") {
		if strings.HasPrefix(kv, "TB_PREPARE_USER=") {
			t.Errorf("no-username env must not carry TB_PREPARE_USER, got %q", kv)
		}
	}

	n, got := 0, ""
	for _, kv := range prepareHostEnv("alice") {
		if strings.HasPrefix(kv, "TB_PREPARE_USER=") {
			n++
			got = strings.TrimPrefix(kv, "TB_PREPARE_USER=")
		}
	}
	if n != 1 || got != "alice" {
		t.Errorf("username env should carry exactly TB_PREPARE_USER=alice, got n=%d val=%q", n, got)
	}
}

// On failure after `prepare-host <user>`, the manual retry must still grant
// access (carry TB_PREPARE_USER=<user>) — otherwise a copy-pasted retry silently
// does less than the original request. The no-username hint carries no such var
// (Bugbot #394).
func TestPrepareHostManualHint_CarriesUser(t *testing.T) {
	if h := prepareHostManualHint(""); strings.Contains(h, "TB_PREPARE_USER") {
		t.Errorf("no-username hint must not set TB_PREPARE_USER: %q", h)
	}
	h := prepareHostManualHint("alice")
	if !strings.Contains(h, "TB_PREPARE_USER=alice") {
		t.Errorf("username hint must carry TB_PREPARE_USER=alice so the retry still grants access: %q", h)
	}
	if !strings.Contains(h, "prepare-host") {
		t.Errorf("hint must invoke prepare-host: %q", h)
	}
}

// prepare-host shells out to bash/curl and readies a Unix host, so it must be
// guarded on Windows (a no-op-with-explanation, not a cryptic missing-bash
// failure) — mirrors upgrade's Windows handling (Bugbot #394).
func TestPrepareHostUnsupportedOnWindows(t *testing.T) {
	if !prepareHostUnsupportedOnOS("windows") {
		t.Error("prepare-host must be guarded on windows")
	}
	for _, goos := range []string{"linux", "darwin"} {
		if prepareHostUnsupportedOnOS(goos) {
			t.Errorf("prepare-host must run on %s", goos)
		}
	}
}
