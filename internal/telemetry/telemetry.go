// Package telemetry emits contract-conformant events from the CLI.
//
// RFC-BACKEND-1872 D2, backend#1897. The normative rules are
// rfcs/specs/backend-1872-telemetry-contract.md; this package is the Go half of
// what the shared Python emitter (backend#1896) does for the services.
//
// WHY THE CLI NEEDS ITS OWN. Every other Class A component is a pod, so the
// edge Collector's filelog receiver reaches its stdout. The CLI is not a pod
// and runs on a user's machine, so nothing collects it — it emits its own
// outcome events through the same gateway and token (D5). Today it emits
// nothing at all, which is why a field failure is only ever a support thread.
//
// WHAT IS ENFORCED. The contract's mechanically-checkable rules are checked
// here, at the call site, and an invalid event is an error the caller cannot
// ignore: the <domain>.<object>.<outcome> grammar with its closed vocabularies,
// the attribute-key namespace, retired names, and the error set a failure must
// carry. A CLI that reports a malformed event is worse than one that reports
// nothing, because the malformed one looks like coverage.
package telemetry

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tracebloc/cli/internal/api"
)

// Service is this component's registry identity (contract §10.1). It is a
// constant, never derived from os.Args[0] or the binary's name — deriving
// service identity from the process is the defect the contract exists to close.
const (
	Service   = "cli"
	Component = "cli"
)

// UnknownVersion is what a build that injected no version reports (§4). A
// value, not an omission: it is queryable and alertable, and an absent key is
// neither. `go build` without -ldflags produces "dev", which maps here.
const UnknownVersion = "0.0.0-unknown"

// eventNameRe is §6.1's grammar: exactly three lowercase segments.
var eventNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){2}$`)

// attrKeyRe is §1.1: lowercase, dot-separated, snake_case leaves.
var attrKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// domains the CLI may emit under (§6.3). Narrower than the full registry on
// purpose: the CLI is not the installer and not the backend, so admitting
// domains it cannot legitimately produce would make a typo look plausible.
var domains = map[string]bool{
	"cli":  true, // command execution
	"auth": true, // login, token refresh — §6.3 names the cli as a primary emitter
}

// outcomes is §6.4, in full. Past tense throughout: an event records something
// that happened.
var outcomes = map[string]bool{
	"started": true, "succeeded": true, "failed": true, "skipped": true,
	"rejected": true, "retried": true, "timed_out": true, "expired": true,
	"cancelled": true, "completed": true,
}

// failureOutcomes oblige the §8.4 error set.
var failureOutcomes = map[string]bool{
	"failed": true, "rejected": true, "timed_out": true,
}

// retired names must not be emitted (§8.5). Each produced a measured defect and
// is replaced, not renamed.
var retired = map[string]bool{
	"env": true, "platform": true, "log_time": true, "traceback": true,
	"extraData": true, "pod-name": true, "pod-status": true,
	"experimentKey": true, "experimentId": true,
}

// The two layers of §1, kept as two sets. One flat allowlist is what let a call
// site smuggle a resource key into the RECORD layer — the cross-layer confusion
// that made cloud_RoleName report a process name, reintroduced at the call site
// of the package meant to close it.

// resourceScope is set once per process by New. A call site may never send one.
// tracebloc.component is listed even though it is correctly tracebloc.-prefixed:
// the namespace rule alone would wave it past.
var resourceScope = map[string]bool{
	"service.name": true, "service.version": true, "service.instance.id": true,
	"deployment.environment": true, "tracebloc.component": true,
	"tracebloc.tenant.id": true,
}

// recordScope is the set of OTel names a call site MAY send. event.name is
// absent deliberately — it is Emit's first argument, and accepting it here
// would let a caller replace the name after the grammar and failure-set checks
// had already run against the real one.
var recordScope = map[string]bool{
	"error.type": true, "exception.type": true, "exception.message": true,
	"exception.stacktrace": true,
}

// Attrs is one event's record attributes. Values are primitives (§1.2); a map
// or a serialised blob standing in for structure is the retired extraData
// defect, and is refused.
type Attrs map[string]any

// Emitter holds the resource attributes — set once, at startup, never per call
// site (§1). A call site that could change them is the layering bug that made
// cloud_RoleName report the process name.
type Emitter struct {
	resource map[string]string
	sink     func(map[string]string, map[string]any)
}

// New builds the emitter for this process.
//
// env is the CLI's own current environment ("dev"/"stg"/"prod" — the same
// values internal/api declares and internal/config stores). version is the
// -ldflags-injected build version; "dev" or empty becomes UnknownVersion.
//
// An unrecognised env is NOT an error and NOT a guess: the emitter is built
// with exporting disabled, because a value no query filters on is worse than no
// record at all (§3.2). Ask Exports() when that distinction matters.
func New(env, version, instanceID string) *Emitter {
	e := &Emitter{resource: map[string]string{
		"service.name":        Service,
		"tracebloc.component": Component,
		"service.version":     normaliseVersion(version),
	}}
	// §1.2 — omitted rather than stamped empty. os.Hostname() returns "" on
	// error, and an empty service.instance.id is the "sent as empty rather than
	// omitted" defect the record layer already refuses; the resource layer must
	// hold the same line, or the rule only applies where it is easiest.
	if id := strings.TrimSpace(instanceID); id != "" {
		e.resource["service.instance.id"] = id
	}
	if api.IsKnownEnv(env) {
		e.resource["deployment.environment"] = strings.ToLower(env)
	}
	return e
}

func normaliseVersion(v string) string {
	v = strings.TrimSpace(v)
	// "dev" is what `go build` without -ldflags reports (see cmd/tracebloc).
	// It is not a released artifact, so it is not a service.version.
	if v == "" || v == "dev" {
		return UnknownVersion
	}
	return v
}

// Exports reports whether this process may send records to the hub.
//
// Only the three known backend environments export. There is no "local" or
// "ci" for the CLI the way there is for a service — a developer's machine
// running against dev IS dev — so the distinction here is known-vs-unknown, and
// unknown never exports.
func (e *Emitter) Exports() bool {
	_, ok := e.resource["deployment.environment"]
	return ok
}

// Resource returns a copy of the per-process attributes.
func (e *Emitter) Resource() map[string]string {
	out := make(map[string]string, len(e.resource))
	for k, v := range e.resource {
		out[k] = v
	}
	return out
}

// Emit validates one occurrence against the contract and hands it to the sink.
//
// It returns an error rather than panicking or logging: a CLI must never die
// because telemetry was malformed, but a malformed event must not pass silently
// either — the caller's tests are where this is meant to fail.
func (e *Emitter) Emit(eventName string, attrs Attrs) error {
	if err := CheckEventName(eventName); err != nil {
		return err
	}
	record, err := normalise(attrs)
	if err != nil {
		return err
	}
	if err := checkFailureSet(eventName, record); err != nil {
		return err
	}
	record["event.name"] = eventName
	// Validation above ALWAYS runs; delivery does not. The "an unrecognised
	// environment never exports" guarantee lived entirely in caller discipline
	// — don't install a sink when !Exports() — even though the emitter already
	// knows. A guarantee enforced by convention at every call site is a
	// guarantee until the first call site forgets.
	if e.sink != nil && e.Exports() {
		e.sink(e.Resource(), record)
	}
	return nil
}

// SetSink installs the delivery function. Nil means validate-and-drop, which is
// what an unconfigured or non-exporting process does — the contract checks
// still run, so a bad event fails in CI regardless of where the binary is.
func (e *Emitter) SetSink(f func(resource map[string]string, record map[string]any)) {
	e.sink = f
}

// CheckEventName applies §6.1's grammar and §6.3/§6.4's closed vocabularies.
func CheckEventName(name string) error {
	if !eventNameRe.MatchString(name) {
		return fmt.Errorf(
			"telemetry: event.name %q must be <domain>.<object>.<outcome> (%s)",
			name, eventNameRe.String())
	}
	parts := strings.Split(name, ".")
	if !domains[parts[0]] {
		return fmt.Errorf(
			"telemetry: event.name %q uses domain %q, which the CLI may not emit; known: %s",
			name, parts[0], keys(domains))
	}
	if !outcomes[parts[2]] {
		return fmt.Errorf(
			"telemetry: event.name %q ends in %q, which is not a registered outcome (known: %s). "+
				"If the verb you want is missing, the ACTION belongs in the object segment: "+
				"auth.token_refresh.succeeded, not auth.token.refreshed",
			name, parts[2], keys(outcomes))
	}
	return nil
}

func normalise(attrs Attrs) (map[string]any, error) {
	out := make(map[string]any, len(attrs))
	for key, value := range attrs {
		// The KEY is validated first, always — even when the value is about to
		// be dropped. Skipping straight to `continue` let a retired or malformed
		// key pass silently whenever it happened to carry an empty value
		// (`Attrs{"experimentKey": nil}` raised nothing), which contradicts this
		// package's own rule that a malformed event must not pass quietly.
		if err := checkAttrKey(key); err != nil {
			return nil, err
		}
		// §1.2 — an absent value is omitted, never sent as nil or "". The
		// retired `traceback` key rode on every record and was empty on 99.8%.
		if value == nil {
			continue
		}
		if s, ok := value.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		if err := checkAttrValue(key, value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

func checkAttrKey(key string) error {
	if retired[key] {
		return fmt.Errorf(
			"telemetry: attribute %q is retired (contract §8.5) and must not be emitted; "+
				"retired names are replaced, not renamed", key)
	}
	if resourceScope[key] || key == "event.name" {
		return fmt.Errorf(
			"telemetry: attribute %q is RESOURCE scope (contract §1) and is set once per "+
				"process by New(); a call site may not send it. Accepting it would let one "+
				"record contradict its own process identity. event.name is Emit's first "+
				"argument, not an attribute: passing it would replace the name after the "+
				"grammar and failure-set checks had run", key)
	}
	if recordScope[key] {
		return nil
	}
	if !attrKeyRe.MatchString(key) {
		return fmt.Errorf(
			"telemetry: attribute key %q must be lowercase dot-separated with snake_case "+
				"leaves (%s) — not camelCase, not kebab-case", key, attrKeyRe.String())
	}
	if !strings.HasPrefix(key, "tracebloc.") {
		return fmt.Errorf(
			"telemetry: attribute key %q is neither an OpenTelemetry attribute nor under "+
				"the tracebloc. namespace (contract §1.1)", key)
	}
	return nil
}

// checkAttrValue applies §1.2. The accepted set is every Go scalar KIND, not a
// list of concrete types: a Go type switch matches the DYNAMIC type, so
// `case int64` does not match a time.Duration (a named type over int64) and an
// idiomatic caller would be told their duration was the retired extraData
// defect. Reflection asks the question that was actually meant.
func checkAttrValue(key string, value any) error {
	switch reflect.ValueOf(value).Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return nil
	default:
		return fmt.Errorf(
			"telemetry: attribute %q has type %T; values must be primitives (contract §1.2). "+
				"A map or a JSON string standing in for structure is the retired extraData "+
				"defect — nothing can query inside it", key, value)
	}
}

// Duration renders d for an attribute value. time.Duration IS an int64 kind and
// so would pass checkAttrValue as a raw nanosecond count — which is a number
// nobody reading a dashboard can interpret. Naming the unit at the call site is
// the point: use `telemetry.Duration(elapsed)` with a `…_ms` attribute key.
func Duration(d time.Duration) int64 { return d.Milliseconds() }

func checkFailureSet(eventName string, record map[string]any) error {
	parts := strings.Split(eventName, ".")
	if !failureOutcomes[parts[2]] {
		return nil
	}
	if _, ok := record["error.type"]; !ok {
		return fmt.Errorf(
			"telemetry: %q is a failure and must carry error.type — a stable, "+
				"low-cardinality classification (contract §8.4). Without it a failure "+
				"cannot be grouped", eventName)
	}
	// §8.4 — where an exception was caught the stacktrace is REQUIRED, not
	// optional. 0.2% of today's error records carry one.
	caught := []string{"exception.type", "exception.message", "exception.stacktrace"}
	var present, missing []string
	for _, k := range caught {
		if _, ok := record[k]; ok {
			present = append(present, k)
		} else {
			missing = append(missing, k)
		}
	}
	if len(present) > 0 && len(missing) > 0 {
		return fmt.Errorf(
			"telemetry: %q carries %v but not %v; the stacktrace is required where an "+
				"exception was caught (contract §8.4)", eventName, present, missing)
	}
	return nil
}

func keys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
