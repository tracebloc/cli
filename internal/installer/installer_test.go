package installer

import (
	"os/exec"
	"strings"
	"testing"
)

// The idiom is both executed and printed for a human to paste, so it has to be
// ONE line. A multi-line block pastes unreliably — notably PowerShell, where a
// multi-line paste can run bottom-up and execute the installer before the
// download. Guard every shape we generate (cli#396).
func TestScriptIsSingleLine(t *testing.T) {
	for _, tc := range []struct{ sub, env string }{
		{"", ""},
		{"prepare-host", ""},
		{"prepare-host", "TB_PREPARE_USER=alice"},
	} {
		if got := Script(tc.sub, tc.env); strings.Contains(got, "\n") {
			t.Errorf("Script(%q,%q) must be a single line (paste-safe), got:\n%q", tc.sub, tc.env, got)
		}
	}
}

// Structural guards on the shared idiom. Each is load-bearing; see Script's doc.
func TestScriptShape(t *testing.T) {
	s := Cmd
	checks := []struct {
		want, why string
	}{
		{"set -e", "fail closed: a non-zero curl must abort, not run an empty script"},
		{"curl", "download the installer"},
		{"-o ", "download to a FILE so curl's exit is checked (not piped)"},
		{"--tlsv1.2", "pin the TLS 1.2 floor on this privileged download (Bugbot #397)"},
		{`bash "$tmp"`, "run the downloaded file, keeping stdin on the TTY"},
		{URL, "point at the single-source installer URL"},
	}
	for _, c := range checks {
		if !strings.Contains(s, c.want) {
			t.Errorf("Cmd missing %q — %s; got: %q", c.want, c.why, s)
		}
	}
	// Must NOT pipe or process-substitute the script into bash: both steal the
	// installer's stdin AND fail open (bash reads an empty script and exits 0 when
	// curl fails). These are exactly the shapes cli#396 replaced.
	for _, bad := range []string{"| bash", "|bash", "<(", "curl -fsSL " + URL + ")"} {
		if strings.Contains(s, bad) {
			t.Errorf("Cmd must not contain %q (steals stdin / fails open); got: %q", bad, s)
		}
	}
	// A subshell wrapper is what makes it paste-safe: set -e / trap stay scoped to
	// the subshell instead of arming errexit on the user's interactive shell.
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		t.Errorf("Cmd must be wrapped in a subshell so set -e/trap don't leak into the pasting shell; got: %q", s)
	}
}

// Executable proof of the two properties that matter, run against a bogus
// endpoint so no network is touched: (1) a failed download makes the command
// exit non-zero — the whole point of the change, since the old `bash <(curl …)`
// / `curl | bash` shapes exited 0 on a curl failure; and (2) pasting it into an
// interactive-style shell does NOT arm errexit or leave a trap behind on the
// caller's shell (the subshell contains both). We swap only the endpoint in the
// real generated string, so the shell shape under test is exactly what ships.
func TestScriptFailsClosedWithoutLeakingErrexit(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// http://127.0.0.1:1 — a port nothing listens on: curl fails fast, offline.
	badCmd := strings.Replace(Cmd, URL, "http://127.0.0.1:1/i.sh", 1)
	if !strings.Contains(badCmd, "127.0.0.1:1") {
		t.Fatalf("could not substitute endpoint in %q", Cmd)
	}

	// The pasted command, then two lines that only run if it did NOT kill the
	// shell and did NOT leave errexit armed. The final `false` would abort an
	// errexit-armed shell before the last echo.
	script := badCmd + "\n" +
		`echo "SURVIVED status=$?"` + "\n" +
		`false; echo "NO-ERREXIT-LEAK"` + "\n" +
		`trap -p EXIT | grep -q "rm -f" && echo "TRAP-LEAKED" || echo "NO-TRAP-LEAK"`

	out, _ := exec.Command("bash", "-c", script).CombinedOutput()
	got := string(out)
	if !strings.Contains(got, "SURVIVED status=") {
		t.Fatalf("pasting the command killed the shell — set -e/trap leaked; output:\n%s", got)
	}
	// The failed download must have produced a non-zero status inside the subshell.
	if strings.Contains(got, "SURVIVED status=0") {
		t.Errorf("command exited 0 on a failed download — NOT fail-closed; output:\n%s", got)
	}
	if !strings.Contains(got, "NO-ERREXIT-LEAK") {
		t.Errorf("errexit leaked into the pasting shell; output:\n%s", got)
	}
	if !strings.Contains(got, "NO-TRAP-LEAK") {
		t.Errorf("the EXIT trap leaked into the pasting shell; output:\n%s", got)
	}
}
