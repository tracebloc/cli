package cli

import (
	"errors"
	"fmt"

	"github.com/tracebloc/cli/internal/push"
)

// taskScopedValue is one value that only some tasks use — the flags each read
// inside a single category branch, so a value passed against a task that
// doesn't consume it would otherwise be silently dropped.
//
// The scope predicate lives HERE, once, because two callers need the same
// answer to "does this task use this value?":
//
//   - the misapplied-flag guard in runDataIngestLocal, which rejects a value
//     the chosen task cannot use, and
//   - dropOutOfScopeTaskValues, which the guided flow runs after the task
//     picker so a value the user is no longer choosing doesn't survive.
//
// Written as two copies of each predicate, they would agree today and drift on
// the next task added to a family — and drift here is invisible: both copies
// keep passing their own tests while disagreeing with each other.
type taskScopedValue struct {
	// flag names the value as the user typed it, for the message.
	flag string
	// inScope answers whether this task consumes the value.
	inScope func(category string) bool
	// isSet reports whether the value is present at all.
	isSet func(*runDataIngestArgs) bool
	// set puts a representative value there. Only tests call it, and they call
	// it to build states from the table rather than by hand — a hand-built
	// fixture is one more copy of the scopes to drift.
	set func(*runDataIngestArgs)
	// clear returns the value to "not supplied".
	clear func(*runDataIngestArgs)
	// message is the whole rejection sentence, written out rather than
	// composed, so the copy catalog (zz-all-strings.golden) shows a reviewer
	// the exact string a user sees instead of a fragment. A test pins that each
	// one names its own flag.
	message func(category string) string
}

// taskScopedValues is the whole set. Order fixes the order of rejection when
// more than one is misapplied, so the message a user sees is deterministic.
var taskScopedValues = []taskScopedValue{
	{
		flag:    "--target-size",
		inScope: push.IsImage,
		isSet:   func(a *runDataIngestArgs) bool { return a.TargetSizeFlag != "" },
		clear:   func(a *runDataIngestArgs) { a.TargetSizeFlag = "" },
		set:     func(a *runDataIngestArgs) { a.TargetSizeFlag = "224x224" },
		message: func(cat string) string {
			return fmt.Sprintf("--target-size is image tasks only; it doesn't apply to task %q", cat)
		},
	},
	{
		flag:    "--min-size",
		inScope: push.IsImage,
		isSet:   func(a *runDataIngestArgs) bool { return a.MinSizeFlag != "" },
		clear:   func(a *runDataIngestArgs) { a.MinSizeFlag = "" },
		set:     func(a *runDataIngestArgs) { a.MinSizeFlag = "32x32" },
		message: func(cat string) string {
			return fmt.Sprintf("--min-size is image tasks only; it doesn't apply to task %q", cat)
		},
	},
	{
		flag:    "--schema",
		inScope: push.IsTabular,
		isSet:   func(a *runDataIngestArgs) bool { return a.SchemaFlag != "" },
		clear:   func(a *runDataIngestArgs) { a.SchemaFlag = "" },
		set:     func(a *runDataIngestArgs) { a.SchemaFlag = "age:INT" },
		message: func(cat string) string {
			return fmt.Sprintf("--schema is tabular/time-series tasks only; it doesn't apply to task %q", cat)
		},
	},
	{
		flag:    "--label-policy",
		inScope: push.IsRegressionClass,
		isSet:   func(a *runDataIngestArgs) bool { return a.Spec.LabelPolicy != "" },
		clear:   func(a *runDataIngestArgs) { a.Spec.LabelPolicy = "" },
		set:     func(a *runDataIngestArgs) { a.Spec.LabelPolicy = "bucket" },
		message: func(cat string) string {
			return fmt.Sprintf("--label-policy is regression-class tasks only (tabular_regression, "+
				"time_series_forecasting, time_to_event_prediction); it doesn't apply to task %q", cat)
		},
	},
	{
		flag:    "--time-column",
		inScope: func(cat string) bool { return cat == "time_to_event_prediction" },
		isSet:   func(a *runDataIngestArgs) bool { return a.Spec.TimeColumn != "" },
		clear:   func(a *runDataIngestArgs) { a.Spec.TimeColumn = "" },
		set:     func(a *runDataIngestArgs) { a.Spec.TimeColumn = "t" },
		message: func(cat string) string {
			return fmt.Sprintf("--time-column is time_to_event_prediction only; it doesn't apply to task %q", cat)
		},
	},
	{
		flag:    "--number-of-keypoints",
		inScope: func(cat string) bool { return cat == "keypoint_detection" },
		isSet:   func(a *runDataIngestArgs) bool { return a.Spec.NumberOfKeypoints != 0 },
		clear:   func(a *runDataIngestArgs) { a.Spec.NumberOfKeypoints = 0 },
		set:     func(a *runDataIngestArgs) { a.Spec.NumberOfKeypoints = 17 },
		message: func(cat string) string {
			return fmt.Sprintf("--number-of-keypoints is keypoint_detection only; it doesn't apply to task %q", cat)
		},
	},
	{
		// The one inverted scope: every task uses a label column EXCEPT
		// self-supervised text, which trains on the text itself. buildText drops
		// the value, so accepting it silently discarded the user's answer and
		// the review echoed a column that never shipped.
		flag:    "--label-column",
		inScope: func(cat string) bool { return !push.SelfSupervisedText(cat) },
		isSet:   func(a *runDataIngestArgs) bool { return a.Spec.LabelColumn != "" },
		clear:   func(a *runDataIngestArgs) { a.Spec.LabelColumn = "" },
		set:     func(a *runDataIngestArgs) { a.Spec.LabelColumn = "label" },
		message: func(cat string) string {
			return fmt.Sprintf("--label-column doesn't apply to task %q — it trains on the text itself, with no label column", cat)
		},
	},
}

// rejectMisappliedTaskValues returns the first value present that the chosen
// task cannot use. Flag-only runs reach this with whatever the user typed;
// guided runs reach it after dropOutOfScopeTaskValues has already removed
// anything the user re-chose away from, so it can only fire on a real mistake.
func rejectMisappliedTaskValues(a *runDataIngestArgs) error {
	for _, v := range taskScopedValues {
		if v.isSet(a) && !v.inScope(a.Spec.Category) {
			return errors.New(v.message(a.Spec.Category))
		}
	}
	return nil
}

// dropValuesLeftBehindByATaskChange clears the task-scoped values that the
// task the user just CHOSE does not use, but the task they arrived with did.
//
// The guided flow calls this immediately after the picker. Without it, a run
// started as `--task time_to_event_prediction --time-column t` that picks
// tabular_classification keeps TimeColumn: the prompt for it never appears (it
// is time_to_event_prediction-only), Review shows it anyway, and the run dies
// AFTER the confirm blaming a flag the user just spent a prompt walking away
// from. Guided mode's promise is that the answers on screen are the run; a
// value no question asked about and no answer can reach is not one of them.
//
// It is scoped to what the CHANGE left behind — `v.inScope(from)` — and not to
// everything out of scope, because those are different sets and clearing the
// wrong one swallows a real mistake. `--task tabular_classification
// --time-column t` is misapplied on the command line: no task change walks away
// from it, so nothing here may clear it and rejectMisappliedTaskValues must
// still fire. Otherwise pressing Enter on the pre-selected task would silently
// drop the flag while the identical invocation under --no-input exits 2 —
// guided mode quietly meaning something different from the flags it echoes
// (Bugbot).
//
// A run with no --task supplied is not a change either: the user never declared
// a task to move away from, so every value stands or falls on the guard.
//
// Nor is a --task the registry does not recognize. A typo is not a task anyone
// walked away from, and treating it as one clears values on its behalf: every
// `inScope` predicate answers from the registry, so an unknown id lands on the
// default side of each one — `!SelfSupervisedText("tabular_clasification")` is
// true because the lookup misses, not because the task uses a label column. So
// `--task tabular_clasification --label-column x`, picking a self-supervised
// text task, silently dropped --label-column, and the guided flow runs BEFORE
// the category gate (data_ingest_local.go:103 vs :165), so the typo itself was
// never reported either — the picker had already overwritten it with a valid id.
// Two values lost, no message, where --no-input exits 2 on the same command.
// An unknown `from` is therefore the no-task case: clear nothing, and let
// rejectMisappliedTaskValues speak (Bugbot).
func dropValuesLeftBehindByATaskChange(a *runDataIngestArgs, from string) {
	if from == "" || !push.IsKnown(from) || from == a.Spec.Category {
		return
	}
	for _, v := range taskScopedValues {
		if v.inScope(from) && !v.inScope(a.Spec.Category) {
			v.clear(a)
		}
	}
}
