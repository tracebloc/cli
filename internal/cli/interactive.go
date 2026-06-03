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
// errInteractiveCancelled is returned when the user declines the
// confirm prompt or hits Ctrl-C. It's control flow, not a failure:
// runDatasetPush maps it to a clean exit (0) with a "Cancelled" note.
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
	prompted := false

	if a.LocalPath == "" {
		ans, err := pr.Input("Path to your dataset directory", "e.g. ./my-data", "", nil)
		if err != nil {
			return err
		}
		a.LocalPath = ans
		prompted = true
	}

	if !categorySet {
		ans, err := pr.Select("Task category", "what kind of data this is",
			promptCategories, a.Spec.Category)
		if err != nil {
			return err
		}
		a.Spec.Category = ans
		prompted = true
	}

	if a.Spec.Table == "" {
		ans, err := pr.Input("Destination table name",
			"MySQL identifier + PVC subdir; letters, digits, underscore only", "",
			push.ValidateTableName)
		if err != nil {
			return err
		}
		a.Spec.Table = ans
		prompted = true
	}

	if a.Spec.Intent == "" {
		ans, err := pr.Select("Intent", "which split this data is",
			[]string{"train", "test"}, "train")
		if err != nil {
			return err
		}
		a.Spec.Intent = ans
		prompted = true
	}

	// masked_language_modeling is self-supervised — no label column.
	if a.Spec.LabelColumn == "" && a.Spec.Category != "masked_language_modeling" {
		ans, err := pr.Input("Label column",
			"the column in labels.csv that holds the label", "label", nil)
		if err != nil {
			return err
		}
		a.Spec.LabelColumn = ans
		prompted = true
	}

	cp, err := promptCategorySpecific(pr, a)
	if err != nil {
		return err
	}
	prompted = prompted || cp

	// Confirm only when we actually prompted something — a push that's
	// fully specified by flags (on a TTY) isn't nagged with a confirm.
	if prompted {
		renderReview(p, a)
		ok, err := pr.Confirm("Proceed with the push?", true)
		if err != nil {
			return err
		}
		if !ok {
			return errInteractiveCancelled
		}
	}
	return nil
}

// promptCategorySpecific prompts for the inputs a particular category
// needs beyond the core fields, filling only the gaps. Returns whether
// it prompted anything (so the caller knows to show the confirm).
func promptCategorySpecific(pr prompter, a *runDatasetPushArgs) (bool, error) {
	cat := a.Spec.Category
	prompted := false
	switch {
	case push.IsImage(cat):
		if cat == "keypoint_detection" && a.Spec.NumberOfKeypoints <= 0 {
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
			ans, err := pr.Input("Image resolution as WxH (blank = auto-detect from the first image)",
				"all images must share it; the ingestor validates, it doesn't resize", "",
				validateOptionalTargetSize)
			if err != nil {
				return prompted, err
			}
			a.TargetSizeFlag = strings.TrimSpace(ans)
			prompted = true
		}
	case push.IsTabular(cat):
		if a.SchemaFlag == "" {
			ans, err := pr.Input("Column schema as col:TYPE,... (blank = infer from the CSV)",
				"e.g. age:INT,price:FLOAT", "", validateOptionalSchema)
			if err != nil {
				return prompted, err
			}
			a.SchemaFlag = strings.TrimSpace(ans)
			prompted = true
		}
		if push.IsRegressionClass(cat) && a.Spec.LabelPolicy == "" {
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

// renderReview prints the assembled push inputs before the confirm
// prompt, so the user sees exactly what's about to happen.
func renderReview(p *ui.Printer, a *runDatasetPushArgs) {
	p.Section("Review")
	p.Field("path", a.LocalPath)
	p.Field("category", a.Spec.Category)
	p.Field("table", a.Spec.Table)
	p.Field("intent", a.Spec.Intent)
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
