package telemetry

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tracebloc/cli/internal/api"
)

// registered is the stand-in for what internal/cli enumerates from the live
// cobra tree. It is deliberately NOT the real tree here: this package must not
// know about cobra, and the tree-derived version of the same assertion lives in
// internal/cli/telemetry_test.go.
var registered = []string{"tracebloc", "data ingest", "cluster info", "login"}

func recorderWithSink(t *testing.T) (*OutcomeRecorder, func() (map[string]string, map[string]any)) {
	t.Helper()
	e := New(api.EnvProd, "0.10.9", "abcdef0123456789")
	var res map[string]string
	var rec map[string]any
	e.SetSink(func(r map[string]string, d map[string]any) { res, rec = r, d })
	return NewOutcomeRecorder(e, registered), func() (map[string]string, map[string]any) { return res, rec }
}

// --- the event name is the outcome ------------------------------------------

func TestTheEventNameFollowsTheExitCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want string
	}{
		{"success", 0, EventCommandSucceeded},
		{"generic failure", 1, EventCommandFailed},
		{"bad input", 2, EventCommandFailed},
		{"ingest failed", 9, EventCommandFailed},
		{"interrupted", ExitCancelled, EventCommandCancelled},
		{"a code no table knows", 77, EventCommandFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, read := recorderWithSink(t)
			if err := r.Record(Outcome{Command: "data ingest", ExitCode: tc.code}); err != nil {
				t.Fatalf("Record: %v", err)
			}
			_, rec := read()
			if rec["event.name"] != tc.want {
				t.Fatalf("exit %d emitted %q, want %q", tc.code, rec["event.name"], tc.want)
			}
		})
	}
}

func TestACancelIsNotAFailure(t *testing.T) {
	// 130 is the user pressing Ctrl-C. Counting it as a failure moves the rate
	// D9's alerts are written against every time somebody changes their mind,
	// so the record must carry no error.type at all — not even an "ok" one.
	r, read := recorderWithSink(t)
	if err := r.Record(Outcome{Command: "login", ExitCode: ExitCancelled}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	_, rec := read()
	if _, ok := rec["error.type"]; ok {
		t.Fatalf("a cancel carried error.type=%v", rec["error.type"])
	}
	if rec[AttrExitCode] != ExitCancelled {
		t.Fatalf("the exit code was lost: %v", rec[AttrExitCode])
	}
}

func TestEveryFailureCarriesAClassFromTheClosedVocabulary(t *testing.T) {
	// DERIVED input domain: every key the production table declares, plus codes
	// outside it. Mutation coverage cannot see a vocabulary gap (workspace
	// CLAUDE.md rule 6), so the domain comes from the producer's own surface.
	codes := []int{}
	for code := range exitClasses {
		codes = append(codes, code)
	}
	codes = append(codes, 42, 77, 255, -1)

	allowed := map[string]bool{ClassUnclassified: true}
	for _, class := range exitClasses {
		allowed[class] = true
	}

	for _, code := range codes {
		t.Run(fmt.Sprintf("exit_%d", code), func(t *testing.T) {
			r, read := recorderWithSink(t)
			if err := r.Record(Outcome{Command: "data ingest", ExitCode: code}); err != nil {
				t.Fatalf("Record: %v", err)
			}
			_, rec := read()
			class, ok := rec["error.type"].(string)
			if !ok {
				t.Fatalf("exit %d produced no error.type: %v", code, rec)
			}
			if !allowed[class] {
				t.Fatalf("exit %d produced error.type %q, which is outside the "+
					"closed vocabulary %v", code, class, allowed)
			}
		})
	}
}

func TestAnUnmappedExitCodeIsUnclassifiedNotStringified(t *testing.T) {
	// Fail closed: a code the table has not seen must be a countable "we cannot
	// name this", never the number rendered into a new value that appears on its
	// own. Asserting WHICH answer, because "some member of the vocabulary" is
	// also satisfied by returning ClassUnspecifiedFailure for everything.
	if got := ClassifyExit(77); got != ClassUnclassified {
		t.Fatalf("ClassifyExit(77) = %q, want %q", got, ClassUnclassified)
	}
	if got := ClassifyExit(2); got != ClassInvalidInput {
		t.Fatalf("ClassifyExit(2) = %q, want %q — the mapped codes must still map",
			got, ClassInvalidInput)
	}
}

// --- the privacy boundary ----------------------------------------------------

// canary is a value that could only have arrived from a user's command line. It
// is written down here, independently of anything the matcher checks — never
// iterated out of the thing under test (workspace CLAUDE.md rule 9).
const canary = "CANARY-PATIENT-7"

func TestAnUnregisteredCommandIsReplacedNotSanitised(t *testing.T) {
	// The whole ticket, in one assertion: an argument-bearing path is not
	// cleaned up, it is refused. The two checks are separate on purpose — the
	// "canary absent" half alone would pass if the attribute were dropped
	// entirely, and the "value is unregistered" half is the mutation anchor: it
	// reddens the moment commandValue stops looking the path up.
	for _, path := range []string{
		"data ingest /Users/" + canary + "/oncology.csv",
		"data ingest --name " + canary,
		"login --token " + canary,
		canary,
		"",
	} {
		t.Run(path, func(t *testing.T) {
			r, read := recorderWithSink(t)
			if err := r.Record(Outcome{Command: path, ExitCode: 0}); err != nil {
				t.Fatalf("Record: %v", err)
			}
			res, rec := read()
			if rec[AttrCommand] != CommandUnregistered {
				t.Fatalf("%s = %v, want %q", AttrCommand, rec[AttrCommand], CommandUnregistered)
			}
			assertNoCanary(t, res, rec)
		})
	}
}

func TestARegisteredCommandSurvivesIntact(t *testing.T) {
	// The other half: the lookup must not become a blanket refusal, or the
	// column is CommandUnregistered forever and nothing above is doing any work.
	for _, path := range registered {
		t.Run(path, func(t *testing.T) {
			r, read := recorderWithSink(t)
			if err := r.Record(Outcome{Command: path, ExitCode: 0}); err != nil {
				t.Fatalf("Record: %v", err)
			}
			_, rec := read()
			if rec[AttrCommand] != path {
				t.Fatalf("%s = %v, want %q", AttrCommand, rec[AttrCommand], path)
			}
		})
	}
}

func TestARecorderWithNoRegisteredCommandsFailsClosed(t *testing.T) {
	// Forgetting to register must not turn the lookup into a pass-through.
	e := New(api.EnvProd, "0.10.9", "abcdef0123456789")
	var rec map[string]any
	e.SetSink(func(_ map[string]string, d map[string]any) { rec = d })
	r := NewOutcomeRecorder(e, nil)
	if err := r.Record(Outcome{Command: "data ingest", ExitCode: 0}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if rec[AttrCommand] != CommandUnregistered {
		t.Fatalf("an empty registry reported %v, want %q", rec[AttrCommand], CommandUnregistered)
	}
}

// TestEveryEmittedStringComesFromAClosedSet is the derived form of "no
// arguments, no paths, no data".
//
// It does not hold a list of forbidden keys — a list like that agrees with
// itself and says nothing about the twentieth attribute somebody adds. It walks
// what the code ACTUALLY emits and requires every value to be an int, or a
// member of a set assembled from the producer's own declarations. A free-text
// channel of any kind fails it, whether or not anyone thought to forbid the
// thing travelling down it.
func TestEveryEmittedStringComesFromAClosedSet(t *testing.T) {
	allowed := map[string]bool{
		EventCommandSucceeded: true,
		EventCommandFailed:    true,
		EventCommandCancelled: true,
		CommandUnregistered:   true,
		ClassUnclassified:     true,
	}
	for _, c := range registered {
		allowed[c] = true
	}
	for _, class := range exitClasses {
		allowed[class] = true
	}

	// Resource values are process identity, not occurrence data: fixed strings,
	// or shapes with no room for a payload.
	shaped := map[string]*regexp.Regexp{
		"service.instance.id": regexp.MustCompile(`^[0-9a-f]{16}$`),
		"service.version":     regexp.MustCompile(`^[0-9A-Za-z.\-+]{1,32}$`),
	}
	fixed := map[string]string{
		"service.name":           Service,
		"tracebloc.component":    Component,
		"os.type":                runtime.GOOS,
		"host.arch":              runtime.GOARCH,
		"deployment.environment": api.EnvProd,
	}

	paths := append([]string{}, registered...)
	paths = append(paths, "data ingest /var/"+canary+"/rows.csv", canary, "")
	codes := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 42, ExitCancelled}

	seen := 0
	for _, path := range paths {
		for _, code := range codes {
			r, read := recorderWithSink(t)
			if err := r.Record(Outcome{
				Command: path, ExitCode: code, Elapsed: 1234 * time.Millisecond,
			}); err != nil {
				t.Fatalf("Record(%q, %d): %v", path, code, err)
			}
			res, rec := read()

			for k, v := range rec {
				seen++
				switch value := v.(type) {
				case int, int64:
				case string:
					if !allowed[value] {
						t.Fatalf("record key %q carried %q, which is not in any "+
							"declared vocabulary — that is a free-text channel", k, value)
					}
				default:
					t.Fatalf("record key %q carried %T; the record must be ints and "+
						"closed-set strings only", k, v)
				}
			}
			for k, v := range res {
				seen++
				if want, ok := fixed[k]; ok {
					if v != want {
						t.Fatalf("resource %q = %q, want %q", k, v, want)
					}
					continue
				}
				re, ok := shaped[k]
				if !ok {
					t.Fatalf("resource carries %q, which this guard has never been "+
						"taught to constrain — classify it before shipping it", k)
				}
				if !re.MatchString(v) {
					t.Fatalf("resource %q = %q, outside %s", k, v, re)
				}
			}
		}
	}
	// An inert run and full coverage look identical in a log. This is the anchor
	// that says the loop above actually inspected something.
	if want := len(paths) * len(codes) * 10; seen < want {
		t.Fatalf("only %d attributes were inspected (expected at least %d) — the "+
			"guard ran over an empty record", seen, want)
	}
}

// --- measurements -------------------------------------------------------------

func TestDurationIsMillisecondsAndNeverNegative(t *testing.T) {
	r, read := recorderWithSink(t)
	if err := r.Record(Outcome{
		Command: "data ingest", ExitCode: 0, Elapsed: 2500 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	_, rec := read()
	if rec[AttrDurationMS] != int64(2500) {
		t.Fatalf("%s = %v, want 2500", AttrDurationMS, rec[AttrDurationMS])
	}

	// A clock step can hand us a negative delta. A negative duration is not a
	// measurement — it skews every percentile computed over the column.
	r2, read2 := recorderWithSink(t)
	if err := r2.Record(Outcome{
		Command: "data ingest", ExitCode: 0, Elapsed: -5 * time.Second,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	_, rec2 := read2()
	if rec2[AttrDurationMS] != int64(0) {
		t.Fatalf("a negative elapsed became %v, want 0", rec2[AttrDurationMS])
	}
}

func TestAZeroDurationIsStillReported(t *testing.T) {
	// A sub-millisecond command rounds to 0, and 0 is a measurement. §1.2's
	// omit-when-absent rule must not swallow it — a command that always returns
	// instantly would otherwise have no duration column at all.
	r, read := recorderWithSink(t)
	if err := r.Record(Outcome{Command: "tracebloc", ExitCode: 0, Elapsed: 0}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	_, rec := read()
	if _, ok := rec[AttrDurationMS]; !ok {
		t.Fatalf("a 0 ms duration was dropped as absent: %v", rec)
	}
}

// --- the resource layer -------------------------------------------------------

func TestOSAndArchAreOTelNamesInTheResourceLayer(t *testing.T) {
	// §1.1 forbids re-inventing an attribute OTel already names, so these are
	// os.type / host.arch and not tracebloc.os / tracebloc.arch. They are
	// compile-time constants of the binary, so they belong to the process, not
	// to the occurrence.
	res := New(api.EnvProd, "0.10.9", "h").Resource()
	if res["os.type"] != runtime.GOOS {
		t.Fatalf("os.type = %q, want %q", res["os.type"], runtime.GOOS)
	}
	if res["host.arch"] != runtime.GOARCH {
		t.Fatalf("host.arch = %q, want %q", res["host.arch"], runtime.GOARCH)
	}
	// …and being resource scope, a call site may not send them. The generic
	// version of this assertion iterates resourceScope, so it covers these two
	// automatically; this names the rule that must fire.
	e := New(api.EnvProd, "0.10.9", "h")
	for _, k := range []string{"os.type", "host.arch"} {
		err := e.Emit(EventCommandSucceeded, Attrs{k: "impostor"})
		if err == nil {
			t.Fatalf("a call site set %q", k)
		}
		if !strings.Contains(err.Error(), "RESOURCE scope") {
			t.Fatalf("%q was refused by another rule, so the layer check is doing "+
				"no work for it: %v", k, err)
		}
	}
}

func assertNoCanary(t *testing.T, res map[string]string, rec map[string]any) {
	t.Helper()
	for k, v := range res {
		if strings.Contains(k, canary) || strings.Contains(v, canary) {
			t.Fatalf("resource leaked the canary: %q = %q", k, v)
		}
	}
	for k, v := range rec {
		if strings.Contains(k, canary) || strings.Contains(fmt.Sprint(v), canary) {
			t.Fatalf("record leaked the canary: %q = %v", k, v)
		}
	}
}
