package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/telemetry"
)

// telemetryCanary is a value that could only have come off a user's command
// line: a path segment with a patient identifier in it. Written down here, and
// never derived from anything the code under test produces — a needle iterated
// out of the haystack finds itself and nothing else.
const telemetryCanary = "CANARY-PATIENT-7"

func testBuildInfo() BuildInfo {
	return BuildInfo{Version: "0.10.9", GitSHA: "abc1234", BuildDate: "2026-08-18"}
}

// isolateConfig points config.Load at an empty directory so these tests never
// read (or report on) the developer's real signed-in environment.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	t.Setenv("CLIENT_ENV", api.EnvProd)
}

// captureOutcome runs the real recorder over the real tree and returns what
// reached the sink.
func captureOutcome(
	t *testing.T, root, executed *cobra.Command, exitCode int, env map[string]string,
) (map[string]string, map[string]any, bool) {
	t.Helper()
	var res map[string]string
	var rec map[string]any
	delivered := 0
	sink := telemetry.Sink(func(r map[string]string, d map[string]any) {
		res, rec = r, d
		delivered++
	})
	getenv := func(k string) string { return env[k] }
	if err := recordCommandOutcome(
		root, executed, testBuildInfo(), exitCode, 1500*time.Millisecond, getenv, sink,
	); err != nil {
		t.Fatalf("recordCommandOutcome: %v", err)
	}
	if delivered > 1 {
		t.Fatalf("one invocation delivered %d events; the contract is exactly one", delivered)
	}
	return res, rec, delivered == 1
}

// walkTree yields every command in the tree, so the assertions below are
// derived from what actually dispatches rather than from a list somebody has to
// remember to extend.
func walkTree(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		out = append(out, c)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return out
}

// --- the closed set is the live tree ------------------------------------------

func TestEveryCommandInTheLiveTreeReportsItsOwnPath(t *testing.T) {
	// The point of deriving the set from the tree: a command added to
	// NewRootCmd is reportable the day it lands. If this ever fails for a new
	// command, the enumeration and the dispatcher have drifted — which would
	// mean that command's failures were being filed under "unregistered".
	isolateConfig(t)
	root := NewRootCmd(testBuildInfo())

	commands := walkTree(root)
	if len(commands) < 10 {
		t.Fatalf("walked only %d commands — the tree was not built", len(commands))
	}
	for _, cmd := range commands {
		t.Run(cmd.CommandPath(), func(t *testing.T) {
			_, rec, ok := captureOutcome(t, root, cmd, 0, nil)
			if !ok {
				t.Fatal("nothing was delivered")
			}
			want := commandPathOf(cmd)
			if rec[telemetry.AttrCommand] != want {
				t.Fatalf("%s = %v, want %q — this command is not in the enumerated set",
					telemetry.AttrCommand, rec[telemetry.AttrCommand], want)
			}
			if rec[telemetry.AttrCommand] == telemetry.CommandUnregistered {
				t.Fatalf("%q dispatches but is not enumerated", cmd.CommandPath())
			}
		})
	}
}

func TestTheBareRootIsNamedNotBlank(t *testing.T) {
	// commandPathOf's root case: an empty string is dropped by the emitter's
	// omit-when-absent rule, which would leave the invocation a first-time user
	// is most likely to produce as the only one with no command on the record.
	isolateConfig(t)
	root := NewRootCmd(testBuildInfo())
	_, rec, ok := captureOutcome(t, root, root, 0, nil)
	if !ok {
		t.Fatal("nothing was delivered")
	}
	if rec[telemetry.AttrCommand] != root.Name() {
		t.Fatalf("bare root reported %v, want %q", rec[telemetry.AttrCommand], root.Name())
	}
}

// --- the privacy boundary, over the real tree ---------------------------------

// TestNoFlagValueOrArgumentCanReachTheRecord is the ticket's hard boundary,
// checked against what the code emits rather than against a list of keys we
// hope nobody adds.
//
// Every command in the live tree has every one of its flags set to the canary,
// canary positional args attached, and a canary in the environment. Then the
// whole delivered payload — keys and values, resource and record — is searched.
func TestNoFlagValueOrArgumentCanReachTheRecord(t *testing.T) {
	isolateConfig(t)
	root := NewRootCmd(testBuildInfo())

	inspected := 0
	for _, cmd := range walkTree(root) {
		// Load the command up with everything a user could have typed.
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(telemetryCanary)
			f.Changed = true
		})
		cmd.SetArgs([]string{"/Users/" + telemetryCanary + "/oncology.csv", telemetryCanary})

		env := map[string]string{
			"CLIENT_ENV":           api.EnvProd,
			"TRACEBLOC_CONFIG_DIR": "/home/" + telemetryCanary + "/.tracebloc",
		}
		res, rec, ok := captureOutcome(t, root, cmd, 9, env)
		if !ok {
			t.Fatalf("%s delivered nothing", cmd.CommandPath())
		}
		for k, v := range res {
			assertNoTelemetryCanary(t, cmd.CommandPath(), "resource", k, v)
			inspected++
		}
		for k, v := range rec {
			assertNoTelemetryCanary(t, cmd.CommandPath(), "record", k, fmt.Sprint(v))
			inspected++
		}
	}
	// The anchor: an inert loop over an empty payload reads exactly like a clean
	// sweep in the log.
	if inspected < 100 {
		t.Fatalf("only %d attributes were searched — the sweep ran over nothing", inspected)
	}
}

func TestACommandPathCarryingAnArgumentIsRefusedNotCleaned(t *testing.T) {
	// The failure mode this guards: some future caller passing os.Args, or a
	// cobra change that starts including args in CommandPath(). The lookup makes
	// that a countable "unregistered", never a partially-scrubbed string.
	isolateConfig(t)
	// The impostor's NAME is the canary, so commandPathOf hands the recorder a
	// path carrying it. (Note what cobra itself does not do: Name() takes the
	// first word of Use, so `Use: "ingest <path>"` yields "ingest" — CommandPath
	// structurally cannot contain an argument today. This test is the guard for
	// the day that stops being true, or for a caller that builds the path itself.)
	impostor := &cobra.Command{Use: telemetryCanary}
	tree := NewRootCmd(testBuildInfo())
	tree.AddCommand(impostor)
	// Enumerate from a tree that never had it — the set is what main.go builds
	// from the dispatcher, and this command was never meant to be in it.
	clean := NewRootCmd(testBuildInfo())

	if got := commandPathOf(impostor); !strings.Contains(got, telemetryCanary) {
		t.Fatalf("the fixture is inert: commandPathOf gave %q, which carries no canary "+
			"for the lookup to refuse", got)
	}

	res, rec, ok := captureOutcome(t, clean, impostor, 1, nil)
	if !ok {
		t.Fatal("nothing was delivered")
	}
	if rec[telemetry.AttrCommand] != telemetry.CommandUnregistered {
		t.Fatalf("%s = %v, want %q", telemetry.AttrCommand,
			rec[telemetry.AttrCommand], telemetry.CommandUnregistered)
	}
	// Sweep the WHOLE payload, not just the one key. Checking only
	// tracebloc.cli.command leaves the canary free to arrive under any other
	// attribute — which is exactly what happened under the "smuggle the raw
	// command into a second attribute" mutation: this test stayed green while
	// the record carried the path verbatim.
	for k, v := range res {
		assertNoTelemetryCanary(t, "impostor", "resource", k, v)
	}
	for k, v := range rec {
		assertNoTelemetryCanary(t, "impostor", "record", k, fmt.Sprint(v))
	}
}

func TestTheWholePayloadIsSerialisableAndSmall(t *testing.T) {
	// The record is what a transport will put on the wire. Anything that will
	// not round-trip as JSON primitives is the retired extraData defect arriving
	// by another door, and an outcome event that needs more than a kilobyte is
	// carrying something it should not.
	isolateConfig(t)
	root := NewRootCmd(testBuildInfo())
	res, rec, ok := captureOutcome(t, root, root, 9, nil)
	if !ok {
		t.Fatal("nothing was delivered")
	}
	blob, err := json.Marshal(map[string]any{"resource": res, "attributes": rec})
	if err != nil {
		t.Fatalf("the payload does not serialise: %v", err)
	}
	if len(blob) > 1024 {
		t.Fatalf("an outcome event serialised to %d bytes: %s", len(blob), blob)
	}
}

// --- opt-out ------------------------------------------------------------------

func TestOptOutStopsEmissionEntirely(t *testing.T) {
	isolateConfig(t)
	root := NewRootCmd(testBuildInfo())

	// DERIVED: the variables the production code declares, not a list restated
	// here. Adding a spelling to telemetryOptOutVars covers it automatically.
	for _, name := range telemetryOptOutVars {
		for _, value := range []string{"1", "true", "yes", "please", "  1  "} {
			t.Run(name+"="+strings.TrimSpace(value), func(t *testing.T) {
				_, _, ok := captureOutcome(t, root, root, 0, map[string]string{name: value})
				if ok {
					t.Fatalf("%s=%q still emitted", name, value)
				}
			})
		}
	}
}

func TestTheOffSpellingsDoNotOptOut(t *testing.T) {
	// The mutation anchor for the test above: if telemetryEnabled returned false
	// unconditionally, every opt-out case would pass and the feature would be
	// dead. These are the values that must NOT disable it.
	isolateConfig(t)
	root := NewRootCmd(testBuildInfo())
	for _, value := range []string{"", "0", "false", "FALSE"} {
		t.Run("value_"+value, func(t *testing.T) {
			_, _, ok := captureOutcome(t, root, root,
				0, map[string]string{"TRACEBLOC_NO_TELEMETRY": value})
			if !ok {
				t.Fatalf("%q disabled telemetry; only an explicit opt-out should", value)
			}
		})
	}
}

// --- environment ---------------------------------------------------------------

func TestTheEnvironmentLabelMatchesTheBackend(t *testing.T) {
	// The label is the backend api.BaseURL actually targets: a known env is
	// itself; anything unrecognised is prod, because BaseURL routes it there.
	// `staging` is the classic near miss — the git branch name, not the `stg`
	// environment value — and it resolves to prod (where a client signed into
	// "staging" really goes), NOT to stg.
	for _, tc := range []struct {
		signedIn string
		want     string
	}{
		{api.EnvDev, api.EnvDev},
		{api.EnvStg, api.EnvStg},
		{"PROD", api.EnvProd},
		{"staging", api.EnvProd}, // unknown -> prod, matching api.BaseURL
		{"", api.EnvProd},        // not signed in, CLIENT_ENV empty -> prod
	} {
		t.Run("signed_in_"+tc.signedIn, func(t *testing.T) {
			t.Setenv("CLIENT_ENV", "")
			if got := telemetryEnv(tc.signedIn); got != tc.want {
				t.Fatalf("telemetryEnv(%q) = %q, want %q", tc.signedIn, got, tc.want)
			}
		})
	}
}

func TestASignedInUnknownEnvIgnoresClientEnv(t *testing.T) {
	// The bug this pins (Asad, cli#528 review): the client resolves a signed-in
	// env via sessionEnv, which returns cfg.CurrentEnv VERBATIM and never consults
	// $CLIENT_ENV — so a config on "banana" talks to prod (api.BaseURL default)
	// regardless of $CLIENT_ENV. The old code resolved the label through
	// ResolveEnv, which DOES read $CLIENT_ENV, so it filed the run under "dev"
	// while every request went to prod. The label must be prod, not dev.
	t.Setenv("CLIENT_ENV", "dev")
	if got := telemetryEnv("banana"); got != api.EnvProd {
		t.Fatalf("telemetryEnv(%q) with CLIENT_ENV=dev = %q, want %q — the label "+
			"must match the backend the client actually contacts (prod)", "banana", got, api.EnvProd)
	}
}

func TestAnUnknownClientEnvIsLabelledProd(t *testing.T) {
	// The end-to-end consequence: not signed in, CLIENT_ENV=staging. sessionEnv
	// resolves that through ResolveEnv -> "staging", and api.BaseURL routes it to
	// prod — so the run genuinely hits prod and its record must be filed under
	// prod, the population this feature exists for, not withheld.
	isolateConfig(t)
	t.Setenv("CLIENT_ENV", "staging")
	root := NewRootCmd(testBuildInfo())
	res, _, ok := captureOutcome(t, root, root, 0, nil)
	if !ok {
		t.Fatal("withheld a record for a run that hits prod under an unknown CLIENT_ENV")
	}
	if res["deployment.environment"] != api.EnvProd {
		t.Fatalf("deployment.environment = %q, want %q", res["deployment.environment"], api.EnvProd)
	}
}

func TestTheSignedInEnvironmentWins(t *testing.T) {
	// A developer signed into dev must not have their runs filed under prod.
	dir := t.TempDir()
	t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
	t.Setenv("CLIENT_ENV", "")
	body := `{"version":2,"current_env":"dev","profiles":{"dev":{"token":"x"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Read it back before asserting on the record: a skip here would be a
	// fail-open, and a config layout this fixture no longer matches must be a
	// finding, not a quiet pass.
	if got := signedInEnv(); got != api.EnvDev {
		t.Fatalf("signedInEnv() = %q, want %q — the on-disk config layout changed "+
			"and this fixture (and possibly the reader) is stale", got, api.EnvDev)
	}
	root := NewRootCmd(testBuildInfo())
	res, _, ok := captureOutcome(t, root, root, 0, nil)
	if !ok {
		t.Fatal("nothing was delivered")
	}
	if res["deployment.environment"] != api.EnvDev {
		t.Fatalf("deployment.environment = %q, want %q",
			res["deployment.environment"], api.EnvDev)
	}
}

func TestASignedInUnknownEnvironmentIsLabelledProd(t *testing.T) {
	// A run signed into an environment the CLI does not recognise talks to prod
	// (sessionEnv hands cfg.CurrentEnv to api.New verbatim, api.BaseURL routes the
	// unknown value to prod), so its record must be filed under prod — that
	// failed-install-on-prod run is exactly what this feature exists to capture.
	dir := t.TempDir()
	t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
	t.Setenv("CLIENT_ENV", "") // so the label comes from the config, not the env
	body := `{"version":2,"current_env":"banana","profiles":{"banana":{"token":"x"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Read it back before asserting: a config layout this fixture no longer
	// matches must be a finding, not a quiet pass that exercises the empty path.
	if got := signedInEnv(); got != "banana" {
		t.Fatalf("signedInEnv() = %q, want %q — the on-disk config layout changed "+
			"and this fixture (and possibly the reader) is stale", got, "banana")
	}
	root := NewRootCmd(testBuildInfo())
	res, _, ok := captureOutcome(t, root, root, 0, nil)
	if !ok {
		t.Fatal("withheld a record for a run signed into an unknown env that hits prod")
	}
	if res["deployment.environment"] != api.EnvProd {
		t.Fatalf("deployment.environment = %q, want %q", res["deployment.environment"], api.EnvProd)
	}
}

// --- instance id ---------------------------------------------------------------

func TestTheInstanceIDIsPerProcessAndNotTheHostname(t *testing.T) {
	// §2 asks for a stable per-process uuid off-cluster. Not the hostname: field
	// hostnames are overwhelmingly "<firstname>-macbook", which §7.3 forbids
	// outright. Two calls must differ, and neither may look like a host.
	host, _ := os.Hostname()
	a, b := processInstanceID(), processInstanceID()
	if a == b {
		t.Fatal("two invocations shared an instance id — that is a durable identifier")
	}
	if len(a) != 16 {
		t.Fatalf("instance id %q is not the expected 16 hex chars", a)
	}
	if host != "" && strings.Contains(a, host) {
		t.Fatalf("the instance id embeds the hostname: %q", a)
	}
}

func assertNoTelemetryCanary(t *testing.T, where, layer, key, value string) {
	t.Helper()
	if strings.Contains(key, telemetryCanary) {
		t.Fatalf("%s: %s key %q carries the canary", where, layer, key)
	}
	if strings.Contains(value, telemetryCanary) {
		t.Fatalf("%s: %s %q = %q carries the canary", where, layer, key, value)
	}
}

// TestTheDocumentedOptOutVariablesAreTheRealOnes closes the gap that makes a
// stale doc worse than no doc: a user who exports the variable
// docs/troubleshooting.md names believes they have opted out. If the name there
// has drifted from telemetryOptOutVars, they have not, and nothing else would
// ever tell them.
//
// DERIVED both ways — it parses the variable names out of the document and
// compares the SET against the production slice, so neither a rename in the
// code nor an edit to the doc can pass on its own.
func TestTheDocumentedOptOutVariablesAreTheRealOnes(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "troubleshooting.md"))
	if err != nil {
		// Fail closed: an unreadable document is not evidence of agreement.
		t.Fatalf("cannot read the document this guard checks: %v", err)
	}
	found := map[string]bool{}
	for _, m := range regexp.MustCompile(`export ([A-Z_]+)=1`).FindAllStringSubmatch(string(body), -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatal("the document names no opt-out variable — either the section was " +
			"removed (then remove this guard) or its shape changed and this parse is inert")
	}
	for _, name := range telemetryOptOutVars {
		if !found[name] {
			t.Errorf("%s disables telemetry but docs/troubleshooting.md does not tell "+
				"anyone so", name)
		}
		delete(found, name)
	}
	for name := range found {
		t.Errorf("docs/troubleshooting.md tells users to export %s, which disables "+
			"nothing — telemetryOptOutVars is %v", name, telemetryOptOutVars)
	}
}
