package cli

import (
	"bytes"
	"errors"
	"strings"
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
	confirm *bool // nil → return the prompt's default (true)
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

func (f *fakePrompter) Confirm(_ string, def bool) (bool, error) {
	if f.confirm != nil {
		return *f.confirm, nil
	}
	return def, nil
}

func discardPrinter() *ui.Printer { return ui.New(&bytes.Buffer{}) }

// TestRunInteractive_FillsAllWhenEmpty: a bare invocation prompts for
// every core field and maps the answers onto SpecArgs.
func TestRunInteractive_FillsAllWhenEmpty(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{
		"Path to your dataset directory": "./data",
		"Task":                           "tabular_classification",
		"Dataset name":                   "churn_train",
		"Is this training or test data?": "test",
		"Label column":                   "churned",
	}}
	a := &runDataIngestArgs{Spec: push.SpecArgs{Category: "image_classification"}}

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

// TestRunInteractive_PickerSeedsFirstTaskWhenUnset: with the task unset
// (taskSet=false) and no category on a, the picker must seed a valid
// option as its default. Dropping --task's old image_classification
// default (#180a) left an empty seed, which real survey.Select rejects
// ("default value \"\" not found in options"), crashing the headline
// "omit --task to pick interactively" flow. Omitting the "Task" answer
// makes the fake return the seeded default verbatim, so asserting the
// category landed on promptCategories[0] locks the seed in — a reseed
// to "" would fail this even though the fake itself doesn't validate.
func TestRunInteractive_PickerSeedsFirstTaskWhenUnset(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{
		"Dataset name": "churn_train",
	}}
	a := &runDataIngestArgs{LocalPath: "./data"} // Category deliberately left ""

	if err := runInteractive(discardPrinter(), f, a, false /*taskSet*/); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.Category != promptCategories[0] {
		t.Errorf("Category = %q, want the seeded first task %q",
			a.Spec.Category, promptCategories[0])
	}
}

// TestRunInteractive_ShowsExampleHints: each input prompt is preceded
// by a visible hint with an example, so the guided flow teaches as it
// goes. Drives runInteractive with a real (buffer-backed) Printer and
// asserts the example text lands in the output.
func TestRunInteractive_ShowsExampleHints(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{
		"Path to your dataset directory": "./d",
		"Dataset name":                   "churn_train",
	}}
	a := &runDataIngestArgs{Spec: push.SpecArgs{Category: "tabular_regression"}}

	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	if err := runInteractive(p, f, a, true /*taskSet*/); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"e.g. churn_train",   // table-name example
		"e.g. label, target", // label-column example
		"age:INT",            // tabular schema example
		"keeps raw values",   // label-policy explanation
	} {
		if !strings.Contains(out, want) {
			t.Errorf("interactive output missing hint %q:\n%s", want, out)
		}
	}
}

// TestRunInteractive_SkipsProvidedValues: flags already set (and an
// explicit --task) mean nothing is prompted.
func TestRunInteractive_SkipsProvidedValues(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{}}
	// text_classification has no task-specific prompts, so with all
	// core fields set + an explicit --task, nothing is asked.
	a := &runDataIngestArgs{
		LocalPath: "./data",
		Spec: push.SpecArgs{
			Category: "text_classification", Table: "t", Intent: "train", LabelColumn: "label",
		},
	}
	if err := runInteractive(discardPrinter(), f, a, true /*taskSet*/); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if len(f.asked) != 0 {
		t.Errorf("expected no prompts, but asked: %v", f.asked)
	}
}

// TestRunInteractive_Keypoint prompts for the required keypoint count;
// the optional resolution left blank means auto-detect.
func TestRunInteractive_Keypoint(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{"Number of keypoints per sample": "17"}}
	a := &runDataIngestArgs{
		LocalPath: "./kp",
		Spec:      push.SpecArgs{Category: "keypoint_detection", Table: "kp_train", Intent: "train", LabelColumn: "image_label"},
	}
	if err := runInteractive(discardPrinter(), f, a, true); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.NumberOfKeypoints != 17 {
		t.Errorf("NumberOfKeypoints = %d, want 17", a.Spec.NumberOfKeypoints)
	}
	if a.TargetSizeFlag != "" {
		t.Errorf("TargetSizeFlag = %q, want empty (auto-detect)", a.TargetSizeFlag)
	}
}

// TestRunInteractive_TabularRegression prompts for the label policy
// (regression-class) and leaves the schema to inference.
func TestRunInteractive_TabularRegression(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{"Label policy": "passthrough"}}
	a := &runDataIngestArgs{
		LocalPath: "./tab",
		Spec:      push.SpecArgs{Category: "tabular_regression", Table: "reg_train", Intent: "train", LabelColumn: "Target"},
	}
	if err := runInteractive(discardPrinter(), f, a, true); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.LabelPolicy != "passthrough" {
		t.Errorf("LabelPolicy = %q, want passthrough", a.Spec.LabelPolicy)
	}
	if a.SchemaFlag != "" {
		t.Errorf("SchemaFlag = %q, want empty (infer)", a.SchemaFlag)
	}
}

// TestRunInteractive_Cancel: declining the confirm returns the
// cancellation sentinel — a clean abort, not a failure.
func TestRunInteractive_Cancel(t *testing.T) {
	no := false
	f := &fakePrompter{
		answers: map[string]string{"Path to your dataset directory": "./x"},
		confirm: &no,
	}
	// path is prompted (→ prompted=true → a confirm is shown); the rest
	// is pre-set so we reach the confirm cleanly.
	a := &runDataIngestArgs{Spec: push.SpecArgs{
		Category: "image_classification", Table: "t", Intent: "train", LabelColumn: "label",
	}}
	if err := runInteractive(discardPrinter(), f, a, true); !errors.Is(err, errInteractiveCancelled) {
		t.Fatalf("err = %v, want errInteractiveCancelled", err)
	}
}

// TestRunInteractive_MLMSkipsLabel: masked_language_modeling has no
// label column, so it must not be prompted.
func TestRunInteractive_MLMSkipsLabel(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{
		"Dataset name":                   "mlm_train",
		"Is this training or test data?": "train",
	}}
	a := &runDataIngestArgs{
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
	f := &fakePrompter{answers: map[string]string{"Dataset name": "../bad"}}
	a := &runDataIngestArgs{
		LocalPath: "./data",
		Spec:      push.SpecArgs{Category: "image_classification", Intent: "train", LabelColumn: "label"},
	}
	if err := runInteractive(discardPrinter(), f, a, true); err == nil {
		t.Fatal("expected an error for an invalid table name, got nil")
	}
}
