package cli

import (
	"os"

	"github.com/AlecAivazis/survey/v2"
	"golang.org/x/term"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// promptCategories is the ordered list offered by the interactive
// category picker — the categories `dataset push` supports today (the
// same set runDatasetPush's category gate accepts). semantic_ /
// instance_segmentation are omitted until they're implemented.
var promptCategories = []string{
	"image_classification",
	"object_detection",
	"keypoint_detection",
	"text_classification",
	"masked_language_modeling",
	"tabular_classification",
	"tabular_regression",
	"time_series_forecasting",
	"time_to_event_prediction",
}

// prompter is the narrow seam over the interactive library. Production
// uses surveyPrompter (a real terminal); tests inject a fake that
// returns scripted answers, so the prompt-mapping logic is unit-
// testable without a pseudo-terminal — the same trick kubernetes.Interface
// uses to let cluster code run against a fake clientset.
type prompter interface {
	// Input asks for free text. def pre-fills the answer; validate, if
	// non-nil, rejects bad input and re-prompts.
	Input(label, help, def string, validate func(string) error) (string, error)
	// Select asks the user to pick one of options; def is the
	// pre-highlighted choice.
	Select(label, help string, options []string, def string) (string, error)
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
		return "", err
	}
	return ans, nil
}

func (surveyPrompter) Select(label, help string, options []string, def string) (string, error) {
	var ans string
	q := &survey.Select{Message: label, Help: help, Options: options, Default: def}
	if err := survey.AskOne(q, &ans); err != nil {
		return "", err
	}
	return ans, nil
}

// isInteractiveTTY reports whether we can run a guided prompt flow:
// both stdin (we read answers) and stdout (we draw prompts) must be a
// real terminal. Piped input, redirected output, or CI all fail this
// and fall back to flag-only behavior.
func isInteractiveTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// runInteractive fills the gaps in a's core push fields by prompting,
// then returns. It only prompts for what's still missing, so flags the
// user already passed win. categorySet says whether --category was set
// explicitly (vs left at its non-empty default), which would otherwise
// hide "the user didn't actually choose a category."
//
// Mutates a through the pointer. PR-b adds category-specific prompts
// (target-size, schema, number-of-keypoints) + a confirm screen.
func runInteractive(p *ui.Printer, pr prompter, a *runDatasetPushArgs, categorySet bool) error {
	p.PromptHeader("Let's set up your dataset push")
	p.Hintf("Press Enter to accept a default; Ctrl-C to cancel.")

	if a.LocalPath == "" {
		ans, err := pr.Input("Path to your dataset directory", "e.g. ./my-data", "", nil)
		if err != nil {
			return err
		}
		a.LocalPath = ans
	}

	if !categorySet {
		ans, err := pr.Select("Task category", "what kind of data this is",
			promptCategories, a.Spec.Category)
		if err != nil {
			return err
		}
		a.Spec.Category = ans
	}

	if a.Spec.Table == "" {
		ans, err := pr.Input("Destination table name",
			"MySQL identifier + PVC subdir; letters, digits, underscore only", "",
			push.ValidateTableName)
		if err != nil {
			return err
		}
		a.Spec.Table = ans
	}

	if a.Spec.Intent == "" {
		ans, err := pr.Select("Intent", "which split this data is",
			[]string{"train", "test"}, "train")
		if err != nil {
			return err
		}
		a.Spec.Intent = ans
	}

	// masked_language_modeling is self-supervised — no label column.
	if a.Spec.LabelColumn == "" && a.Spec.Category != "masked_language_modeling" {
		ans, err := pr.Input("Label column",
			"the column in labels.csv that holds the label", "label", nil)
		if err != nil {
			return err
		}
		a.Spec.LabelColumn = ans
	}

	return nil
}
