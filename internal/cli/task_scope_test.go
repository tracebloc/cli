package cli

import (
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/push"
)

// A run started with task-scoped flags that then picks a DIFFERENT task at the
// guided prompt must not carry the old task's values through. Before the reset,
// `--task time_to_event_prediction --time-column t` + picking
// tabular_classification kept TimeColumn on the spec: no prompt asks about it
// (it is time_to_event_prediction-only), Review showed it anyway, and the run
// then died AFTER the confirm blaming a flag the user had just walked away from.
func TestGuided_TaskScopedValuesDoNotSurviveATaskChange(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Name:":  "t",
		"Task:":  "tabular_classification",
		"Label:": "churned",
	}}
	// A LEGAL starting state: every value here is in scope for the supplied
	// task. Starting from an illegal one would test the wrong thing — a flag
	// misapplied from the outset is the guard's job, not the reset's.
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec: push.SpecArgs{
			Category:    "time_to_event_prediction",
			TimeColumn:  "t",
			LabelPolicy: "bucket",
			LabelColumn: "churned",
		},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.TimeColumn != "" {
		t.Errorf("TimeColumn = %q survived the change to tabular_classification", a.Spec.TimeColumn)
	}
	if a.Spec.LabelPolicy != "" {
		t.Errorf("LabelPolicy = %q survived the change", a.Spec.LabelPolicy)
	}
	// Not a blanket wipe: the label column IS in scope for the new task, and
	// the answer just given must stand.
	if a.Spec.LabelColumn != "churned" {
		t.Errorf("LabelColumn = %q, want the answer just given", a.Spec.LabelColumn)
	}
	if err := rejectMisappliedTaskValues(a); err != nil {
		t.Errorf("the state left behind is one the guard rejects: %v", err)
	}
}

// The same within the image family, where the value dropped is a number rather
// than a string and two neighbouring flags must survive.
func TestGuided_KeypointsGoWhenTheTaskStopsUsingThem(t *testing.T) {
	dir := imageDirLayout(t)
	f := &fakePrompter{answers: map[string]string{
		"Name:":  "t",
		"Task:":  "image_classification",
		"Label:": "label",
	}}
	a := &runDataIngestArgs{
		LocalPath:      dir,
		TargetSizeFlag: "224x224",
		MinSizeFlag:    "32x32",
		Spec: push.SpecArgs{
			Category:          "keypoint_detection",
			NumberOfKeypoints: 17,
			LabelColumn:       "label",
		},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.NumberOfKeypoints != 0 {
		t.Errorf("NumberOfKeypoints = %d survived the change to image_classification", a.Spec.NumberOfKeypoints)
	}
	if a.TargetSizeFlag == "" || a.MinSizeFlag == "" {
		t.Errorf("image flags were wiped though both tasks are image tasks: target=%q min=%q",
			a.TargetSizeFlag, a.MinSizeFlag)
	}
}

// The reset must be a no-op when the picked task is the supplied one — the
// tests above cannot tell "cleared what the change left behind" from "cleared
// everything", and clearing everything would silently discard flags the user
// meant and the prompts pre-fill from.
func TestGuided_TaskScopedValuesSurviveWhenTheTaskIsUnchanged(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Name:":   "t",
		"Task:":   "time_to_event_prediction",
		"Target:": "days",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec: push.SpecArgs{
			Category:    "time_to_event_prediction",
			TimeColumn:  "tenure_days",
			LabelPolicy: "passthrough",
		},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.TimeColumn != "tenure_days" {
		t.Errorf("TimeColumn = %q, want the supplied value to survive as the prompt default", a.Spec.TimeColumn)
	}
	if a.Spec.LabelPolicy != "passthrough" {
		t.Errorf("LabelPolicy = %q, want the supplied value to survive", a.Spec.LabelPolicy)
	}
}

// Every row's rejection must still fire for a task outside its scope, and must
// name its own flag. Without the name check a row could carry another row's
// message — the table makes that a copy-paste away, and the resulting error
// would send the user to a flag they never passed.
func TestEveryTaskScopedValueRejectsOutOfScopeAndNamesItsFlag(t *testing.T) {
	for _, v := range taskScopedValues {
		t.Run(v.flag, func(t *testing.T) {
			// Find a task this value does NOT apply to, from the real task list
			// rather than a hand-picked one, so a scope widened later still
			// finds its counterexample or fails loudly here.
			var out string
			for _, cat := range push.SupportedCategoryIDs() {
				if !v.inScope(cat) {
					out = cat
					break
				}
			}
			if out == "" {
				t.Fatalf("%s applies to every supported task — it is not task-scoped", v.flag)
			}
			msg := v.message(out)
			if !strings.Contains(msg, v.flag) {
				t.Errorf("message %q does not name its own flag %s", msg, v.flag)
			}
			if !strings.Contains(msg, out) {
				t.Errorf("message %q does not name the task it was rejected for", msg)
			}
		})
	}
}

// A flag misapplied on the COMMAND LINE must still be rejected in guided mode.
// The first version of the reset cleared everything out of scope for the picked
// task, which also cleared a flag that was wrong from the start — so pressing
// Enter on the pre-selected task silently dropped it while the identical
// invocation under --no-input exited 2. Guided mode quietly meaning something
// different from the flags it echoes is worse than the bug it replaced.
func TestGuided_AMisappliedFlagIsStillRejectedWhenTheTaskIsKept(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Name:":  "t",
		"Task:":  "tabular_classification",
		"Label:": "churned",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec:      push.SpecArgs{Category: "tabular_classification", TimeColumn: "t"},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.TimeColumn != "t" {
		t.Fatalf("TimeColumn = %q — the reset swallowed a flag no task change walked away from", a.Spec.TimeColumn)
	}
	if err := rejectMisappliedTaskValues(a); err == nil {
		t.Error("the misapplied --time-column was accepted; --no-input would exit 2 on the same command line")
	}
}

// Same, with no --task supplied at all: the user never declared a task to move
// away from, so nothing was walked away from and the guard still speaks.
func TestGuided_AMisappliedFlagIsStillRejectedWhenNoTaskWasSupplied(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Name:":  "t",
		"Task:":  "tabular_classification",
		"Label:": "churned",
	}}
	a := &runDataIngestArgs{LocalPath: dir, Spec: push.SpecArgs{TimeColumn: "t"}}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if err := rejectMisappliedTaskValues(a); err == nil {
		t.Error("a --time-column with no --task was silently dropped rather than rejected")
	}
}

// An UNRECOGNIZED --task is not a task the user walked away from, and this is the
// that made the reset silently eat two values at once. Every `inScope` predicate
// answers from the registry, so an unknown id lands on the DEFAULT side of each:
// `!SelfSupervisedText("tabular_classifier")` is true because the lookup
// misses. Pick a self-supervised text task and --label-column then reads as
// "in scope before, out of scope now" and gets cleared — so
// rejectMisappliedTaskValues finds nothing to complain about. The typo is gone
// too: the guided flow runs before the category gate
// (data_ingest_local.go:103 vs :165) and the picker has already overwritten it
// with a valid id, so the run proceeds as though the user typed neither flag,
// where --no-input exits 2 on the same command line.
func TestGuided_AnUnrecognizedTaskIsNotATaskChange(t *testing.T) {
	dir := textDirLayout(t)
	f := &fakePrompter{answers: map[string]string{
		"Name:": "mlm_train",
		"Task:": "masked_language_modeling",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		// A plausible wrong name (not tabular_classification): unknown to the
		// registry, so SelfSupervisedText() misses and every inScope() defaults
		// to true. Spelled correctly on purpose — `misspell` is a lint gate here.
		Spec: push.SpecArgs{Category: "tabular_classifier", LabelColumn: "churned"},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.LabelColumn != "churned" {
		t.Fatalf("LabelColumn = %q — the reset cleared it on behalf of a task that does not exist", a.Spec.LabelColumn)
	}
	if err := rejectMisappliedTaskValues(a); err == nil {
		t.Error("a --label-column misapplied alongside an unrecognized --task was accepted; --no-input would exit 2")
	}
}

// The edge Shujaat named on review: the user changes A -> B and the flag was
// invalid for BOTH. It is tempting to treat a re-pick as blanket permission to
// drop, but the flag was already wrong when they typed it and no answer they
// gave walks away from it — so it must still be rejected, exactly as it would
// be under --no-input. `v.inScope(from)` is what makes that true; a reset
// keyed only on the picked task would swallow it.
func TestGuided_AFlagInvalidForBothTasksIsStillRejected(t *testing.T) {
	dir := imageDirLayout(t)
	f := &fakePrompter{answers: map[string]string{
		"Name:":  "t",
		"Task:":  "image_classification",
		"Label:": "label",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec: push.SpecArgs{
			Category:    "keypoint_detection",
			LabelColumn: "label",
			// --time-column applies to neither keypoint_detection nor
			// image_classification.
			TimeColumn: "t",
		},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.TimeColumn != "t" {
		t.Fatalf("TimeColumn = %q — a re-pick is not permission to drop a flag that was always wrong", a.Spec.TimeColumn)
	}
	if err := rejectMisappliedTaskValues(a); err == nil {
		t.Error("the misapplied --time-column was accepted after a task change")
	}
}

// The property the shared predicate buys: from ANY legal starting state — the
// values in scope for the task the user arrived with — a change to ANY other
// task leaves a state the guard accepts. Both sides are built from the table,
// so a scope widened later is exercised here without editing this test.
func TestAnyLegalStateSurvivesAnyTaskChange(t *testing.T) {
	cats := push.SupportedCategoryIDs()
	for _, from := range cats {
		for _, to := range cats {
			if from == to {
				continue
			}
			t.Run(from+"->"+to, func(t *testing.T) {
				a := &runDataIngestArgs{Spec: push.SpecArgs{Category: from}}
				for _, v := range taskScopedValues {
					if v.inScope(from) {
						v.set(a)
					}
				}
				if err := rejectMisappliedTaskValues(a); err != nil {
					t.Fatalf("the starting state is not legal, so this proves nothing: %v", err)
				}
				a.Spec.Category = to
				dropValuesLeftBehindByATaskChange(a, from)
				if err := rejectMisappliedTaskValues(a); err != nil {
					t.Errorf("after the change the guard rejects: %v", err)
				}
			})
		}
	}
}
