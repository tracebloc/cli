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
		"Please name the dataset.":      "t",
		"Which task?":                   "tabular_classification",
		"Which column holds the label?": "churned",
	}}
	a := &runDataIngestArgs{
		LocalPath:      dir,
		TargetSizeFlag: "224x224",
		MinSizeFlag:    "32x32",
		Spec: push.SpecArgs{
			Category:          "time_to_event_prediction",
			TimeColumn:        "t",
			LabelPolicy:       "bucket",
			NumberOfKeypoints: 17,
		},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	for _, c := range []struct {
		name string
		got  string
	}{
		{"TimeColumn", a.Spec.TimeColumn},
		{"LabelPolicy", a.Spec.LabelPolicy},
		{"TargetSizeFlag", a.TargetSizeFlag},
		{"MinSizeFlag", a.MinSizeFlag},
	} {
		if c.got != "" {
			t.Errorf("%s = %q survived the task change to tabular_classification", c.name, c.got)
		}
	}
	if a.Spec.NumberOfKeypoints != 0 {
		t.Errorf("NumberOfKeypoints = %d survived the task change", a.Spec.NumberOfKeypoints)
	}
	// The reset is not a blanket wipe: the label column IS in scope for
	// tabular_classification, and the answer just given must stand.
	if a.Spec.LabelColumn != "churned" {
		t.Errorf("LabelColumn = %q, want the answer just given", a.Spec.LabelColumn)
	}
}

// The reset must be a no-op when the picked task is the supplied one — the
// test above cannot tell "cleared what went out of scope" from "cleared
// everything", and clearing everything would silently discard flags the user
// meant and the prompts pre-fill from.
func TestGuided_TaskScopedValuesSurviveWhenTheTaskIsUnchanged(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Please name the dataset.":                 "t",
		"Which task?":                              "time_to_event_prediction",
		"Which column holds the value to predict?": "days",
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

// The clearer and the guard read the same predicate, which is the point of the
// table — but "same predicate" is only worth something if clearing actually
// satisfies the guard. Walk every row against every supported task: after the
// reset, nothing may be rejected.
func TestClearingAlwaysSatisfiesTheGuard(t *testing.T) {
	for _, cat := range push.SupportedCategoryIDs() {
		t.Run(cat, func(t *testing.T) {
			a := &runDataIngestArgs{
				TargetSizeFlag: "224x224",
				MinSizeFlag:    "32x32",
				SchemaFlag:     "age:INT",
				Spec: push.SpecArgs{
					Category:          cat,
					LabelColumn:       "label",
					LabelPolicy:       "bucket",
					TimeColumn:        "t",
					NumberOfKeypoints: 17,
				},
			}
			if err := rejectMisappliedTaskValues(a); err == nil {
				t.Fatalf("everything set on %s was accepted — the guard is inert", cat)
			}
			dropOutOfScopeTaskValues(a)
			if err := rejectMisappliedTaskValues(a); err != nil {
				t.Errorf("after the reset the guard still rejects: %v", err)
			}
		})
	}
}
