package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

func (f *fakePrompter) Select(label, _ /*help*/ string, options []string, def string) (string, error) {
	// Honour survey.Select's real contract: a non-empty Default that is not one
	// of the Options aborts the prompt on a terminal ("default value … not found
	// in options"). The original double ignored Options entirely, so a Select
	// whose pre-filled default came from a mistyped flag stayed green here while
	// crashing on a real TTY (PR #505). Validating the default in the double is
	// what connects these tests to that failure — an unguarded supplied default
	// now reddens instead of passing.
	if def != "" && !slices.Contains(options, def) {
		return "", fmt.Errorf("default value %q not found in options %v", def, options)
	}
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
		[]byte("filename,label\n001.jpg,cat\n"), 0o644); err != nil {
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
		"Do you want to ingest training or test data?": "test",
		"Please name the dataset.":                     "churn_train",
		"Where is your data?":                          dir,
		"Which task?":                                  "tabular_classification",
		"Which column holds the label?":                "churned",
	}}
	a := &runDataIngestArgs{}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}

	// The four core questions must appear in data-first order, ahead of the
	// label question.
	want := []string{
		"Do you want to ingest training or test data?",
		"Please name the dataset.",
		"Where is your data?",
		"Which task?",
		"Which column holds the label?",
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

// TestRunInteractive_PathPromptCopyIsFileOrFolder pins the #181 copy
// restoration: now that the walk accepts a bare .csv, the path prompt says
// "file or folder" again (softened to folder-only in #180b).
func TestRunInteractive_PathPromptCopyIsFileOrFolder(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Do you want to ingest training or test data?": "train",
		"Please name the dataset.":                     "churn",
		"Where is your data?":                          dir,
		"Which task?":                                  "tabular_classification",
		"Which column holds the label?":                "churned",
	}}
	a := &runDataIngestArgs{}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	found := false
	for _, label := range f.asked {
		if label == "Where is your data?" {
			found = true
		}
		if strings.Contains(label, "the folder holding it") {
			t.Errorf("path prompt still uses the folder-only copy: %q", label)
		}
	}
	if !found {
		t.Errorf("path prompt label not asked; got %v", f.asked)
	}
}

// TestRunInteractive_SniffEchoesFamily: a confident layout is echoed back
// and the family question is NOT asked (the sniff is enough).
func TestRunInteractive_SniffEchoesFamily(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Please name the dataset.":      "t",
		"Which column holds the label?": "churned",
	}}
	a := &runDataIngestArgs{LocalPath: dir, Spec: push.SpecArgs{Intent: "train"}}
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	if err := runInteractive(p, f, a); err != nil {
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
		"Please name the dataset.":      "t",
		"What kind of data is this?":    "image",
		"Which task?":                   "image_classification",
		"Which column holds the label?": "label",
	}}
	a := &runDataIngestArgs{LocalPath: empty, Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if !contains(f.asked, "What kind of data is this?") {
		t.Errorf("ambiguous layout should ask the family plainly; asked=%v", f.asked)
	}
	if a.Spec.Category != "image_classification" {
		t.Errorf("Category = %q, want image_classification (family answer honored)", a.Spec.Category)
	}
}

// TestResolveFamily_SurfacesMiscasedHint pins the PR's headline behavior: an
// ambiguous sniff that carries an advisory hint (a mis-cased media folder next
// to labels.csv the walk can't see) has that hint surfaced through the printer
// before the family question — instead of silently ingesting the tree as a
// table (#203). The sniff tracks the walk, so behavior is FS-dependent; this
// asserts whichever branch applies on the machine it runs on. On a
// case-sensitive FS (Linux CI) the hint fires, so deleting resolveFamily's
// Warnf branch fails this test there.
func TestResolveFamily_SurfacesMiscasedHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "labels.csv"),
		[]byte("filename,label\n001.jpg,cat\n"), 0o644); err != nil {
		t.Fatalf("write labels.csv: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "Images"), 0o755); err != nil {
		t.Fatalf("mkdir Images: %v", err)
	}

	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	f := &fakePrompter{answers: map[string]string{"What kind of data is this?": "image"}}
	fam, err := resolveFamily(p, f, dir)
	if err != nil {
		t.Fatalf("resolveFamily: %v", err)
	}

	if push.SniffFamily(dir).Hint != "" {
		// Case-sensitive FS: the walk can't see Images/, so the sniff stays
		// ambiguous with a rename hint. resolveFamily must print it, and still
		// ask the family plainly (the hint is advisory, not a lock).
		if !strings.Contains(buf.String(), "fix it and ingest again") {
			t.Errorf("resolveFamily must surface the mis-cased rename hint; got:\n%s", buf.String())
		}
		if !contains(f.asked, "What kind of data is this?") {
			t.Errorf("hint is advisory — the family question must still be asked; asked=%v", f.asked)
		}
	} else {
		// Case-insensitive FS: the walk resolves Images/, so the sniff is
		// confident image — no false rename hint, no family question.
		if fam != push.FamilyImage {
			t.Errorf("family = %v, want image (walk resolves the mis-cased folder here)", fam)
		}
		if strings.Contains(buf.String(), "fix it and ingest again") {
			t.Errorf("no false rename hint when the walk sees the folder; got:\n%s", buf.String())
		}
		if contains(f.asked, "What kind of data is this?") {
			t.Errorf("a confident sniff must not ask the family question; asked=%v", f.asked)
		}
	}
}

// TestRunInteractive_ExplicitTaskStillAsks: an explicit --task no longer skips
// the picker (#711) — it PRE-SELECTS. It does still settle the family directly,
// so the sniff is unnecessary and the user's own task is among the options.
func TestRunInteractive_ExplicitTaskStillAsks(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Please name the dataset.":      "t",
		"Which column holds the label?": "churned",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec:      push.SpecArgs{Category: "tabular_classification", Intent: "train"},
	}
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	if err := runInteractive(p, f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if !slices.Contains(f.asked, "Which task?") {
		t.Errorf("the task question must still be asked; asked: %v", f.asked)
	}
	// The family came from the supplied task, so the layout sniff is not needed
	// and its echo must not appear.
	for _, l := range f.asked {
		if l == "What kind of data is this?" {
			t.Errorf("a supplied task settles the family; must not ask %q", l)
		}
	}
	if strings.Contains(buf.String(), "Found a CSV table") {
		t.Errorf("a supplied task must not echo a sniff")
	}
	// Unanswered by the fake, so it returns the prompt's default — proving the
	// supplied task was pre-selected rather than discarded.
	if a.Spec.Category != "tabular_classification" {
		t.Errorf("Category = %q, want the supplied task pre-selected", a.Spec.Category)
	}
}

// TestPickTask_FamilyScoped: the picker offers only the given family's
// tasks, wires the friendly display names + the locked glosses — never the
// other families' tasks. After RFC-0002 phase 4 every text task is wired, so
// the text picker has no "Not yet in the CLI" section at all.
func TestPickTask_FamilyScoped(t *testing.T) {
	// Text family: all tasks are available now — fill-mask (gloss),
	// classification, the two structured-pair tasks, and the two seq tasks;
	// image/tabular tasks must not appear.
	f := &fakePrompter{answers: map[string]string{"Which task?": "text_classification"}}
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	id, err := pickTask(p, f, push.FamilyText, "")
	if err != nil {
		t.Fatalf("pickTask: %v", err)
	}
	if id != "text_classification" {
		t.Errorf("id = %q, want text_classification", id)
	}
	out := buf.String()
	for _, want := range []string{
		"What kind of machine learning task is this data for?",
		"masked_language_modeling",     // MLM gloss (available)
		"text_classification",          // label
		"seq2seq",                      // seq2seq gloss (now available)
		"token_classification",         // now available
		"sentence_pair_classification", // now available
		"embeddings",                   // now available
	} {
		if !strings.Contains(out, want) {
			t.Errorf("picker output missing %q:\n%s", want, out)
		}
	}
	// Every text task is wired now — no pending section.
	if strings.Contains(out, "Not yet in the CLI:") {
		t.Errorf("text picker should have no pending section now:\n%s", out)
	}
	// Other families must not leak in.
	for _, unwanted := range []string{"image_classification", "tabular_classification", "time_to_event_prediction"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("text picker leaked a non-text task %q:\n%s", unwanted, out)
		}
	}
}

// TestPickTask_ImageAllAvailable: after #182 wired semantic_segmentation, every
// image task is available in the CLI, so the image picker lists them all under
// "Available now:" with no greyed "Not yet in the CLI" pending section.
func TestPickTask_ImageAllAvailable(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{"Which task?": "image_classification"}}
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	if _, err := pickTask(p, f, push.FamilyImage, ""); err != nil {
		t.Fatalf("pickTask: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"image_classification",
		"semantic_segmentation", // now selectable, no longer pending
	} {
		if !strings.Contains(out, want) {
			t.Errorf("image picker missing %q:\n%s", want, out)
		}
	}
	// No pending section and no stale backend#816 note now that semseg is wired.
	for _, unwanted := range []string{"Not yet in the CLI", "backend#816"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("image picker still shows pending content %q:\n%s", unwanted, out)
		}
	}
}

// TestPickTask_TabularGloss: the tabular picker shows the survival-analysis
// gloss for time_to_event_prediction and can select it back to its id.
func TestPickTask_TabularGloss(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{"Which task?": "time_to_event_prediction"}}
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	id, err := pickTask(p, f, push.FamilyTabular, "")
	if err != nil {
		t.Fatalf("pickTask: %v", err)
	}
	if id != "time_to_event_prediction" {
		t.Errorf("id = %q, want time_to_event_prediction", id)
	}
	if !strings.Contains(buf.String(), "time_to_event_prediction") {
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
		"Please name the dataset.":      "t",
		"Which task?":                   "tabular_classification",
		"Which column holds the label?": "income",
	}}
	a := &runDataIngestArgs{LocalPath: dir, Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
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
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if !contains(f.asked, "Which column holds the value to predict?") {
		t.Errorf("regression should ask for the value to predict; asked=%v", f.asked)
	}
	if contains(f.asked, "Which column holds the label?") {
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
		"Which column holds the label?": "my_label",
	}}
	a := &runDataIngestArgs{
		LocalPath: empty,
		Spec:      push.SpecArgs{Category: "image_classification", Table: "t", Intent: "train"},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
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
		"Please name the dataset.": "mlm_train",
		"Which task?":              "masked_language_modeling",
	}}
	a := &runDataIngestArgs{LocalPath: dir, Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
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

// TestRunInteractive_AsksEvenWhenFullySpecified is the inverse of the rule this
// replaced (#711). Guided mode used to prompt for nothing when every value
// arrived on the command line — which is what turned a path that had merely gone
// stale into a dead end, with the user sitting at a prompt that never came.
//
// Every core question must now be asked, and every supplied value must survive
// as the default: the fake answers nothing, so each field keeping its original
// value proves the pre-fill rather than a re-entered answer.
func TestRunInteractive_AsksEvenWhenFullySpecified(t *testing.T) {
	dir := textDirLayout(t)
	f := &fakePrompter{answers: map[string]string{}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec: push.SpecArgs{
			Category: "text_classification", Table: "t", Intent: "train", LabelColumn: "label",
		},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	for _, want := range []string{
		"Do you want to ingest training or test data?",
		"Please name the dataset.",
		"Where is your data?",
		"Which task?",
	} {
		if !slices.Contains(f.asked, want) {
			t.Errorf("guided mode must ask %q even when it was supplied; asked: %v", want, f.asked)
		}
	}
	if a.Spec.Intent != "train" || a.Spec.Table != "t" ||
		a.Spec.Category != "text_classification" || a.LocalPath != dir {
		t.Errorf("supplied values must survive as prompt defaults, got intent=%q table=%q category=%q path=%q",
			a.Spec.Intent, a.Spec.Table, a.Spec.Category, a.LocalPath)
	}
}

// With nothing supplied, each question must still fall back to the value it
// always defaulted to. This pins behaviourally what the string-index backstop
// used to pin textually: moving those defaults out of inline arguments and into
// variables (so a supplied value can take their place) removed "train",
// "bucket" and "time" from zz-all-strings.golden.
func TestRunInteractive_UnsuppliedDefaultsUnchanged(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Please name the dataset.":      "t",
		"Which task?":                   "time_to_event_prediction",
		"Which column holds the label?": "churned",
	}}
	a := &runDataIngestArgs{LocalPath: dir}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.Intent != "train" {
		t.Errorf("Intent = %q, want the unchanged \"train\" fallback", a.Spec.Intent)
	}
	if a.Spec.TimeColumn != "time" {
		t.Errorf("TimeColumn = %q, want the unchanged \"time\" fallback", a.Spec.TimeColumn)
	}
}

func TestRunInteractive_LabelPolicyDefaultUnchanged(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Please name the dataset.":      "t",
		"Which task?":                   "tabular_regression",
		"Which column holds the label?": "churned",
	}}
	a := &runDataIngestArgs{LocalPath: dir}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if a.Spec.LabelPolicy != "bucket" {
		t.Errorf("LabelPolicy = %q, want the unchanged \"bucket\" fallback", a.Spec.LabelPolicy)
	}
}

// A supplied value must be offered back as the DEFAULT, not silently replaced by
// the flow's own fallback — the specific regression that would make "always ask"
// feel like "always retype".
func TestRunInteractive_SuppliedValuesArePrefilled(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Which column holds the label?": "churned",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec: push.SpecArgs{
			Category: "tabular_regression", Table: "prefilled_name", Intent: "test",
		},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	// "test" must survive: the Select's own fallback is "train".
	if a.Spec.Intent != "test" {
		t.Errorf("Intent = %q, want the supplied \"test\" pre-selected (flow default is \"train\")", a.Spec.Intent)
	}
	if a.Spec.Table != "prefilled_name" {
		t.Errorf("Table = %q, want the supplied name pre-filled", a.Spec.Table)
	}
	if a.LocalPath != dir {
		t.Errorf("LocalPath = %q, want the supplied path pre-filled", a.LocalPath)
	}
}

// TestDefaultInOptions pins the guard that keeps a command-line value from
// crashing a survey.Select: a valid value passes through, an empty or unknown
// one drops to the fallback. Reverting the helper body to `return want` reddens
// the empty, typo and case-mismatch rows.
func TestDefaultInOptions(t *testing.T) {
	opts := []string{"train", "test"}
	cases := []struct {
		name, supplied, fallback, want string
	}{
		{"valid-supplied-passes-through", "test", "train", "test"},
		{"first-option-supplied", "train", "train", "train"},
		{"empty-uses-fallback", "", "train", "train"},
		{"unknown-typo-uses-fallback", "training", "train", "train"},
		{"case-mismatch-uses-fallback", "Train", "train", "train"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultInOptions(tc.supplied, opts, tc.fallback); got != tc.want {
				t.Errorf("defaultInOptions(%q, %v, %q) = %q, want %q",
					tc.supplied, opts, tc.fallback, got, tc.want)
			}
		})
	}
}

// TestRunInteractive_InvalidSuppliedSelectDefaultFallsBack: a mistyped --intent
// or --label-policy must not crash the guided prompt. survey.Select aborts when
// its Default is not one of the Options; guided mode pre-fills those Selects with
// the command-line value, so a typo like --intent training would open a prompt
// survey refuses to draw (#505, High). The value is guarded (defaultInOptions):
// an unknown supplied value drops back to the flow's own default, and the
// question is still ASKED.
//
// Mutation-proof: the strict fake Select errors on a default that isn't in its
// options, so reverting either guard makes runInteractive return that error and
// this test fails.
func TestRunInteractive_InvalidSuppliedSelectDefaultFallsBack(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Which column holds the value to predict?": "income",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec: push.SpecArgs{
			Category:    "tabular_regression",
			Table:       "reg_train",
			Intent:      "training", // typo — not train/test
			LabelPolicy: "buckets",  // typo — not bucket/passthrough
		},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("a mistyped supplied Select default must not crash the prompt: %v", err)
	}
	// Typo'd values were dropped to the flow's own valid defaults (the fake
	// returns the prompt default when nothing is scripted for that question).
	if a.Spec.Intent != "train" {
		t.Errorf("Intent = %q, want the \"train\" fallback after a typo'd --intent", a.Spec.Intent)
	}
	if a.Spec.LabelPolicy != "bucket" {
		t.Errorf("LabelPolicy = %q, want the \"bucket\" fallback after a typo'd --label-policy", a.Spec.LabelPolicy)
	}
}

// TestRunInteractive_SuppliedLabelColumnStillAsks: a supplied --label-column no
// longer SKIPS the label question (#505, Medium) — it PRE-FILLS it, exactly like
// a stale path. The old gate (LabelColumn == "") left a user who mistyped the
// column with no way to correct it at the prompt; now the header-backed picker
// still opens, so re-answering wins.
//
// Mutation-proof: restoring the `&& a.Spec.LabelColumn == ""` gate skips the
// question — f.asked loses it AND the re-answer no longer takes — so both
// assertions redden.
func TestRunInteractive_SuppliedLabelColumnStillAsks(t *testing.T) {
	dir := tabularDir(t) // header: age,income,churned
	f := &fakePrompter{answers: map[string]string{
		// Re-answer the label with a different real column: only reachable if the
		// question was actually asked rather than skipped by the supplied value.
		"Which column holds the label?": "churned",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec: push.SpecArgs{
			Category: "tabular_classification", Table: "t", Intent: "train",
			LabelColumn: "income", // supplied — must pre-fill, not skip
		},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if !contains(f.asked, "Which column holds the label?") {
		t.Errorf("a supplied --label-column must still ASK the label question; asked=%v", f.asked)
	}
	if a.Spec.LabelColumn != "churned" {
		t.Errorf("LabelColumn = %q, want the re-answered \"churned\" (prompt was live, not skipped)", a.Spec.LabelColumn)
	}
}

// TestPromptLabelColumn_SuppliedDefaultPrefillsAndGuards drives the label picker
// directly: a supplied column that names a real header is pre-selected, while an
// empty or mistyped one falls back to the header-derived default without crashing
// the Select. It is the same defaultInOptions guard used for --intent, applied to
// the column the label question now pre-fills (#505).
//
// Mutation-proof: passing `supplied` straight to pr.Select (dropping the guard)
// makes the "mistyped" row error under the strict fake; not threading the value
// at all makes the "valid-supplied" row return the header default instead.
func TestPromptLabelColumn_SuppliedDefaultPrefillsAndGuards(t *testing.T) {
	dir := tabularDir(t) // header: age,income,churned  (no column named "label")
	const cat = "tabular_classification"
	const q = "Which column holds the label?"

	cases := []struct {
		name, supplied, want string
	}{
		// Nothing scripted → the strict fake returns the Select's default, so the
		// asserted value IS whatever default promptLabelColumn passed in.
		{"valid-supplied-is-preselected", "income", "income"},
		{"empty-supplied-uses-header-default", "", "age"}, // defaultLabelChoice → first header
		{"mistyped-supplied-falls-back", "incom", "age"},  // guarded, no crash
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakePrompter{answers: map[string]string{}}
			got, err := promptLabelColumn(f, cat, dir, q, tc.supplied)
			if err != nil {
				t.Fatalf("promptLabelColumn(supplied=%q) errored (default not guarded into options?): %v", tc.supplied, err)
			}
			if got != tc.want {
				t.Errorf("promptLabelColumn(supplied=%q) = %q, want %q", tc.supplied, got, tc.want)
			}
		})
	}
}

// TestRunInteractive_Keypoint prompts for the required keypoint count; the
// optional resolution left blank means auto-detect.
func TestRunInteractive_Keypoint(t *testing.T) {
	dir := imageDirLayout(t)
	f := &fakePrompter{answers: map[string]string{
		"How many keypoints per sample?": "17",
		"Which column holds the label?":  "image_label",
	}}
	a := &runDataIngestArgs{
		LocalPath: dir,
		Spec:      push.SpecArgs{Category: "keypoint_detection", Table: "kp_train", Intent: "train"},
	}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
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
	if err := runInteractive(discardPrinter(), f, a); err != nil {
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
			"Please name the dataset.":      "t",
			"Which column holds the label?": "churned",
		},
		confirm: &no,
	}
	a := &runDataIngestArgs{LocalPath: dir, Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a); !errors.Is(err, errInteractiveCancelled) {
		t.Fatalf("err = %v, want errInteractiveCancelled", err)
	}
}

// TestRunInteractive_RejectsBadName: the name prompt runs
// push.ValidateTableName, so an unsafe name surfaces as an error.
func TestRunInteractive_RejectsBadName(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{"Please name the dataset.": "../bad"}}
	a := &runDataIngestArgs{Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a); err == nil {
		t.Fatal("expected an error for an invalid name, got nil")
	}
}

// TestRunInteractive_RejectsEmptyPath: a bare Enter at the path prompt is
// rejected by the validator rather than sniffing the current working
// directory (empty path → Abs("") → cwd).
func TestRunInteractive_RejectsEmptyPath(t *testing.T) {
	f := &fakePrompter{answers: map[string]string{
		"Please name the dataset.": "t",
		"Where is your data?":      "   ",
	}}
	a := &runDataIngestArgs{Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a); err == nil {
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
		"Please name the dataset.":      "t",
		"Where is your data?":           "  " + dir + "  ",
		"Which column holds the label?": "churned",
	}}
	a := &runDataIngestArgs{Spec: push.SpecArgs{Intent: "train"}}
	if err := runInteractive(discardPrinter(), f, a); err != nil {
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

// TestRunInteractive_ShowsExampleHints: the path and schema steps carry a
// visible example, so the guided flow teaches as it goes. (LocalPath is left
// empty so the path step — and its per-modality examples — actually renders.)
func TestRunInteractive_ShowsExampleHints(t *testing.T) {
	dir := tabularDir(t)
	f := &fakePrompter{answers: map[string]string{
		"Please name the dataset.":      "churn_train",
		"Where is your data?":           dir,
		"Which column holds the label?": "churned",
	}}
	a := &runDataIngestArgs{Spec: push.SpecArgs{Intent: "train"}}
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	if err := runInteractive(p, f, a); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	for _, want := range []string{"~/data/patients.csv", "age:INT"} {
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

// TestStripSurroundingQuotes / TestDequotePath / TestValidateDatasetPathDequote
// pin the interactive path-prompt quote handling (#386): a pasted quoted path
// must resolve identically to the bare path, an unquoted path with spaces must
// survive untouched, and a name that really contains a quote must not be
// corrupted beyond the single matched outer pair.
func TestStripSurroundingQuotes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single-quoted", "'/a/b'", "/a/b"},
		{"double-quoted", `"/a/b"`, "/a/b"},
		{"bare", "/a/b", "/a/b"},
		{"bare-with-spaces", "/a/b c/train", "/a/b c/train"},
		{"quoted-with-spaces", "'/home/me/my data/train'", "/home/me/my data/train"},
		{"name-contains-a-quote-unquoted", "/a/b's", "/a/b's"},
		{"name-contains-a-quote-quoted-strips-one-pair", "'/a/b's'", "/a/b's"},
		{"inner-quotes-left-untouched", `/a/"b"/c`, `/a/"b"/c`},
		{"mismatched-quotes-left-untouched", `'/a/b"`, `'/a/b"`},
		{"single-char-not-stripped", "'", "'"},
		{"empty", "", ""},
		{"empty-single-quotes", "''", ""},
		{"empty-double-quotes", `""`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripSurroundingQuotes(tc.in); got != tc.want {
				t.Errorf("stripSurroundingQuotes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDequotePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"quoted-with-surrounding-space", "  '/a/b'  ", "/a/b"},
		{"double-quoted", `"/a/b"`, "/a/b"},
		{"bare-with-interior-spaces", " /a/b c ", "/a/b c"},
		{"quoted-path-with-spaces", "'/home/me/my data/train'", "/home/me/my data/train"},
		{"only-quotes", "''", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dequotePath(tc.in); got != tc.want {
				t.Errorf("dequotePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateDatasetPathRejectsEmptyQuotes(t *testing.T) {
	for _, in := range []string{"", "   ", "''", `""`, "  ''  "} {
		if err := validateDatasetPath(in); err == nil {
			t.Errorf("validateDatasetPath(%q) = nil, want a required-path error", in)
		}
	}
	for _, in := range []string{"'/a/b'", "/a/b", "/a/b c"} {
		if err := validateDatasetPath(in); err != nil {
			t.Errorf("validateDatasetPath(%q) = %v, want nil", in, err)
		}
	}
}
