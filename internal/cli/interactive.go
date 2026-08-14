package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime"
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
// confirm prompt or hits Ctrl-C. It's control flow, not a failure —
// every site reports it through cleanCancel / mapPromptErr below: a
// visible "Cancelled — …" note and a clean exit (0).
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
	defer enableKeyEventInput()()
	if err := survey.AskOne(q, &ans, opts...); err != nil {
		return "", mapErr(err)
	}
	return ans, nil
}

func (s surveyPrompter) Select(label, help string, options []string, def string) (string, error) {
	var ans string
	q := &survey.Select{Message: s.message(label), Help: help, Options: options, Default: def}
	// Arrow keys only work while the console delivers VK_* key events; see
	// enableKeyEventInput. Without this, ↓ typed "[B" into the filter on Windows and
	// the selection never moved (#475).
	defer enableKeyEventInput()()
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
	defer enableKeyEventInput()()
	if err := survey.AskOne(&survey.Confirm{Message: label, Default: def}, &ans); err != nil {
		return false, mapErr(err)
	}
	return ans, nil
}

// datasetPathExamples returns the per-modality example paths in the form the user's own
// OS actually uses.
//
// The prompt used to show only ~/data/... . On Windows that is not a path anyone has, and with
// no example to copy the shape of, users invent formats the CLI can't read -- a Python-style
// r"C:\Users\..." literal was what prompted client#615. An example in the right shape is the
// cheapest possible fix: it costs one line of output and removes the guesswork.
//
// Parameterised on goos rather than reading runtime.GOOS internally so a test can exercise the
// Windows branch from a Linux or macOS runner, where it would otherwise never be covered.
func datasetPathExamples(goos string) (tabular, images, text string) {
	if goos == "windows" {
		return `C:\Users\you\data\patients.csv`, `C:\Users\you\data\xray\`, `C:\Users\you\data\reviews\`
	}
	return "~/data/patients.csv", "~/data/xray/", "~/data/reviews/"
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

// cleanCancel is the ONE place a cancelled prompt is reported to the user. It
// prints the CLI's cancellation line — "Cancelled — <nothing>." — and returns
// nil, which ExitCodeFromError maps to exitOK.
//
// Exit 0 is the convention every prompting command follows (data ingest, data
// delete, resources set, client create, delete): backing out at a question is a
// user choice, and nothing was started, so there is no failure to report.
// exitInterrupted (130) is for the OTHER Ctrl-C — the one that interrupts work
// already in flight (the sign-in wait, `client status --wait`, the seal suite, an
// installer re-run), where an operation really was cut short. See exitcodes.go.
//
// nothing says what did NOT happen ("nothing was changed."), and takes format
// args for the sites that name the thing they left alone. The prefix lives here
// so no site invents its own wording, and the argument is required so no site can
// report a cancellation without saying what it left untouched.
func cleanCancel(p *ui.Printer, nothing string, a ...any) error {
	p.Infof("Cancelled — %s", fmt.Sprintf(nothing, a...))
	return nil
}

// mapPromptErr maps a prompter error to the CLI's exit contract, so a prompt can
// neither fail nor be cancelled silently. Ctrl-C (errInteractiveCancelled, from
// mapErr above) goes through cleanCancel — the same visible note and the same
// exit 0 the site's declined-answer branch produces. Anything else is a real
// prompt failure: exit 1.
//
// The Printer and the note are in the signature deliberately. The bug this
// replaced mapped the cancellation straight to nil, so Ctrl-C exited 0 with no
// output at all — a script could not tell it apart from a completed run
// (backend#1253). Handling the sentinel now costs you a note; printing nothing
// is no longer reachable.
//
// Sites whose non-cancel error needs a code other than exitFailure keep their own
// errors.Is check and call cleanCancel directly — the printing still funnels
// through one place.
func mapPromptErr(p *ui.Printer, err error, nothing string, a ...any) error {
	if errors.Is(err, errInteractiveCancelled) {
		return cleanCancel(p, nothing, a...)
	}
	return &exitError{code: exitFailure, err: err}
}

// isInteractiveTTY reports whether we can run a guided prompt flow:
// both stdin (we read answers) and stdout (we draw prompts) must be a
// real terminal. Piped input, redirected output, or CI all fail this
// and fall back to flag-only behavior.
func isInteractiveTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// defaultInOptions returns want when it is one of options, otherwise fallback.
// A survey.Select whose Default is not among its Options aborts on a real
// terminal ("default value … not found in options"), so any command-line value
// used to PRE-FILL a Select must be validated against that Select's options
// first — a typo like --intent training or --label-policy buckets would
// otherwise crash the prompt survey can't draw. When the supplied value isn't a
// real option the question is still ASKED; the prompt just opens on the
// sensible fallback. This is the guard pickTask has always applied, factored out
// so every supplied-default Select shares one implementation.
func defaultInOptions(want string, options []string, fallback string) string {
	for _, o := range options {
		if o == want {
			return want
		}
	}
	return fallback
}

// runInteractive walks a's core ingest fields by prompting, data-first
// (RFC-0002 §12.1): intent → name → path → task → task-specific questions →
// review.
//
// It asks EVERY question relevant to the chosen task (#711). A value the user
// passed on the command line pre-fills the answer — Enter accepts it — but never
// suppresses the question. Which questions are relevant still depends on the
// task picked at step 4; that is the only gate.
//
// When no task was supplied, the family is sniffed from the data the user
// pointed at (echoed back, or asked plainly when ambiguous) and only that
// family's tasks are offered. When one WAS supplied it selects the family
// directly, so the user's own answer is always among the options.
//
// Mutates a through the pointer.
func runInteractive(p *ui.Printer, pr prompter, a *runDataIngestArgs) error {

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

	// Guided mode ASKS. A value that arrived on the command line becomes the
	// prompt's DEFAULT — never a reason to skip the question (#711).
	//
	// It used to skip: each step was wrapped in `if <value> == ""`. That turned a
	// path which had merely gone stale into a dead end — the user was sitting at
	// a prompt, ready to answer, and instead got "no such file or directory" with
	// no way to correct it. Data moves between runs; a name or a task does not
	// stop being true, but the flow should not have to reason about which is
	// which. So: always ask, pre-filled, Enter accepts.
	//
	// Only the TASK gate survives, one level down — which questions apply still
	// depends on the task chosen at step 4 (self-supervised text has no label and
	// no extras). That gate is about relevance, not about what the user typed.
	//
	// Non-interactive is untouched: runInteractive is only reached on a TTY
	// without --no-input/--output-json, so scripts still take flags silently.

	// Step 1 — intent: what this data is for.
	{
		opts := []string{"train", "test"}
		// A supplied --intent pre-fills the prompt, but only when it names a real
		// option: a typo would otherwise crash survey.Select on a TTY (see
		// defaultInOptions). Unknown value → fall back to "train" and still ask.
		def := defaultInOptions(a.Spec.Intent, opts, "train")
		p.PromptStep(1, 4, "Do you want to ingest training or test data?")
		p.Newline()
		ans, err := pr.Select("Do you want to ingest training or test data?", "which split this data is",
			opts, def)
		if err != nil {
			return err
		}
		a.Spec.Intent = ans
	}

	// Step 2 — name. The character rules surface only if the name is rejected
	// (see ValidateTableName), so the prompt stays clean.
	{
		p.PromptStep(2, 4, "Please name the dataset.")
		p.Newline()
		ans, err := pr.Input("Please name the dataset.",
			"letters, digits, and underscores — no hyphens or spaces, use _; start with a letter or underscore  e.g. churn_train",
			a.Spec.Table,
			push.ValidateTableName)
		if err != nil {
			return err
		}
		a.Spec.Table = ans
	}

	// Step 3 — path. Show what "file or folder" means per modality, then
	// detect the family from the layout and echo it back.
	{
		p.PromptStep(3, 4, "Where is your data?")
		p.Newline()
		exTab, exImg, exTxt := datasetPathExamples(runtime.GOOS)
		p.Hintf("Give the path to a file or a folder — whichever holds your data:")
		p.Infof("Tabular   one CSV file                        e.g. %s", exTab)
		p.Infof("Images    a folder with labels.csv + images/   e.g. %s", exImg)
		p.Infof("Text      a folder with labels.csv + texts/     e.g. %s", exTxt)
		p.Newline()
		ans, err := pr.Input("Where is your data?", fmt.Sprintf("e.g. %s or %s", exTab, exImg),
			a.LocalPath, validateDatasetPath)
		if err != nil {
			return err
		}
		// Canonicalize before storing: dequotePath trims stray leading/
		// trailing space (a pasted " ~/data" would otherwise defeat expandHome,
		// whose first char isn't '~', and filepath.Abs would prepend cwd so the
		// sniff / label preview read a path that doesn't exist) AND strips one
		// matching pair of surrounding quotes. Dragging a folder into a terminal
		// auto-quotes it, and users habitually quote paths with spaces; this
		// prompt is read literally, not shell-parsed, so the quotes would
		// otherwise become part of the path (#386). validateDatasetPath applies
		// the same canonicalization, so the re-prompt guard stays consistent.
		a.LocalPath = dequotePath(ans)
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

	// (d) task — family-scoped, and always asked (#711).
	//
	// An explicit --task no longer skips the picker; it selects the family and
	// becomes the pre-selected option. Taking the family from the SUPPLIED task
	// rather than the sniff matters: the two can disagree (a tabular task passed
	// against a folder that sniffs image), and scoping the list to the sniffed
	// family would leave the user's own answer missing from its own default.
	// With no task supplied, the family is sniffed from the layout (and echoed),
	// or asked plainly when the layout is ambiguous — unchanged.
	{
		var fam push.Family
		if spec, ok := push.Lookup(a.Spec.Category); ok {
			fam = spec.Family
		} else {
			f, err := resolveFamily(p, pr, a.LocalPath)
			if err != nil {
				return err
			}
			fam = f
		}
		id, err := pickTask(p, pr, fam, a.Spec.Category)
		if err != nil {
			return err
		}
		a.Spec.Category = id
	}

	// (e) task-specific questions, including the label column.
	if _, err := promptCategorySpecific(p, pr, a); err != nil {
		return err
	}

	// (f) review + single confirm — unconditional now (#711). It used to be
	// gated on "did we actually prompt anything", so an ingest fully specified
	// by flags wasn't nagged. Guided mode always prompts now, so that gate can
	// only ever be true; keeping it would be a condition that reads as a choice
	// while having none.
	{
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
func pickTask(p *ui.Printer, pr prompter, fam push.Family, want string) (string, error) {
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
	// def pre-selects a task the user already named (#711), honoured only when it
	// is actually in this family's list (defaultInOptions) — survey would
	// otherwise render a default the user cannot see, and an Enter on it would be
	// unexplainable.
	def := defaultInOptions(want, opts, opts[0])
	ans, err := pr.Select("Which task?", "pick the task this data is for", opts, def)
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
// beyond the core fields. Like the core steps (#711), each question is always
// asked with any supplied value pre-filled as the default — never skipped. The
// label column comes first (the one question every non-self-supervised task
// shares), then the family-specific extras. Returns whether it prompted anything
// (retained for the caller; the confirm is unconditional now).
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
	//
	// A supplied --label-column pre-fills the pick (like every other value under
	// #711) — it no longer SKIPS the question, so a wrong or mistyped column can
	// be corrected here the same way a stale path can. Only self-supervised text
	// (no label at all) still bypasses it.
	if !push.SelfSupervisedText(cat) {
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
		ans, err := promptLabelColumn(pr, cat, a.LocalPath, question, a.Spec.LabelColumn)
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
		if cat == "keypoint_detection" {
			p.Section("How many keypoints per sample?")
			p.Newline()
			p.Hintf("The number of landmark points each sample is annotated with — dataset-specific.  e.g. 17 for COCO human pose")
			p.Newline()
			kpDef := ""
			if a.Spec.NumberOfKeypoints > 0 {
				kpDef = strconv.Itoa(a.Spec.NumberOfKeypoints)
			}
			ans, err := pr.Input("How many keypoints per sample?",
				"e.g. 17 for COCO pose", kpDef, validatePositiveInt)
			if err != nil {
				return prompted, err
			}
			n, _ := strconv.Atoi(strings.TrimSpace(ans))
			a.Spec.NumberOfKeypoints = n
			prompted = true
		}
		{
			p.Section("Image resolution")
			p.Newline()
			p.Hintf("The size your images already are, as WxH — tracebloc checks every image matches and never resizes. Press Enter to read it from your first image.  e.g. 224x224")
			p.Newline()
			ans, err := pr.Input("Image resolution",
				"the size your images already are; tracebloc checks it, it never resizes", a.TargetSizeFlag,
				validateOptionalTargetSize)
			if err != nil {
				return prompted, err
			}
			a.TargetSizeFlag = strings.TrimSpace(ans)
			prompted = true
		}
	case push.IsTabular(cat):
		{
			p.Section("Column types")
			p.Newline()
			p.Hintf("We infer each column's type from your CSV. Press Enter to accept, or type overrides like age:INT,price:FLOAT.")
			p.Newline()
			ans, err := pr.Input("Column types", "e.g. age:INT,price:FLOAT", a.SchemaFlag, validateOptionalSchema)
			if err != nil {
				return prompted, err
			}
			a.SchemaFlag = strings.TrimSpace(ans)
			prompted = true
		}
		if push.IsRegressionClass(cat) {
			p.Section("Label policy")
			p.Newline()
			p.Hintf("Regression targets are continuous. 'bucket' groups them into ranges before they leave the cluster; 'passthrough' keeps raw values.")
			p.Newline()
			opts := []string{"bucket", "passthrough"}
			// A supplied --label-policy pre-fills only when valid; a typo like
			// "buckets" would otherwise abort the Select on a TTY (defaultInOptions).
			lpDef := defaultInOptions(a.Spec.LabelPolicy, opts, "bucket")
			ans, err := pr.Select("Label policy",
				"bucket bins the target before it leaves the cluster",
				opts, lpDef)
			if err != nil {
				return prompted, err
			}
			a.Spec.LabelPolicy = ans
			prompted = true
		}
		if cat == "time_to_event_prediction" {
			p.Section("Time column")
			p.Newline()
			p.Hintf("The column holding the duration / time-to-event.  e.g. time, tenure_days")
			p.Newline()
			tcDef := a.Spec.TimeColumn
			if tcDef == "" {
				tcDef = "time"
			}
			ans, err := pr.Input("Time column", "the duration/time column name", tcDef, nil)
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
// can be read, it offers those columns as an exact-match SELECT — pre-selecting
// a supplied --label-column when it names a real header, else a column literally
// named "label" if present; otherwise — the header isn't readable yet — it falls
// back to free text pre-filled with the supplied value so the flow never stalls.
//
// supplied is guarded through defaultInOptions before it reaches survey.Select:
// a mistyped --label-column that is not one of the real headers would otherwise
// abort the prompt on a TTY, the same default-not-in-options crash guarded
// everywhere else in the guided flow.
func promptLabelColumn(pr prompter, category, root, question, supplied string) (string, error) {
	headers, err := push.PreviewLabelHeaders(category, root)
	if err == nil && len(headers) > 0 {
		ans, serr := pr.Select(question,
			"pick the label/target column from your CSV header", headers,
			defaultInOptions(supplied, headers, defaultLabelChoice(headers)))
		return strings.TrimSpace(ans), serr
	}
	ans, ierr := pr.Input(question, "the label/target column name", supplied, nil)
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
// runs, silently ingesting whatever happens to sit in the cwd. It validates
// the canonicalized value (dequotePath) so an answer that is nothing but an
// empty pair of quotes is rejected at the prompt rather than surviving to
// statDatasetPath with a messy error (#386).
func validateDatasetPath(s string) error {
	if dequotePath(s) == "" {
		return fmt.Errorf("a dataset path is required")
	}
	return nil
}

// dequotePath canonicalizes an interactive path answer: it trims surrounding
// whitespace, then strips one matching pair of surrounding quotes (via
// stripSurroundingQuotes), then trims again in case the quotes wrapped padding.
// It is applied ONLY to the guided prompt answer, never to the flag/positional
// path — the shell already de-quotes those.
func dequotePath(s string) string {
	return strings.TrimSpace(stripSurroundingQuotes(strings.TrimSpace(s)))
}

// stripSurroundingQuotes removes at most one matching pair of surrounding
// single or double quotes from s, and only when the first and last rune are
// the SAME quote char (' or ') and s has at least two runes. Inner quotes are
// left untouched, mismatched quotes ('…" ) are left as-is, and a path whose
// real name contains a quote loses only that single matched outer pair. This
// exists because the interactive path prompt is read literally, not
// shell-parsed, so a pasted quoted path would otherwise fail to resolve (#386).
func stripSurroundingQuotes(s string) string {
	r := []rune(s)
	if len(r) < 2 {
		return s
	}
	first, last := r[0], r[len(r)-1]
	if (first == '\'' || first == '"') && first == last {
		return string(r[1 : len(r)-1])
	}
	return s
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
