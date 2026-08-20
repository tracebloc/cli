package telemetry

import "time"

// Command outcomes — backend#1907, RFC-BACKEND-1872 D12's host-process path.
//
// WHAT THIS IS FOR. The CLI is the least-observed surface in the product and
// the one that runs on the most different machines. Every failure in the
// backend#736 class — the binary landing on a PATH the shell does not read, a
// cluster command reading a kubeconfig context nobody meant, a package manager
// blocked on a lock held by something else — was invisible until a customer
// happened to mention it. One outcome event per invocation is what turns that
// class into a number.
//
// THE PRIVACY BOUNDARY IS STRUCTURAL, NOT EDITORIAL. The ticket's rule is "no
// arguments, no paths, no data", and a rule phrased that way is a convention
// that asks. What is built here instead is a record with no free-text channel
// at all:
//
//   - the command is a LOOKUP into the set of paths enumerated from the live
//     cobra tree at startup; anything else reports CommandUnregistered;
//   - the error class is a lookup keyed on an INT — the CLI's own frozen
//     exit-code contract — so the classifier cannot see an error message, a
//     path or an argument, because it is never given one;
//   - everything else is an int.
//
// A sanitiser would have to anticipate what it strips. A closed set only ever
// admits what was enumerated. That difference is why there is no redaction
// regex anywhere in this file.

// The three terminal event names. Compile-time constants per contract §6.2 —
// no segment is ever computed. There is deliberately no cli.command.started:
// §6.5 requires a terminal event on every path where a `started` is emitted,
// and a process that is killed outright cannot honour that.
const (
	EventCommandSucceeded = "cli.command.succeeded"
	EventCommandFailed    = "cli.command.failed"
	EventCommandCancelled = "cli.command.cancelled"
)

// Record-scope attribute keys. tracebloc.cli.command is the contract's own name
// for this field (§7.1: "CLI command path — e.g. `data ingest`, never the
// arguments").
const (
	AttrCommand    = "tracebloc.cli.command"
	AttrExitCode   = "tracebloc.cli.exit_code"
	AttrDurationMS = "tracebloc.cli.duration_ms"
)

// CommandUnregistered is what a path outside the registered set reports.
//
// It is the fail-closed answer, and it is reported as a VALUE rather than by
// dropping the attribute so that "we saw a command we could not name" stays
// countable. A recorder built with no registered commands at all therefore
// reports this for everything — forgetting to register cannot silently turn the
// lookup into a pass-through.
const CommandUnregistered = "unregistered"

// ExitCancelled is 128+SIGINT — the user pressed Ctrl-C. It is not a failure and
// must not inflate one: a cancel that counted as a failure would move the rate
// D9's alerts are written against every time someone changed their mind.
const ExitCancelled = 130

// The closed error.type vocabulary for the `cli` domain (contract §8.4; the
// spec's open question 1 says each emitter ticket proposes its own).
//
// DERIVED FROM THE EXIT CODES, NOT FROM THE ERROR TEXT. internal/cli/exitcodes.go
// already carries a reviewed, documented, FROZEN classification of every way the
// CLI can fail — it is the scripting contract customers branch on. Classifying
// from anywhere else would mean inventing a second taxonomy that drifts from the
// first, and would mean handing the classifier an error string, which is exactly
// the channel a path or a cell value travels down.
const (
	ClassUnspecifiedFailure  = "unspecified_failure"
	ClassInvalidInput        = "invalid_input"
	ClassLocalEnvironment    = "local_environment"
	ClassNoSecureEnvironment = "no_secure_environment"
	ClassAuth                = "auth"
	ClassConflict            = "conflict"
	ClassClusterOperation    = "cluster_operation"
	ClassSubmitRejected      = "submit_rejected"
	ClassIngestFailed        = "ingest_failed"
	ClassUnclassified        = "unclassified"
)

// exitClasses maps the CLI's exit codes to that vocabulary. Several codes carry
// more than one per-command meaning (exitChecksFailed shares 2 with
// exitBadInput, exitNoSuchDataset shares 5 with exitAuth, and 7 is three
// meanings) — the class names the shared bucket, because the code is what a
// customer's script sees and grouping finer than the contract would be a
// distinction nothing downstream can act on. tracebloc.cli.command separates
// them when it matters.
var exitClasses = map[int]string{
	1: ClassUnspecifiedFailure,
	2: ClassInvalidInput,
	3: ClassLocalEnvironment,
	4: ClassNoSecureEnvironment,
	5: ClassAuth,
	6: ClassConflict,
	7: ClassClusterOperation,
	8: ClassSubmitRejected,
	9: ClassIngestFailed,
}

// ClassifyExit maps an exit code to a member of the closed vocabulary.
//
// Total by construction: every int has an answer, and an unmapped one is
// ClassUnclassified rather than the code rendered as a string. That is the
// fail-closed half — a code this table has not seen is a finding you can alert
// on, not a new namespace that appears on its own.
func ClassifyExit(code int) string {
	if class, ok := exitClasses[code]; ok {
		return class
	}
	return ClassUnclassified
}

// Sink is the delivery seam. It is a named type so the wiring in internal/cli
// can say what it is handing over; Emitter.SetSink takes the same shape.
type Sink func(resource map[string]string, record map[string]any)

// Outcome is one invocation, as measured by the caller.
type Outcome struct {
	// Command is the cobra command path minus the arguments — "data ingest".
	// It is not trusted: Record looks it up in the registered set and reports
	// CommandUnregistered if it is not there.
	Command string
	// ExitCode is what the process is about to exit with.
	ExitCode int
	// Elapsed is wall-clock time for the invocation.
	Elapsed time.Duration
}

// OutcomeRecorder turns an Outcome into exactly one contract-conformant event.
type OutcomeRecorder struct {
	emitter  *Emitter
	commands map[string]bool
}

// NewOutcomeRecorder closes the tracebloc.cli.command value set over commands.
//
// The caller passes the paths enumerated from the live command tree, so the set
// is derived from the thing that actually dispatches rather than restated here.
// A command added to the tree is covered without touching this file; a value
// that is not a command in the tree cannot be emitted at all.
func NewOutcomeRecorder(e *Emitter, commands []string) *OutcomeRecorder {
	set := make(map[string]bool, len(commands))
	for _, c := range commands {
		if c != "" {
			set[c] = true
		}
	}
	return &OutcomeRecorder{emitter: e, commands: set}
}

// Record emits the invocation's single terminal event.
func (r *OutcomeRecorder) Record(o Outcome) error {
	name := EventCommandSucceeded
	switch {
	case o.ExitCode == 0:
	case o.ExitCode == ExitCancelled:
		name = EventCommandCancelled
	default:
		name = EventCommandFailed
	}

	attrs := Attrs{
		AttrCommand:    r.commandValue(o.Command),
		AttrExitCode:   o.ExitCode,
		AttrDurationMS: durationMS(o.Elapsed),
	}
	// §8.4 — only the failure outcomes oblige the error set. Emit enforces this
	// too; setting it here for a cancel would be a second, disagreeing opinion
	// about what counts as a failure.
	if name == EventCommandFailed {
		attrs["error.type"] = ClassifyExit(o.ExitCode)
	}
	return r.emitter.Emit(name, attrs)
}

// commandValue is the privacy boundary: a set membership test, not a cleanup
// pass. Whatever the caller hands over either IS one of the paths the tree can
// dispatch, or it does not reach the record in any form.
func (r *OutcomeRecorder) commandValue(path string) string {
	if r.commands[path] {
		return path
	}
	return CommandUnregistered
}

// durationMS clamps below zero. A wall-clock delta can go negative across a
// clock step, and a negative duration is not a measurement — it is a number
// that would quietly skew every percentile computed over the column.
func durationMS(d time.Duration) int64 {
	if ms := Duration(d); ms > 0 {
		return ms
	}
	return 0
}
