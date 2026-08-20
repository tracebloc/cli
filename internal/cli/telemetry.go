package cli

// Command-outcome telemetry wiring — backend#1907.
//
// One event per invocation, emitted from the single place every command path
// converges on (main.go, after ExecuteContextC returns). Hooking each handler
// instead would mean N call sites that each have to remember, and §6.5's
// "terminal event on every path" would then be true only for the handlers
// somebody remembered.
//
// WHERE THE TRANSPORT LIVES. RFC-BACKEND-1872's Collector gateway was replaced
// on 17 Aug by an ingest endpoint on the backend (rfcs#28, backend#1905), which
// now accepts OTLP/HTTP JSON (backend#2213). Delivery is implemented in
// telemetry_transport.go and the mapping in telemetry_otlp.go (backend#2217);
// this file still owns only WHAT is emitted. Validation runs on every build
// regardless of whether delivery is configured, so a malformed event fails in CI
// wherever the binary was built.

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/telemetry"
)

// telemetryOptOutVars disable emission when set. Opt-OUT, per the ticket:
// telemetry that only the already-convinced enable measures the wrong
// population, and the population this exists for is people whose install just
// failed. DO_NOT_TRACK is the cross-vendor spelling; supporting it means a user
// who has already expressed the preference once does not have to learn ours.
var telemetryOptOutVars = []string{"TRACEBLOC_NO_TELEMETRY", "DO_NOT_TRACK"}

// telemetryEnabled reports whether this invocation may emit.
//
// Anything other than the explicit "off" spellings counts as opting out. The
// asymmetry is on purpose: a user who typed TRACEBLOC_NO_TELEMETRY=please
// meant it, and guessing wrong in the other direction sends a record they
// declined.
func telemetryEnabled(getenv func(string) string) bool {
	for _, name := range telemetryOptOutVars {
		switch strings.ToLower(strings.TrimSpace(getenv(name))) {
		case "", "0", "false":
			continue
		default:
			return false
		}
	}
	return true
}

// commandPaths enumerates every path the tree can dispatch, DERIVED from the
// live tree rather than listed here. That is what makes the closed set in
// telemetry.NewOutcomeRecorder maintain itself: a command added to NewRootCmd is
// reportable the day it lands, and a value that is not a command in the tree can
// never be emitted — including one assembled out of user input.
func commandPaths(root *cobra.Command) []string {
	var out []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		out = append(out, commandPathOf(c))
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return out
}

// commandPathOf renders one command as the contract's tracebloc.cli.command
// value: the invocation minus the binary name, "data ingest" (§7.1). The bare
// root reports its own name rather than an empty string, which normalise would
// drop as absent — leaving the one invocation shape a first-time user is most
// likely to produce as the only one with no command on the record.
func commandPathOf(c *cobra.Command) string {
	if c == nil {
		return ""
	}
	path := strings.TrimSpace(c.CommandPath())
	root := c.Root().Name()
	if path == root || path == "" {
		return root
	}
	return strings.TrimSpace(strings.TrimPrefix(path, root))
}

// telemetryEnv picks deployment.environment for the records.
//
// It labels each record with the backend the client is ACTUALLY talking to,
// resolved exactly the way api.BaseURL resolves it — because that is the host
// these records are about. The mapping mirrors BaseURL: a known env is itself; a
// present-but-unrecognised value is prod, because api.BaseURL routes every
// unknown value to https://api.tracebloc.io (sessionEnv hands cfg.CurrentEnv to
// api.New verbatim). So prod is the accurate label for that population, not a
// guess — and NOT withheld: a misconfigured install that hits prod and fails is
// exactly the run this feature exists to see.
//
// $CLIENT_ENV is consulted only when there is no signed-in env, matching
// sessionEnv: once cfg.CurrentEnv is set the client ignores $CLIENT_ENV, so
// resolving a signed-in unknown through $CLIENT_ENV would label the record for a
// backend the client never contacts (the bug this replaces).
//
// NOTE: that api.BaseURL silently routes an unknown env to prod — so an install
// believing it is on another backend sends its token there — is a real defect,
// but in client.go, not here; tracked separately. This function must match that
// behaviour until it changes, not diverge from it.
func telemetryEnv(env string) string {
	resolved := env
	if resolved == "" {
		// Not signed in: $CLIENT_ENV, then the prod default (as sessionEnv does).
		resolved = api.ResolveEnv("")
	}
	if api.IsKnownEnv(resolved) {
		return strings.ToLower(resolved)
	}
	// Unrecognised: api.BaseURL sends it to prod, so prod is where these records
	// belong.
	return api.EnvProd
}

// signedInEnv reads the environment the config points at, best-effort. A
// missing or unreadable config is simply "not signed in".
func signedInEnv() string {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.CurrentEnv
}

// processInstanceID is the per-PROCESS id §2 asks for off-cluster.
//
// Not the hostname, and not a persisted machine id. Hostnames in this product's
// field data are overwhelmingly "<firstname>-macbook", which §7.3 forbids
// outright; a persisted id would be a durable identifier we would then have to
// answer erasure requests about. A fresh random value per run still separates
// concurrent runs, which is all service.instance.id is for here.
func processInstanceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Omitted rather than faked. New() drops an empty instance id, and a
		// constant stand-in would silently fuse every affected run into one.
		return ""
	}
	return hex.EncodeToString(b)
}

// RecordCommandOutcome emits the single terminal event for this invocation.
// main.go calls it once, after the command tree has returned and before exit.
//
// It never returns an error and never panics: a CLI that died because telemetry
// was unhappy would be a strictly worse CLI. A malformed event is caught by the
// tests below, where it is free.
func RecordCommandOutcome(root, executed *cobra.Command, info BuildInfo, exitCode int, elapsed time.Duration) {
	// The sink is built from the SAME resolved env the emitter is labelled with,
	// not from a second read of the config. Two independent resolutions here is
	// how a record ends up labelled `stg` and posted to prod.
	env := telemetryEnv(signedInEnv())
	_ = recordCommandOutcome(root, executed, info, exitCode, elapsed, os.Getenv, pendingSink(env))
}

// recordCommandOutcome is RecordCommandOutcome with its two ambient
// dependencies passed in, so the tests drive the real thing.
func recordCommandOutcome(
	root, executed *cobra.Command,
	info BuildInfo,
	exitCode int,
	elapsed time.Duration,
	getenv func(string) string,
	sink telemetry.Sink,
) error {
	if !telemetryEnabled(getenv) {
		return nil
	}
	emitter := telemetry.New(telemetryEnv(signedInEnv()), info.Version, processInstanceID())
	if sink != nil {
		emitter.SetSink(sink)
	}
	recorder := telemetry.NewOutcomeRecorder(emitter, commandPaths(root))
	return recorder.Record(telemetry.Outcome{
		Command:  commandPathOf(executed),
		ExitCode: exitCode,
		Elapsed:  elapsed,
	})
}
