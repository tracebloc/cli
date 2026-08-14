package telemetry

import (
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/api"
)

// --- identity ---------------------------------------------------------------

func TestServiceIdentityIsConstant(t *testing.T) {
	// §2 — never derived from os.Args[0], the binary name, or a hostname.
	// Deriving service identity from the process is what makes `gunicorn` a
	// service in today's topology.
	e := New(api.EnvProd, "0.10.7", "host-1")
	r := e.Resource()
	if r["service.name"] != "cli" || r["tracebloc.component"] != "cli" {
		t.Fatalf("identity is not the registry value: %v", r)
	}
}

func TestEveryEnvironmentTheCLIDeclaresIsAccepted(t *testing.T) {
	// DERIVED, not restated: these are internal/api's own constants. If the CLI
	// gains an environment there, this fails until telemetry classifies it —
	// the alternative is records landing under a value no query filters on.
	for _, env := range []string{api.EnvDev, api.EnvStg, api.EnvProd} {
		t.Run(env, func(t *testing.T) {
			e := New(env, "0.10.7", "host-1")
			if !e.Exports() {
				t.Fatalf("%q is a declared CLI environment but does not export", env)
			}
			if got := e.Resource()["deployment.environment"]; got != env {
				t.Fatalf("deployment.environment = %q, want %q", got, env)
			}
		})
	}
}

func TestTelemetryAgreesWithIsKnownEnv(t *testing.T) {
	// The two must not drift: api.IsKnownEnv is what rejects a `--env staging`
	// typo at the CLI's front door, and this is what decides whether the same
	// value reaches the hub. One saying yes while the other says no is how
	// records get a guessed environment.
	for _, env := range []string{"dev", "stg", "prod", "staging", "prd", "PROD", ""} {
		t.Run(env, func(t *testing.T) {
			e := New(env, "0.10.7", "host-1")
			if e.Exports() != api.IsKnownEnv(env) {
				t.Fatalf("Exports()=%v but api.IsKnownEnv(%q)=%v",
					e.Exports(), env, api.IsKnownEnv(env))
			}
		})
	}
}

func TestAnUnknownEnvironmentIsNotGuessed(t *testing.T) {
	// §3.2 — a value no query filters on is worse than no record. `staging` is
	// the classic near miss: it is the git branch name, and `stg` is the
	// environment value.
	e := New("staging", "0.10.7", "host-1")
	if e.Exports() {
		t.Fatal("an unrecognised environment exported")
	}
	if _, ok := e.Resource()["deployment.environment"]; ok {
		t.Fatal("an unrecognised environment was stamped anyway")
	}
}

func TestVersionUnknownIsAValueNotAnOmission(t *testing.T) {
	// `go build` without -ldflags reports "dev", which is not a released
	// artifact. §4 wants 0.0.0-unknown: queryable and alertable.
	for _, in := range []string{"", "dev", "   "} {
		e := New(api.EnvProd, in, "host-1")
		if got := e.Resource()["service.version"]; got != UnknownVersion {
			t.Fatalf("version %q -> %q, want %q", in, got, UnknownVersion)
		}
	}
	if got := New(api.EnvProd, "0.10.7", "h").Resource()["service.version"]; got != "0.10.7" {
		t.Fatalf("an injected version was lost: %q", got)
	}
}

// --- event names ------------------------------------------------------------

func TestConformingEventNamesAreAccepted(t *testing.T) {
	e := New(api.EnvProd, "0.10.7", "h")
	for _, name := range []string{
		"cli.command.started", "cli.command.succeeded",
		"auth.token_refresh.succeeded", "auth.login.failed",
	} {
		t.Run(name, func(t *testing.T) {
			attrs := Attrs{}
			if strings.HasSuffix(name, "failed") {
				attrs["error.type"] = "x"
			}
			if err := e.Emit(name, attrs); err != nil {
				t.Fatalf("rejected a conforming name: %v", err)
			}
		})
	}
}

func TestTheGrammarIsExactlyThreeSegments(t *testing.T) {
	for _, name := range []string{
		"cli.failed", "cli.command.run.failed", "CLI.command.failed",
		"cli.Command.failed", "cli-command-failed", "cli..failed", "",
	} {
		t.Run(name, func(t *testing.T) {
			if err := CheckEventName(name); err == nil {
				t.Fatalf("accepted a malformed name %q", name)
			}
		})
	}
}

func TestAnEventNameMayNotCarryASubject(t *testing.T) {
	// `cli.command.e0qaz0zi.failed` has four segments — the grammar enforces
	// "no subject in the name" rather than merely stating it. The subject is an
	// attribute (§7).
	if err := CheckEventName("cli.command.e0qaz0zi.failed"); err == nil {
		t.Fatal("accepted a name carrying a subject")
	}
}

func TestADomainTheCLICannotEmitIsRefused(t *testing.T) {
	// Narrower than the full registry on purpose: the CLI is not the installer
	// and not the backend. 461 of 484 browser events are named `not_specified`
	// because an unregistered value became a new silent namespace.
	for _, name := range []string{
		"install.preflight.failed", "training.job.failed", "telemetry.thing.failed",
	} {
		if err := CheckEventName(name); err == nil {
			t.Fatalf("accepted a domain the CLI may not emit: %q", name)
		}
	}
}

func TestAnUnregisteredOutcomeIsRefusedAndSuggestsTheFix(t *testing.T) {
	err := CheckEventName("auth.token.refreshed")
	if err == nil {
		t.Fatal("accepted an unregistered outcome")
	}
	if !strings.Contains(err.Error(), "auth.token_refresh.succeeded") {
		t.Fatalf("the error does not point at the fix: %v", err)
	}
}

// --- attributes -------------------------------------------------------------

func TestRetiredNamesAreRefusedAsRetired(t *testing.T) {
	// Asserting WHICH rule fires. Every retired name is also caught by the
	// namespace or key-shape rule, so a test that only checks "it errors"
	// passes with the retired check deleted — the Python side proved that under
	// mutation. The message is what tells a caller the field was replaced.
	e := New(api.EnvProd, "0.10.7", "h")
	for name := range retired {
		t.Run(name, func(t *testing.T) {
			err := e.Emit("cli.command.succeeded", Attrs{name: "x"})
			if err == nil {
				t.Fatalf("accepted retired attribute %q", name)
			}
			if !strings.Contains(err.Error(), "retired") {
				t.Fatalf("%q was rejected by another rule, so the retired check "+
					"is doing no work: %v", name, err)
			}
		})
	}
}

func TestKeyShapeAndNamespace(t *testing.T) {
	e := New(api.EnvProd, "0.10.7", "h")
	for _, key := range []string{"experimentKey", "pod-name", "cluster.name", "Foo"} {
		t.Run(key, func(t *testing.T) {
			if err := e.Emit("cli.command.succeeded", Attrs{key: "x"}); err == nil {
				t.Fatalf("accepted key %q", key)
			}
		})
	}
	// The shape rule needs a key that passes every OTHER rule: not retired, not
	// an OTel name, correctly under `tracebloc.` — and badly shaped. Without
	// one, the namespace rule catches every case and the shape check does no
	// work; it survived mutation until this was added.
	for _, key := range []string{
		"tracebloc.clientID",  // camelCase leaf
		"tracebloc.client-id", // kebab leaf
		"tracebloc.Client.id", // capitalised segment
	} {
		t.Run(key, func(t *testing.T) {
			err := e.Emit("cli.command.succeeded", Attrs{key: "x"})
			if err == nil {
				t.Fatalf("accepted badly-shaped key %q", key)
			}
			if !strings.Contains(err.Error(), "snake_case") {
				t.Fatalf("%q was rejected by another rule, so the key-shape "+
					"check is doing no work: %v", key, err)
			}
		})
	}

	if err := e.Emit("cli.command.succeeded", Attrs{"tracebloc.client.id": "abc"}); err != nil {
		t.Fatalf("rejected a valid tracebloc key: %v", err)
	}
	if err := e.Emit("cli.command.succeeded", Attrs{"error.type": "x"}); err != nil {
		t.Fatalf("rejected a valid OTel key: %v", err)
	}
}

func TestAbsentValuesAreOmittedNotSent(t *testing.T) {
	// The retired `traceback` key rode on every record and was empty on 99.8%.
	e := New(api.EnvProd, "0.10.7", "h")
	var got map[string]any
	e.SetSink(func(_ map[string]string, record map[string]any) { got = record })
	if err := e.Emit("cli.command.succeeded", Attrs{
		"tracebloc.client.id": nil,
		"tracebloc.note":      "",
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"tracebloc.client.id", "tracebloc.note"} {
		if _, ok := got[k]; ok {
			t.Fatalf("%q was sent empty rather than omitted", k)
		}
	}
}

func TestNonPrimitiveValuesAreRefused(t *testing.T) {
	e := New(api.EnvProd, "0.10.7", "h")
	if err := e.Emit("cli.command.succeeded", Attrs{
		"tracebloc.detail": map[string]string{"a": "b"},
	}); err == nil {
		t.Fatal("accepted a map value — that is the retired extraData defect")
	}
}

// --- failures ---------------------------------------------------------------

func TestAFailureMustCarryErrorType(t *testing.T) {
	e := New(api.EnvProd, "0.10.7", "h")
	for _, outcome := range []string{"failed", "rejected", "timed_out"} {
		t.Run(outcome, func(t *testing.T) {
			if err := e.Emit("cli.command."+outcome, Attrs{}); err == nil {
				t.Fatalf("accepted a %s with no error.type", outcome)
			}
		})
	}
}

func TestACaughtExceptionMustBringItsStacktrace(t *testing.T) {
	e := New(api.EnvProd, "0.10.7", "h")
	err := e.Emit("cli.command.failed", Attrs{
		"error.type":        "network",
		"exception.type":    "net.OpError",
		"exception.message": "boom",
	})
	if err == nil || !strings.Contains(err.Error(), "stacktrace") {
		t.Fatalf("a partial exception set was accepted: %v", err)
	}
}

func TestACompleteFailureRecordIsAccepted(t *testing.T) {
	e := New(api.EnvProd, "0.10.7", "h")
	if err := e.Emit("cli.command.failed", Attrs{
		"error.type":           "network",
		"exception.type":       "net.OpError",
		"exception.message":    "boom",
		"exception.stacktrace": "goroutine 1…",
	}); err != nil {
		t.Fatalf("rejected a complete failure record: %v", err)
	}
}

// --- delivery ---------------------------------------------------------------

func TestValidationRunsEvenWithNoSink(t *testing.T) {
	// A non-exporting build must still fail a malformed event, or the contract
	// is only enforced where it is least likely to be tested.
	e := New("staging", "0.10.7", "h") // not exporting
	if err := e.Emit("cli.command.refreshed", Attrs{}); err == nil {
		t.Fatal("a non-exporting emitter skipped validation")
	}
}

func TestTheSinkReceivesResourceAndRecordSeparately(t *testing.T) {
	// §1's layering: resource describes the emitter, record the occurrence. A
	// sink that saw them merged could not tell which layer a key came from —
	// which is the confusion that produced cloud_RoleName.
	e := New(api.EnvProd, "0.10.7", "h")
	var res map[string]string
	var rec map[string]any
	e.SetSink(func(r map[string]string, d map[string]any) { res, rec = r, d })
	if err := e.Emit("cli.command.succeeded", Attrs{"tracebloc.client.id": "abc"}); err != nil {
		t.Fatal(err)
	}
	if res["service.name"] != "cli" {
		t.Fatalf("resource lost its identity: %v", res)
	}
	if rec["event.name"] != "cli.command.succeeded" || rec["tracebloc.client.id"] != "abc" {
		t.Fatalf("record is wrong: %v", rec)
	}
	if _, leaked := rec["service.name"]; leaked {
		t.Fatal("a resource attribute leaked into the record layer")
	}
}

func TestResourceIsACopy(t *testing.T) {
	// A call site that could mutate the resource is the §1 bug.
	e := New(api.EnvProd, "0.10.7", "h")
	e.Resource()["service.name"] = "backend"
	if e.Resource()["service.name"] != "cli" {
		t.Fatal("the resource is mutable from outside")
	}
}
