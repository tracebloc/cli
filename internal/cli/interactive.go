package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/AlecAivazis/survey/v2/terminal"
	"golang.org/x/term"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// prompter is the narrow seam over the interactive library. Production
// uses surveyPrompter (a real terminal); tests inject a fake that
// returns scripted answers, so the prompt-mapping logic is unit-
// testable without a pseudo-terminal — the same trick kubernetes.Interface
// uses to let cluster code run against a fake clientset.
// errInteractiveCancelled is returned when the user declines the
// confirm prompt or hits Ctrl-C. It's control flow, not a failure:
// runDataIngest maps it to a clean exit (0) with a "Cancelled" note.
var errInteractiveCancelled = errors.New("cancelled by user")

type prompter interface {
	// Input asks for free text. def pre-fills the answer; validate, if
	// non-nil, rejects bad input and re-prompts.
	Input(label, help, def string, validate func(string) error) (string, error)
	// Select asks the user to pick one of options; def is the
	// pre-highlighted choice.
	Select(label, help string, options []string, def string) (string, error)
	// Confirm asks a yes/no question; def is the answer on a bare Enter.
	Confirm(label string, def bool) (bool, error)
}

// surveyPrompter is the production prompter, backed by
// AlecAivazis/survey/v2 against the real terminal.
//
// bare drops the question text from the prompt line (survey's Message), for
// flows where the CLI already prints the question itself as a step header
// (the guided ingest flow, via PromptStep) — so the prompt reads "? <answer>"
// with no duplicate question. Confirm always keeps its label (a short y/n with
// no header of its own).
type surveyPrompter struct{ bare bool }

func (s surveyPrompter) message(label string) string {
	if s.bare {
		return ""
	}
	return label
}

func (s surveyPrompter) Input(label, help, def string, validate func(string) error) (string, error) {
	var ans string
	q := &survey.Input{Message: s.message(label), Help: help, Default: def}
	var opts []survey.AskOpt
	if validate != nil {
		// survey hands the validator the raw answer as interface{};
		// for an Input that's always a string.
		opts = append(opts, survey.WithValidator(func(v interface{}) error {
			s, _ := v.(string)
			return validate(s)
		}))
	}
	if err := survey.AskOne(q, &ans, opts...); err != nil {
		return "", mapErr(err)
	}
	return ans, nil
}

func (s surveyPrompter) Select(label, help string, options []string, def string) (string, error) {
	var ans string
	q := &survey.Select{Message: s.message(label), Help: help, Options: options, Default: def}
	if err := survey.AskOne(q, &ans); err != nil {
		return "", mapErr(err)
	}
	return ans, nil
}

func (s surveyPrompter) Confirm(label string, def bool) (bool, error) {
	// Confirm always keeps its label (never bare): a y/N prompt has no step
	// header of its own, and the overwrite-replace confirm fires later, during
	// the cluster phase, with nothing printed before it — a bare "? (y/N)"
	// there would be a label-less destructive prompt.
	ans := def
	if err := survey.AskOne(&survey.Confirm{Message: label, Default: def}, &ans); err != nil {
		return false, mapErr(err)
	}
	return ans, nil
}

// mapErr translates survey's Ctrl-C (terminal.InterruptErr) into our
// errInteractiveCancelled, so the rest of the code never imports survey
// internals to recognize a cancellation — the prompter seam stays
// leak-free.
func mapErr(err error) error {
	if errors.Is(err, terminal.InterruptErr) {
		return errInteractiveCancelled
	}
	return err
}

// isInteractiveTTY reports whether we can run a guided prompt flow:
// both stdin (we read answers) and stdout (we draw prompts) must be a
// real terminal. Piped input, redirected output, or CI all fail this
// and fall back to flag-only behavior.
func isInteractiveTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// runInteractive fills the gaps in a's core ingest fields by prompting,
// data-first (RFC-0002 §12.1): intent → name → path → task → task-specific
// questions → review. It only prompts for what's still missing, so flags
// the user already passed win. taskSet says whether the task was passed
// explicitly (via --task or the hidden --category alias); when it wasn't,
// the family is sniffed from the data the user pointed at (echoed back, or
// asked plainly when ambiguous) and only that family's tasks are offered.
//
// Mutates a through the pointer.
func runInteractive(p *ui.Printer, pr prompter, a *runDataIngestArgs, taskSet bool) error {
	prompted := false

	// The guided flow is a four-step setup: intent → name → path → task. Each
	// question prints as its own step header (PromptStep), with any supporting
	// line beneath it and an answer-only prompt (the prompter runs bare — see
	// surveyPrompter). Everything task-dependent comes AFTER the four steps as
	// unnumbered refinements (Section headers): the label column, and per-task
	// extras like schema, resolution, keypoints. These aren't numbered because
	// which ones apply — and whether any apply at all — depends on the task
	// picked at step 4 (self-supervised text has no label and no extras), so a
	// fixed "of N" couldn't be honest about them.
	//
	// Spacing is uniform (STYLE.md "Guided-prompt spacing"): the header carries
	// its own leading blank; then one blank line, the optional supporting text,
	// one blank line, and the `?` prompt. With no supporting text the single
	// blank sits directly between header and prompt. A result that belongs to an
	// answer (the sniff echo) attaches to it with no blank.

	// Step 1 — intent: what this data is for.
	if a.Spec.Intent == "" {
		p.PromptStep(1, 4, "Do you want to ingest training or test data?")
		p.Newline()
		ans, err := pr.Select("Do you want to ingest training or test data?", "which split this data is",
			[]string{"train", "test"}, "train")
		if err != nil {
			return err
		}
		a.Spec.Intent = ans
		prompted = true
	}

	// Step 2 — name. No auto-fill; the character rules surface only if the
	// name is rejected (see ValidateTableName), so the prompt stays clean.
	if a.Spec.Table == "" {
		p.PromptStep(2, 4, "Please name the dataset.")
		p.Newline()
		ans, err := pr.Input("Please name the dataset.",
			"letters, digits, and underscores; start with a letter or underscore  e.g. churn_train", "",
			push.ValidateTableName)
		if err != nil {
			return err
		}
		a.Spec.Table = ans
		prompted = true
	}

	// Step 3 — path. Show what "file or folder" means per modality, then
	// detect the family from the layout and echo it back.
	if a.LocalPath == "" {
		p.PromptStep(3, 4, "Where is your data?")
		p.Newline()
		p.Hintf("Give the path to a file or a folder — whichever holds your data:")
		p.Infof("Tabular   one CSV file                        e.g. ~/data/patients.csv")
		p.Infof("Images    a folder with labels.csv + images/   e.g. ~/data/xray/")
		p.Infof("Text      a folder with labels.csv + texts/     e.g. ~/data/reviews/")
		p.Newline()
		ans, err := pr.Input("Where is your data?", "e.g. ~/data/patients.csv or ~/data/xray/", "", validateDatasetPath)
		if err != nil {
			return err
		}
		// Trim before storing: validateDatasetPath only trims to check for
		// emptiness, so a pasted " ~/data" (stray leading/trailing space)
		// would otherwise survive here and defeat expandHome (first char
		// isn't '~') — filepath.Abs then prepends cwd and the sniff / label
		// preview read a path that doesn't exist.
		a.LocalPath = strings.TrimSpace(ans)
		prompted = true
	}
	// Expand a leading ~ now so the family sniff + label-header preview read
	// the real path; runDataIngest's own expandHome then no-ops.
	a.LocalPath = expandHome(a.LocalPath)

	// Path existence FIRST (#181): fail plainly on a typo'd path here, before
	// the family sniff / label preview below touch it — otherwise the user
	// answers the whole questionnaire (family, task, label) against a path
	// that doesn't exist, only to hit the hard error afterward. runDataIngest
	// re-checks for the flag-only route; this keeps the invariant on the
	// guided route too. The exitError propagates unwrapped (see runDataIngest).
	if err := statDatasetPath(a.LocalPath); err != nil {
		return err
	}

	// (d) task — family-scoped. An explicit --task wins and skips both the
	// sniff and the picker (§5.1). Otherwise the family is sniffed from the
	// layout (and echoed), or asked plainly when the layout is ambiguous,
	// and then only that family's tasks are offered.
	if !taskSet {
		fam, err := resolveFamily(p, pr, a.LocalPath)
		if err != nil {
			return err
		}
		id, err := pickTask(p, pr, fam)
		if err != nil {
			return err
		}
		a.Spec.Category = id
		prompted = true
	}

	// (e) task-specific questions, including the label column.
	cp, err := promptCategorySpecific(p, pr, a)
	if err != nil {
		return err
	}
	prompted = prompted || cp

	// (f) review + single confirm. Only when we actually prompted something
	// — an ingest fully specified by flags (on a TTY) isn't nagged.
	if prompted {
		renderReview(p, a)
		a.ReviewShown = true
		// No header here: Confirm keeps its own label ("Proceed with the
		// ingest?"), so a Section would just duplicate it. One blank line for
		// breathing room between the Review block and the y/N prompt.
		p.Newline()
		ok, err := pr.Confirm("Proceed with the ingest?", true)
		if err != nil {
			return err
		}
		if !ok {
			return errInteractiveCancelled
		}
	}
	return nil
}

// resolveFamily turns the data the user pointed at into a task family. The
// sniff is a HINT, not a lock (§5.1): a confident layout is echoed and
// used; an ambiguous one is asked plainly. (The caller unconditionally
// prompts for the task afterward, so resolveFamily needn't report whether
// it prompted the family question.)
func resolveFamily(p *ui.Printer, pr prompter, path string) (push.Family, error) {
	s := push.SniffFamily(path)
	if s.Confident {
		p.Successf("%s", s.Echo)
		return s.Family, nil
	}
	if s.Hint != "" {
		// Advisory only — e.g. a mis-cased media folder the walk won't see
		// (#203). We still ask the family question; the hint just tells the
		// user what looks off so they can fix the layout.
		p.Warnf("%s", s.Hint)
	}
	p.Section("What kind of data is this?")
	p.Newline()
	p.Hintf("We couldn't tell from the layout — tabular = a CSV table; image = labels.csv + images/; text = labels.csv + texts/.")
	p.Newline()
	opts := push.FamilyNouns()
	ans, err := pr.Select("What kind of data is this?",
		"tabular = a CSV table; image = labels.csv + images/; text = labels.csv + texts/",
		opts, opts[0])
	if err != nil {
		return 0, err
	}
	return push.FamilyFromNoun(ans), nil
}

// pickTask renders the family's tasks — "Display name — one-liner ·
// task_id", split into Available now and (greyed) Not yet in the CLI —
// and asks the user to pick one of the available ones. It never shows the
// flat 15-item wall: only this family's tasks appear (§7).
func pickTask(p *ui.Printer, pr prompter, fam push.Family) (string, error) {
	var available, pending []push.CategorySpec
	for _, s := range push.CategoriesByFamily(fam) {
		if s.CLISupported {
			available = append(available, s)
		} else {
			pending = append(pending, s)
		}
	}
	if len(available) == 0 {
		// Can't happen with today's registry (every family has a supported
		// task); guard so a future all-pending family fails loudly, not with
		// an index panic.
		return "", fmt.Errorf("no CLI-supported tasks for %s data yet", push.FamilyNoun(fam))
	}

	// Align the task IDs into a column so the blurbs line up, sized to the
	// longest ID shown (available + pending).
	width := 0
	for _, s := range available {
		if len(s.ID) > width {
			width = len(s.ID)
		}
	}
	for _, s := range pending {
		if len(s.ID) > width {
			width = len(s.ID)
		}
	}
	width += 3

	p.PromptStep(4, 4, "What kind of machine learning task is this data for?")
	p.Newline()
	for _, s := range available {
		p.Para(fmt.Sprintf("  %-*s%s", width, s.ID, s.Blurb))
	}
	if len(pending) > 0 {
		p.Newline()
		p.Hintf("Not yet in the CLI:")
		for _, s := range pending {
			p.Hintf("  %-*s%s  (%s)", width, s.ID, s.Blurb, s.UnsupportedNote)
		}
	}
	p.Newline()

	// The options ARE task IDs (what the list shows and what the user picks),
	// so the answer is the category directly. Guard an unexpected answer by
	// falling back to the first available — never return an empty category.
	opts := make([]string, len(available))
	for i, s := range available {
		opts[i] = s.ID
	}
	ans, err := pr.Select("Which task?", "pick the task this data is for", opts, opts[0])
	if err != nil {
		return "", err
	}
	for _, id := range opts {
		if id == ans {
			return ans, nil
		}
	}
	return available[0].ID, nil
}

// promptCategorySpecific prompts for the inputs a particular task needs
// beyond the core fields, filling only the gaps. The label column comes
// first (it's the one question every non-self-supervised task shares),
// then the family-specific extras. Returns whether it prompted anything
// (so the caller knows to show the confirm).
func promptCategorySpecific(p *ui.Printer, pr prompter, a *runDataIngestArgs) (bool, error) {
	cat := a.Spec.Category
	prompted := false

	// Label column — the answer the model learns to produce. The first
	// task-specific refinement (unnumbered Section, like the extras below), not
	// a numbered core step: it's skipped for self-supervised text (MLM/CLM: the
	// target comes from the text itself, there's no label column), so numbering
	// it "of N" would promise a step that flow never reaches. Interactive picks
	// from the REAL CSV header row so the choice exact-matches a column that
	// exists — killing the case-mismatch silent-null-label class
	// (data-ingestors#340) that free-typing "Label" against a "label" header
	// would cause. Wording is per-task: a class to sort into vs a numeric value
	// to predict (§8).
	if !push.SelfSupervisedText(cat) && a.Spec.LabelColumn == "" {
		question := "Which column holds the label?"
		desc := "The answer the model learns to produce — for classification, the class.  e.g. diagnosis, churned"
		if push.IsRegressionClass(cat) {
			question = "Which column holds the value to predict?"
			desc = "The number the model learns to predict.  e.g. price, age, days_to_event"
		}
		p.Section(question)
		p.Newline()
		p.Hintf("%s", desc)
		p.Newline()
		ans, err := promptLabelColumn(pr, cat, a.LocalPath, question)
		if err != nil {
			return prompted, err
		}
		a.Spec.LabelColumn = ans
		prompted = true
	}

	// Further task-specific refinements — like the label above, each gets its
	// own Section header rather than a step number, since which ones appear
	// depends on the task.
	switch {
	case push.IsImage(cat):
		if cat == "keypoint_detection" && a.Spec.NumberOfKeypoints <= 0 {
			p.Section("How many keypoints per sample?")
			p.Newline()
			p.Hintf("The number of landmark points each sample is annotated with — dataset-specific.  e.g. 17 for COCO human pose")
			p.Newline()
			ans, err := pr.Input("How many keypoints per sample?",
				"e.g. 17 for COCO pose", "", validatePositiveInt)
			if err != nil {
				return prompted, err
			}
			n, _ := strconv.Atoi(strings.TrimSpace(ans))
			a.Spec.NumberOfKeypoints = n
			prompted = true
		}
		if a.TargetSizeFlag == "" {
			p.Section("Image resolution")
			p.Newline()
			p.Hintf("The size your images already are, as WxH — tracebloc checks every image matches and never resizes. Press Enter to read it from your first image.  e.g. 224x224")
			p.Newline()
			ans, err := pr.Input("Image resolution",
				"the size your images already are; tracebloc checks it, it never resizes", "",
				validateOptionalTargetSize)
			if err != nil {
				return prompted, err
			}
			a.TargetSizeFlag = strings.TrimSpace(ans)
			prompted = true
		}
	case push.IsTabular(cat):
		if a.SchemaFlag == "" {
			p.Section("Column types")
			p.Newline()
			p.Hintf("We infer each column's type from your CSV. Press Enter to accept, or type overrides like age:INT,price:FLOAT.")
			p.Newline()
			ans, err := pr.Input("Column types", "e.g. age:INT,price:FLOAT", "", validateOptionalSchema)
			if err != nil {
				return prompted, err
			}
			a.SchemaFlag = strings.TrimSpace(ans)
			prompted = true
		}
		if push.IsRegressionClass(cat) && a.Spec.LabelPolicy == "" {
			p.Section("Label policy")
			p.Newline()
			p.Hintf("Regression targets are continuous. 'bucket' groups them into ranges before they leave the cluster; 'passthrough' keeps raw values.")
			p.Newline()
			ans, err := pr.Select("Label policy",
				"bucket bins the target before it leaves the cluster",
				[]string{"bucket", "passthrough"}, "bucket")
			if err != nil {
				return prompted, err
			}
			a.Spec.LabelPolicy = ans
			prompted = true
		}
		if cat == "time_to_event_prediction" && a.Spec.TimeColumn == "" {
			p.Section("Time column")
			p.Newline()
			p.Hintf("The column holding the duration / time-to-event.  e.g. time, tenure_days")
			p.Newline()
			ans, err := pr.Input("Time column", "the duration/time column name", "time", nil)
			if err != nil {
				return prompted, err
			}
			a.Spec.TimeColumn = strings.TrimSpace(ans)
			prompted = true
		}
	}
	return prompted, nil
}

// promptLabelColumn asks for the label/target column. When the CSV header
// can be read, it offers those columns as an exact-match SELECT (defaulting
// to a column literally named "label" if present); otherwise — the header
// isn't readable yet — it falls back to free text so the flow never stalls.
func promptLabelColumn(pr prompter, category, root, question string) (string, error) {
	headers, err := push.PreviewLabelHeaders(category, root)
	if err == nil && len(headers) > 0 {
		ans, serr := pr.Select(question,
			"pick the label/target column from your CSV header", headers, defaultLabelChoice(headers))
		return strings.TrimSpace(ans), serr
	}
	ans, ierr := pr.Input(question, "the label/target column name", "", nil)
	return strings.TrimSpace(ans), ierr
}

// defaultLabelChoice pre-highlights a column literally named "label"
// (case-insensitive) when one exists, else the first column — a sensible
// starting point for the SELECT.
func defaultLabelChoice(headers []string) string {
	for _, h := range headers {
		if strings.EqualFold(h, "label") {
			return h
		}
	}
	// The only caller (promptLabelColumn) guards len(headers) > 0 before
	// calling, so headers is never empty here.
	return headers[0]
}

// renderReview prints the assembled ingest inputs before the confirm
// prompt, so the user sees exactly what's about to happen. Order mirrors
// the data-first flow: name → task → intent → path, then task extras.
func renderReview(p *ui.Printer, a *runDataIngestArgs) {
	p.Section("Review")
	p.Field("name", a.Spec.Table)
	p.Field("task", a.Spec.Category)
	p.Field("intent", a.Spec.Intent)
	p.Field("path", a.LocalPath)
	if a.Spec.LabelColumn != "" {
		p.Field("label column", a.Spec.LabelColumn)
	}
	if a.Spec.NumberOfKeypoints > 0 {
		p.Field("keypoints", strconv.Itoa(a.Spec.NumberOfKeypoints))
	}
	switch {
	case a.TargetSizeFlag != "":
		p.Field("resolution", a.TargetSizeFlag)
	case push.IsImage(a.Spec.Category):
		p.Field("resolution", "auto-detect")
	}
	// Only shown when set — --min-size is opt-in with no local default,
	// so there's nothing to echo otherwise. Surfacing it lets a mistyped
	// floor (e.g. 640x640 for 64x64) be caught at the confirm gate.
	if a.MinSizeFlag != "" {
		p.Field("min size", a.MinSizeFlag)
	}
	switch {
	case a.SchemaFlag != "":
		p.Field("schema", a.SchemaFlag)
	case push.IsTabular(a.Spec.Category):
		p.Field("schema", "infer from CSV")
	}
	if a.Spec.LabelPolicy != "" {
		p.Field("label policy", a.Spec.LabelPolicy)
	}
	if a.Spec.TimeColumn != "" {
		p.Field("time column", a.Spec.TimeColumn)
	}
}

// validateDatasetPath rejects an empty / whitespace-only answer. Without
// it, a bare Enter at the path prompt yields "" — and SniffFamily(Abs(""))
// would sniff the current working directory before any empty-path guard
// runs, silently ingesting whatever happens to sit in the cwd.
func validateDatasetPath(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("a dataset path is required")
	}
	return nil
}

// validatePositiveInt accepts a string that parses to an int > 0.
func validatePositiveInt(s string) error {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err != nil || n <= 0 {
		return fmt.Errorf("must be a positive integer")
	}
	return nil
}

// validateOptionalTargetSize accepts "" (auto-detect) or a valid WxH.
func validateOptionalTargetSize(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	_, _, err := push.ParseTargetSize(s)
	return err
}

// validateOptionalSchema accepts "" (infer) or a valid col:TYPE,... set.
func validateOptionalSchema(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	_, err := push.ParseSchema(s)
	return err
}
