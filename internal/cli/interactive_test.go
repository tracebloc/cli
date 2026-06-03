package cli

import (
	"bytes"
	"testing"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// fakePrompter is the test double for the prompter seam: it returns
// scripted answers keyed by prompt label and records the order of
// labels asked, so tests can assert WHICH fields were prompted and how
// answers map onto SpecArgs — with no real terminal involved.
type fakePrompter struct {
	answers map[string]string
	asked   []string
}

func (f *fakePrompter) answer(label, def string) string {
	f.asked = append(f.asked, label)
	if a, ok := f.answers[label]; ok {
		return a
	}
	return def
}

func (f *fakePrompter) Input(label, _ /*help*/, def string, validate func(string) error) (string, error) {
	ans := f.answer(label, def)
	if validate != nil {
		if err := validate(ans); err != nil {
			return "", err
		}
	}
	return ans, nil
}

func (f *fakePrompter) Select(label, _ /*help*/ string, _ []string, def string) (string, error) {
	return f.answer(label, def), nil
}

func discardPrinter() *ui.Printer { return ui.New(&bytes.Buffer{}) }

// TestRunInteractive_FillsAllWhenEmpty: a bare invocation prompts for
// every core field and maps the answers onto SpecArgs.
func TestRunInteractive_FillsAllWhenEmpty(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{
		"Path to your dataset directory": "./data",
		"Task category":                  "tabular_classification",
		"Destination table name":         "churn_train",
		"Intent":                         "test",
		"Label column":                   "churned",
	}}
	a := &runDatasetPushArgs{Spec: push.SpecArgs{Category: "image_classification"}}

	if err := runInteractive(discardPrinter(), f, a, false); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.LocalPath != "./data" {
		t.Errorf("LocalPath = %q, want ./data", a.LocalPath)
	}
	if a.Spec.Category != "tabular_classification" {
		t.Errorf("Category = %q, want tabular_classification", a.Spec.Category)
	}
	if a.Spec.Table != "churn_train" {
		t.Errorf("Table = %q, want churn_train", a.Spec.Table)
	}
	if a.Spec.Intent != "test" {
		t.Errorf("Intent = %q, want test", a.Spec.Intent)
	}
	if a.Spec.LabelColumn != "churned" {
		t.Errorf("LabelColumn = %q, want churned", a.Spec.LabelColumn)
	}
}

// TestRunInteractive_SkipsProvidedValues: flags already set (and an
// explicit --category) mean nothing is prompted.
func TestRunInteractive_SkipsProvidedValues(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{}}
	a := &runDatasetPushArgs{
		LocalPath: "./data",
		Spec: push.SpecArgs{
			Category: "image_classification", Table: "t", Intent: "train", LabelColumn: "label",
		},
	}
	if err := runInteractive(discardPrinter(), f, a, true /*categorySet*/); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if len(f.asked) != 0 {
		t.Errorf("expected no prompts, but asked: %v", f.asked)
	}
}

// TestRunInteractive_MLMSkipsLabel: masked_language_modeling has no
// label column, so it must not be prompted.
func TestRunInteractive_MLMSkipsLabel(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{
		"Destination table name": "mlm_train",
		"Intent":                 "train",
	}}
	a := &runDatasetPushArgs{
		LocalPath: "./data",
		Spec:      push.SpecArgs{Category: "masked_language_modeling"},
	}
	if err := runInteractive(discardPrinter(), f, a, true); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	for _, l := range f.asked {
		if l == "Label column" {
			t.Errorf("masked_language_modeling should not prompt for a label column")
		}
	}
	if a.Spec.Table != "mlm_train" || a.Spec.Intent != "train" {
		t.Errorf("table/intent not filled: %+v", a.Spec)
	}
}

// TestRunInteractive_RejectsBadTable: the table prompt runs
// push.ValidateTableName, so an unsafe name surfaces as an error.
func TestRunInteractive_RejectsBadTable(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{"Destination table name": "../bad"}}
	a := &runDatasetPushArgs{
		LocalPath: "./data",
		Spec:      push.SpecArgs{Category: "image_classification", Intent: "train", LabelColumn: "label"},
	}
	if err := runInteractive(discardPrinter(), f, a, true); err == nil {
		t.Fatal("expected an error for an invalid table name, got nil")
	}
}
