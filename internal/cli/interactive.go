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
type surveyPrompter struct{}

func (surveyPrompter) Input(label, help, def string, validate func(string) error) (string, error) {
	var ans string
	q := &survey.Input{Message: label, Help: help, Default: def}
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

func (surveyPrompter) Select(label, help string, options []string, def string) (string, error) {
	var ans string
	q := &survey.Select{Message: label, Help: help, Options: options, Default: def}
	if err := survey.AskOne(q, &ans); err != nil {
		return "", mapErr(err)
	}
	return ans, nil
}

func (surveyPrompter) Confirm(label string, def bool) (bool, error) {
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
	p.PromptHeader("Let's set up your data ingest")
	p.Hintf("Press Enter to accept a default; Ctrl-C to cancel.")
	prompted := false

	// (a) intent — the first thing to settle: what this data is for.
	if a.Spec.Intent == "" {
		p.PromptHint("Whether this split trains the model or evaluates it.")
		ans, err := pr.Select("Is this training or test data?", "which split this data is",
			[]string{"train", "test"}, "train")
		if err != nil {
			return err
		}
		a.Spec.Intent = ans
		prompted = true
	}

	// (b) name — no auto-fill: the example lives in the hint, so the user
	// types their own name rather than editing a pre-filled default.
	if a.Spec.Table == "" {
		p.PromptHint("A name for this dataset — you'll reference it by this name when you start a training run. Start with a letter or underscore, then letters, digits, underscores.  e.g. churn_train")
		ans, err := pr.Input("What should we call this dataset?",
			"MySQL identifier + PVC subdir; start with a letter or underscore, then letters, digits, underscore", "",
			push.ValidateTableName)
		if err != nil {
			return err
		}
		a.Spec.Table = ans
		prompted = true
	}

	// (c) path — then detect the family from the layout and echo it back.
	if a.LocalPath == "" {
		p.PromptHint("The file or folder holding your data — a single .csv for a table, or labels.csv + an images/ folder for images.  e.g. ~/datasets/churn")
		ans, err := pr.Input("Where is your data? (file or folder)", "e.g. ./my-data", "", validateDatasetPath)
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
	if s := push.SniffFamily(path); s.Confident {
		p.Successf("%s", s.Echo)
		return s.Family, nil
	}
	p.PromptHint("We couldn't tell the data type from what's there — which is it?")
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

	p.Section(fmt.Sprintf("Tasks for %s data", push.FamilyNoun(fam)))
	p.Hintf("Available now:")
	for _, s := range available {
		p.Infof("%s — %s · %s", s.DisplayName(), s.Blurb, s.ID)
	}
	if len(pending) > 0 {
		p.Hintf("Not yet in the CLI:")
		for _, s := range pending {
			p.Hintf("  %s — %s · %s  (%s)", s.DisplayName(), s.Blurb, s.ID, s.UnsupportedNote)
		}
	}

	opts := make([]string, len(available))
	byName := make(map[string]string, len(available))
	for i, s := range available {
		opts[i] = s.DisplayName()
		byName[s.DisplayName()] = s.ID
	}
	ans, err := pr.Select("Which task?", "pick the task this data is for", opts, opts[0])
	if err != nil {
		return "", err
	}
	if id, ok := byName[ans]; ok {
		return id, nil
	}
	// Defensive: an answer that isn't one of the offered display names.
	// Never return an empty category — fall back to the first available.
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

	// Label column — the answer the model learns to produce. Skipped for
	// self-supervised text (MLM/CLM: the target comes from the text itself,
	// there's no label column). Interactive picks from the REAL CSV header
	// row so the choice exact-matches a column that exists — killing the
	// case-mismatch silent-null-label class (data-ingestors#340) that
	// free-typing "Label" against a "label" header would cause. Wording is
	// per-task: a class to sort into vs a numeric value to predict (§8).
	if !push.SelfSupervisedText(cat) && a.Spec.LabelColumn == "" {
		question := "Which column holds the class?"
		if push.IsRegressionClass(cat) {
			question = "Which column holds the value to predict?"
		}
		p.PromptHint("The column in your CSV with the answer the model learns to produce.")
		ans, err := promptLabelColumn(pr, cat, a.LocalPath, question)
		if err != nil {
			return prompted, err
		}
		a.Spec.LabelColumn = ans
		prompted = true
	}

	switch {
	case push.IsImage(cat):
		if cat == "keypoint_detection" && a.Spec.NumberOfKeypoints <= 0 {
			p.PromptHint("How many keypoints each sample is annotated with — dataset-specific, no default.  e.g. 17 for COCO human pose")
			ans, err := pr.Input("Number of keypoints per sample",
				"e.g. 17 for COCO pose", "", validatePositiveInt)
			if err != nil {
				return prompted, err
			}
			n, _ := strconv.Atoi(strings.TrimSpace(ans))
			a.Spec.NumberOfKeypoints = n
			prompted = true
		}
		if a.TargetSizeFlag == "" {
			p.PromptHint("The resolution your images already are. tracebloc never resizes — it checks every image is exactly this size and rejects any that differ. Blank = read it from your first image.  e.g. 224x224")
			ans, err := pr.Input("Image resolution as WxH (blank = read it from your first image)",
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
			p.PromptHint("Override the column types the CLI would infer. Blank = infer from the CSV.  e.g. age:INT,price:FLOAT,city:VARCHAR")
			ans, err := pr.Input("Column schema as col:TYPE,... (blank = infer from the CSV)",
				"e.g. age:INT,price:FLOAT", "", validateOptionalSchema)
			if err != nil {
				return prompted, err
			}
			a.SchemaFlag = strings.TrimSpace(ans)
			prompted = true
		}
		if push.IsRegressionClass(cat) && a.Spec.LabelPolicy == "" {
			p.PromptHint("Regression targets are continuous. 'bucket' groups them into ranges before they leave the cluster; 'passthrough' keeps raw values.")
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
			p.PromptHint("The column holding the duration / time-to-event.  e.g. time, tenure_days")
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
