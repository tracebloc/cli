package telemetry

import (
	"strings"
	"testing"
	"time"

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

// --- review findings on cli#503 (merged) ------------------------------------

func TestNoResourceScopeKeyMayComeFromACallSite(t *testing.T) {
	// §1 — a call site that sets a resource attribute is a bug. One flat
	// allowlist let a caller smuggle one into the RECORD layer, so a single
	// record could contradict its own process identity: the cloud_RoleName
	// cross-layer confusion, at the call site of the package meant to close it.
	e := New(api.EnvProd, "0.10.7", "h")
	for key := range resourceScope {
		t.Run(key, func(t *testing.T) {
			err := e.Emit("cli.command.succeeded", Attrs{key: "impostor"})
			if err == nil {
				t.Fatalf("accepted resource-scope key %q from a call site", key)
			}
			if !strings.Contains(err.Error(), "RESOURCE scope") {
				t.Fatalf("%q was rejected by another rule, so the layer check "+
					"is doing no work for it: %v", key, err)
			}
		})
	}
}

func TestEventNameCannotBePassedAsAnAttribute(t *testing.T) {
	// It is Emit's first argument. Accepting it would replace the name AFTER
	// the grammar and failure-set checks ran against the real one — delivering
	// a failure-shaped name that skipped §8.4.
	e := New(api.EnvProd, "0.10.7", "h")
	err := e.Emit("cli.command.succeeded", Attrs{"event.name": "cli.command.failed"})
	if err == nil {
		t.Fatal("accepted event.name as an attribute")
	}
	// Asserting WHICH rule: event.name is also not tracebloc.-prefixed, so the
	// namespace rule would reject it too, and a test that only checked "it
	// errors" would pass with this guard deleted.
	if !strings.Contains(err.Error(), "Emit's first argument") {
		t.Fatalf("rejected by another rule: %v", err)
	}
}

func TestRecordScopeOTelKeysStillPass(t *testing.T) {
	// The split must not become a blanket ban on OTel names.
	e := New(api.EnvProd, "0.10.7", "h")
	if err := e.Emit("cli.command.failed", Attrs{
		"error.type":           "network",
		"exception.type":       "net.OpError",
		"exception.message":    "boom",
		"exception.stacktrace": "goroutine 1…",
	}); err != nil {
		t.Fatalf("rejected record-scope OTel keys: %v", err)
	}
}

func TestGoScalarKindsAreAcceptedNotJustNamedTypes(t *testing.T) {
	// A Go type switch matches the DYNAMIC type, so `case int64` does not match
	// a time.Duration. An idiomatic caller in #1907 would have been told their
	// duration was the retired extraData defect.
	e := New(api.EnvProd, "0.10.7", "h")
	for name, v := range map[string]any{
		"duration": 5 * time.Second,
		"int32":    int32(3),
		"uint":     uint(3),
		"uint64":   uint64(3),
		"float32":  float32(1.5),
		"named":    time.Duration(7),
	} {
		t.Run(name, func(t *testing.T) {
			if err := e.Emit("cli.command.succeeded", Attrs{"tracebloc.v": v}); err != nil {
				t.Fatalf("rejected a Go scalar %T: %v", v, err)
			}
		})
	}
	// …and a genuine non-scalar is still refused.
	if err := e.Emit("cli.command.succeeded", Attrs{
		"tracebloc.v": map[string]string{"a": "b"},
	}); err == nil {
		t.Fatal("accepted a map — that is the extraData defect")
	}
}

func TestDurationHelperRendersMilliseconds(t *testing.T) {
	// time.Duration passes checkAttrValue as a raw NANOSECOND count, which is a
	// number nobody reading a dashboard can interpret. The helper names the unit.
	if got := Duration(1500 * time.Millisecond); got != 1500 {
		t.Fatalf("Duration = %d, want 1500", got)
	}
}

func TestAnEmptyInstanceIDIsOmittedNotStamped(t *testing.T) {
	// os.Hostname() returns "" on error. An empty service.instance.id is the
	// "sent as empty rather than omitted" defect the RECORD layer already
	// refuses; the resource layer must hold the same line.
	for _, id := range []string{"", "   "} {
		r := New(api.EnvProd, "0.10.7", id).Resource()
		if v, ok := r["service.instance.id"]; ok {
			t.Fatalf("stamped service.instance.id = %q for input %q", v, id)
		}
	}
	if r := New(api.EnvProd, "0.10.7", "host-1").Resource(); r["service.instance.id"] != "host-1" {
		t.Fatal("a real instance id was lost")
	}
}

func TestDeliveryIsGatedOnExports(t *testing.T) {
	// The "unrecognised environment never exports" guarantee lived entirely in
	// caller discipline, even though the emitter already knew.
	u := New("staging", "0.10.7", "h") // not a known env
	delivered := false
	u.SetSink(func(map[string]string, map[string]any) { delivered = true })
	if err := u.Emit("cli.command.succeeded", Attrs{}); err != nil {
		t.Fatalf("validation should still run: %v", err)
	}
	if delivered {
		t.Fatal("delivered a record although Exports() is false")
	}

	// …and validation still runs on that path, so a bad event fails in CI
	// wherever the binary is built.
	if err := u.Emit("cli.command.refreshed", Attrs{}); err == nil {
		t.Fatal("a non-exporting emitter skipped validation")
	}

	// The exporting path still delivers.
	e := New(api.EnvProd, "0.10.7", "h")
	got := false
	e.SetSink(func(map[string]string, map[string]any) { got = true })
	if err := e.Emit("cli.command.succeeded", Attrs{}); err != nil || !got {
		t.Fatalf("an exporting emitter did not deliver (err=%v)", err)
	}
}

func TestAKeyIsValidatedEvenWhenItsValueIsDropped(t *testing.T) {
	// A retired or malformed key that happened to carry an empty value escaped
	// every rule — contradicting this package's own "a malformed event must not
	// pass silently".
	e := New(api.EnvProd, "0.10.7", "h")
	for name, attrs := range map[string]Attrs{
		"retired nil":   {"experimentKey": nil},
		"retired empty": {"extraData": ""},
		"bad shape nil": {"tracebloc.clientID": nil},
		"unprefixed":    {"cluster.name": ""},
	} {
		t.Run(name, func(t *testing.T) {
			if err := e.Emit("cli.command.succeeded", attrs); err == nil {
				t.Fatalf("a bad key passed silently because its value was empty: %v", attrs)
			}
		})
	}
	// A GOOD key with an empty value is still dropped, not an error.
	var got map[string]any
	e.SetSink(func(_ map[string]string, r map[string]any) { got = r })
	if err := e.Emit("cli.command.succeeded", Attrs{"tracebloc.note": ""}); err != nil {
		t.Fatalf("a valid key with an empty value should be dropped, not rejected: %v", err)
	}
	if _, ok := got["tracebloc.note"]; ok {
		t.Fatal("an empty value was sent rather than omitted")
	}
}
