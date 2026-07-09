package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// fakePrompter is the test double for the prompter seam: it returns
// scripted answers keyed by prompt label and records the order of
// labels asked, so tests can assert WHICH fields were prompted (and in
// what order) and how answers map onto SpecArgs — with no real terminal
// involved.
type fakePrompter struct {
	answers map[string]string
	asked   []string
	confirm *bool // nil → return the prompt's default
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

// tabularDir drops a directory holding a single CSV with a known header,
// so the family sniff reads "tabular" and the label picker can offer real
// columns.
func tabularDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data.csv"),
		[]byte("age,income,churned\n42,50000,yes\n"), 0o644); err != nil {
		t.Fatalf("write data.csv: %v", err)
	}
	return root
}

// imageDirLayout drops labels.csv + an images/ folder so the sniff reads
// "image".
func imageDirLayout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "labels.csv"),
		[]byte("image_id,label\n001.jpg,cat\n"), 0o644); err != nil {
		t.Fatalf("write labels.csv: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "images"), 0o755); err != nil {
		t.Fatalf("mkdir images: %v", err)
	}
	return root
}

// textDirLayout drops labels.csv + a texts/ folder so the sniff reads
// "text".
func textDirLayout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "labels.csv"),
		[]byte("text_id,label\n001.txt,spam\n"), 0o644); err != nil {
		t.Fatalf("write labels.csv: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "texts"), 0o755); err != nil {
		t.Fatalf("mkdir texts: %v", err)
	}
	return root
}

// TestRunInteractive_PromptOrder: a bare invocation prompts data-first —
// intent, then name, then path, then task — before any task-specific
// question. Pins the RFC-0002 §12.1 order.
func TestRunInteractive_PromptOrder(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Is this training or test data?":              "test",
		"What should we call this dataset?":           "churn_train",
		"Where is your data? (the folder holding it)": dir,
		"Which task?":                   "Tabular classification",
		"Which column holds the class?": "churned",
	}}
	a := &runDataIngestArgs{}
	if err := runInteractive(discardPrinter(), f, a, false /*taskSet*/); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}

	// The four core questions must appear in data-first order, ahead of the
	// label question.
	want := []string{
		"Is this training or test data?",
		"What should we call this dataset?",
		"Where is your data? (the folder holding it)",
		"Which task?",
		"Which column holds the class?",
	}
	if !orderedSubsequence(f.asked, want) {
		t.Errorf("prompt order = %v, want subsequence %v", f.asked, want)
	}
	if a.Spec.Intent != "test" || a.Spec.Table != "churn_train" ||
		a.LocalPath != dir || a.Spec.Category != "tabular_classification" ||
		a.Spec.LabelColumn != "churned" {
		t.Errorf("fields not mapped: %+v localPath=%q", a.Spec, a.LocalPath)
	}
}

// TestRunInteractive_SniffEchoesFamily: a confident layout is echoed back
// and the family question is NOT asked (the sniff is enough).
func TestRunInteractive_SniffEchoesFamily(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"What should we call this dataset?": "t",
		"Which column holds the class?":     "churned",
	}}
	a := &runDataIngestArgs{LocalPath: dir, Spec: push.SpecArgs{Intent: "train"}}
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	if err := runInteractive(p, f, a, false); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if !strings.Contains(buf.String(), "Found a CSV table") {
		t.Errorf("expected a tabular sniff echo, got:\n%s", buf.String())
	}
	for _, l := range f.asked {
		if l == "What kind of data is this?" {
			t.Errorf("a confident sniff must not ask the family question")
		}
	}
	if a.Spec.Category != "tabular_classification" {
		t.Errorf("Category = %q, want tabular_classification", a.Spec.Category)
	}
}

// TestRunInteractive_SniffIsHintNotLock: an ambiguous layout falls back to
// asking the family plainly, then scopes the picker to the answer.
func TestRunInteractive_SniffIsHintNotLock(t *testing.T) {
	empty := t.TempDir() // no csv, no images/, no texts/ → ambiguous
	f := &fakePrompter{answers: map[string]string{
		"What should we call this dataset?": "t",
		"What kind of data is this?":        "image",
		"Which task?":                       "Image classification",
		"Which column holds the class?":     "label",
	}}
	a := &runDataIngestArgs{LocalPath: empty, Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a, false); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if !contains(f.asked, "What kind of data is this?") {
		t.Errorf("ambiguous layout should ask the family plainly; asked=%v", f.asked)
	}
	if a.Spec.Category != "image_classification" {
		t.Errorf("Category = %q, want image_classification (family answer honored)", a.Spec.Category)
	}
}

// TestRunInteractive_ExplicitTaskSkipsSniff: an explicit --task wins — no
// sniff echo, no family question, no task picker.
func TestRunInteractive_ExplicitTaskSkipsSniff(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"What should we call this dataset?": "t",
		"Which column holds the class?":     "churned",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec:      push.SpecArgs{Category: "tabular_classification", Intent: "train"},
	}
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	if err := runInteractive(p, f, a, true /*taskSet*/); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	for _, l := range f.asked {
		if l == "Which task?" || l == "What kind of data is this?" {
			t.Errorf("explicit --task must skip the picker/sniff; asked %q", l)
		}
	}
	if strings.Contains(buf.String(), "Found a CSV table") {
		t.Errorf("explicit --task must not echo a sniff")
	}
}

// TestPickTask_FamilyScoped: the picker offers only the given family's
// tasks, wires the friendly display names + the locked glosses, and lists
// the not-yet-supported ones (greyed, with a reason) — never the other
// families' tasks.
func TestPickTask_FamilyScoped(t *testing.T) {
	// Text family: fill-mask (gloss) is available; seq2seq
	// (translation / summarization, gloss) + token_classification are
	// pending; image/tabular tasks must not appear.
	f := &fakePrompter{answers: map[string]string{"Which task?": "Text classification"}}
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	id, err := pickTask(p, f, push.FamilyText)
	if err != nil {
		t.Fatalf("pickTask: %v", err)
	}
	if id != "text_classification" {
		t.Errorf("id = %q, want text_classification", id)
	}
	out := buf.String()
	for _, want := range []string{
		"Tasks for text data",
		"fill-mask",                   // MLM gloss (available)
		"Text classification",         // label
		"Not yet in the CLI:",         // pending header
		"translation / summarization", // seq2seq gloss (pending)
		"token_classification",        // pending id
		"schema-recognized",           // an UnsupportedNote fragment
	} {
		if !strings.Contains(out, want) {
			t.Errorf("picker output missing %q:\n%s", want, out)
		}
	}
	// Other families must not leak in.
	for _, unwanted := range []string{"Image classification", "Tabular classification", "Survival analysis"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("text picker leaked a non-text task %q:\n%s", unwanted, out)
		}
	}
}

// TestPickTask_TabularGloss: the tabular picker shows the survival-analysis
// gloss for time_to_event_prediction and can select it back to its id.
func TestPickTask_TabularGloss(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{"Which task?": "Survival analysis"}}
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	id, err := pickTask(p, f, push.FamilyTabular)
	if err != nil {
		t.Fatalf("pickTask: %v", err)
	}
	if id != "time_to_event_prediction" {
		t.Errorf("id = %q, want time_to_event_prediction", id)
	}
	if !strings.Contains(buf.String(), "Survival analysis") {
		t.Errorf("tabular picker missing the survival-analysis gloss:\n%s", buf.String())
	}
}

// TestRunInteractive_LabelSelectFromHeaders: the label question is a SELECT
// over the real CSV header row, so the chosen column exact-matches one that
// exists (killing the case-mismatch silent-null-label bug).
func TestRunInteractive_LabelSelectFromHeaders(t *testing.T) {
	dir := tabularDir(t) // header: age,income,churned
	// Script an answer that only works if the options were the real headers.
	f := &fakePrompter{answers: map[string]string{
		"What should we call this dataset?": "t",
		"Which task?":                       "Tabular classification",
		"Which column holds the class?":     "income",
	}}
	a := &runDataIngestArgs{LocalPath: dir, Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a, false); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.LabelColumn != "income" {
		t.Errorf("LabelColumn = %q, want income", a.Spec.LabelColumn)
	}
}

// TestRunInteractive_RegressionLabelWording: a regression-class task words
// the label question as the value to predict, not a class.
func TestRunInteractive_RegressionLabelWording(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Which column holds the value to predict?": "income",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec:      push.SpecArgs{Category: "tabular_regression", Table: "t", Intent: "train"},
	}
	if err := runInteractive(discardPrinter(), f, a, true /*taskSet*/); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if !contains(f.asked, "Which column holds the value to predict?") {
		t.Errorf("regression should ask for the value to predict; asked=%v", f.asked)
	}
	if contains(f.asked, "Which column holds the class?") {
		t.Errorf("regression must not use the class wording")
	}
	if a.Spec.LabelColumn != "income" {
		t.Errorf("LabelColumn = %q, want income", a.Spec.LabelColumn)
	}
}

// TestRunInteractive_LabelFreeTextFallback: when the header can't be read
// (no CSV where the label would live), the label question falls back to
// free text rather than stalling.
func TestRunInteractive_LabelFreeTextFallback(t *testing.T) {
	empty := t.TempDir() // no labels.csv → PreviewLabelHeaders errors
	f := &fakePrompter{answers: map[string]string{
		"Which column holds the class?": "my_label",
	}}
	a := &runDataIngestArgs{
		LocalPath: empty,
		Spec:      push.SpecArgs{Category: "image_classification", Table: "t", Intent: "train"},
	}
	if err := runInteractive(discardPrinter(), f, a, true); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.LabelColumn != "my_label" {
		t.Errorf("LabelColumn = %q, want my_label (free-text fallback)", a.Spec.LabelColumn)
	}
}

// TestRunInteractive_MLMSkipsLabel: masked_language_modeling is
// self-supervised — the label question must not be asked.
func TestRunInteractive_MLMSkipsLabel(t *testing.T) {
	dir := textDirLayout(t)
	f := &fakePrompter{answers: map[string]string{
		"What should we call this dataset?": "mlm_train",
		"Which task?":                       "fill-mask",
	}}
	a := &runDataIngestArgs{LocalPath: dir, Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a, false); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	for _, l := range f.asked {
		if strings.HasPrefix(l, "Which column holds") {
			t.Errorf("masked_language_modeling should not ask for a label column")
		}
	}
	if a.Spec.Category != "masked_language_modeling" {
		t.Errorf("Category = %q, want masked_language_modeling", a.Spec.Category)
	}
}

// TestRunInteractive_SkipsProvidedValues: flags already set (and an
// explicit --task) mean nothing is prompted.
func TestRunInteractive_SkipsProvidedValues(t *testing.T) {
	dir := textDirLayout(t)
	f := &fakePrompter{answers: map[string]string{}}
	a := &runDataIngestArgs{
		LocalPath: dir,
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

// TestRunInteractive_Keypoint prompts for the required keypoint count; the
// optional resolution left blank means auto-detect.
func TestRunInteractive_Keypoint(t *testing.T) {
	dir := imageDirLayout(t)
	f := &fakePrompter{answers: map[string]string{
		"Number of keypoints per sample": "17",
		"Which column holds the class?":  "image_label",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec:      push.SpecArgs{Category: "keypoint_detection", Table: "kp_train", Intent: "train"},
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
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Label policy": "passthrough",
		"Which column holds the value to predict?": "income",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec:      push.SpecArgs{Category: "tabular_regression", Table: "reg_train", Intent: "train"},
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
	dir := tabularDir(t)
	no := false
	f := &fakePrompter{
		answers: map[string]string{
			"What should we call this dataset?": "t",
			"Which column holds the class?":     "churned",
		},
		confirm: &no,
	}
	a := &runDataIngestArgs{LocalPath: dir, Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a, false); !errors.Is(err, errInteractiveCancelled) {
		t.Fatalf("err = %v, want errInteractiveCancelled", err)
	}
}

// TestRunInteractive_RejectsBadName: the name prompt runs
// push.ValidateTableName, so an unsafe name surfaces as an error.
func TestRunInteractive_RejectsBadName(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{"What should we call this dataset?": "../bad"}}
	a := &runDataIngestArgs{Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a, true); err == nil {
		t.Fatal("expected an error for an invalid name, got nil")
	}
}

// TestRunInteractive_RejectsEmptyPath: a bare Enter at the path prompt is
// rejected by the validator rather than sniffing the current working
// directory (empty path → Abs("") → cwd).
func TestRunInteractive_RejectsEmptyPath(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{
		"What should we call this dataset?":           "t",
		"Where is your data? (the folder holding it)": "   ",
	}}
	a := &runDataIngestArgs{Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a, false); err == nil {
		t.Fatal("expected an error for an empty dataset path, got nil")
	}
}

// TestRunInteractive_TrimsPath: a path answer with surrounding whitespace
// (a common paste artifact) is trimmed before it's stored, so expandHome
// and the family sniff read the real path rather than a cwd-prefixed
// mangle. Without the trim, " <dir>" defeats expandHome and the sniff
// would land in the wrong place.
func TestRunInteractive_TrimsPath(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"What should we call this dataset?":           "t",
		"Where is your data? (the folder holding it)": "  " + dir + "  ",
		"Which column holds the class?":               "churned",
	}}
	a := &runDataIngestArgs{Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a, false); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.LocalPath != dir {
		t.Errorf("LocalPath = %q, want %q (surrounding whitespace not trimmed)", a.LocalPath, dir)
	}
	// The trimmed path must have sniffed cleanly as tabular (not landed in a
	// cwd-prefixed nonexistent dir that would force the family question).
	if a.Spec.Category != "tabular_classification" {
		t.Errorf("Category = %q, want tabular_classification (sniff read the trimmed path)", a.Spec.Category)
	}
}

// TestRunInteractive_ShowsExampleHints: the name and path prompts carry a
// visible example, so the guided flow teaches as it goes.
func TestRunInteractive_ShowsExampleHints(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"What should we call this dataset?": "churn_train",
		"Which column holds the class?":     "churned",
	}}
	a := &runDataIngestArgs{LocalPath: dir, Spec: push.SpecArgs{Intent: "train"}}
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	if err := runInteractive(p, f, a, false); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	for _, want := range []string{"e.g. churn_train", "age:INT"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("interactive output missing hint %q:\n%s", want, buf.String())
		}
	}
}

// --- small assertion helpers -------------------------------------------

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// orderedSubsequence reports whether want appears in got in order (not
// necessarily contiguously).
func orderedSubsequence(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}
