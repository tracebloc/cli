package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/schema"
)

// newIngestCmd implements the `tracebloc ingest` subtree. Today it
// has only one verb — `validate` — which runs the schema check
// locally without touching the cluster. Future verbs (status, retry,
// cancel) hang off this same parent in later phases.
func newIngestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Inspect and manage ingestion configurations",
		Long: `Commands for working with ingest.yaml files and ingestion runs.

Today only ` + "`validate`" + ` is implemented — it runs the same schema check
the cluster's jobs-manager runs, but locally and instantly. Use it to
catch typos and missing fields before submitting an ingestion.

Future verbs (status, retry, cancel) will land alongside the
push/list/show commands in later phases.`,
	}

	cmd.AddCommand(newIngestValidateCmd())
	return cmd
}

// newIngestValidateCmd implements `tracebloc ingest validate <path>`.
// Reads a YAML file from disk, validates it against the embedded v1
// schema, prints any violations in the same format the Python
// implementation uses, exits non-zero on any violation.
//
// The output format is deliberately matched to
// tracebloc_ingestor.cli.run._format_errors so a customer's editor
// or CI can grep both implementations' output uniformly. See
// internal/schema/validate.go for the formatting contract.
func newIngestValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate <path>",
		Short: "Validate an ingest.yaml against the embedded v1 schema, locally",
		Long: `Reads <path>, parses it as YAML, and validates it against the bundled
ingest.v1.json schema (synced from tracebloc/data-ingestors). Prints
violations in the same JSON-pointer-prefixed format the cluster's
jobs-manager uses, and exits non-zero if any are found.

Useful as a pre-flight before ` + "`tracebloc dataset push`" + ` lands in a
future phase; for now, customers running the Helm chart can validate
their ` + "`ingest.yaml`" + ` before invoking ` + "`helm install`" + `, getting
millisecond local feedback instead of a multi-second cluster round
trip.

Exit codes:
  0   YAML parses and validates cleanly
  2   YAML parses but has schema violations (printed to stderr)
  3   YAML doesn't parse or file isn't readable`,
		Args: cobra.ExactArgs(1),
		RunE: runIngestValidate,
	}
	return cmd
}

func runIngestValidate(cmd *cobra.Command, args []string) error {
	path := args[0]

	body, err := os.ReadFile(path)
	if err != nil {
		// fileError is exit-code 3 territory. We use a sentinel
		// exit-coded error so cobra propagates the right code via
		// the main()-side os.Exit mapping (in a follow-up commit
		// we'll wire main.go to inspect for these).
		return &exitError{code: 3, err: fmt.Errorf("reading %s: %w", path, err)}
	}

	v, err := schema.NewV1Validator()
	if err != nil {
		// Schema-compilation failures are infrastructure-side, not
		// customer-side — we bundle the schema, so this only fires
		// if the build is broken. Treat as exit-code-2 so CI can
		// distinguish from a customer file problem.
		return &exitError{code: 2, err: fmt.Errorf("loading embedded schema: %w", err)}
	}

	_, violations, parseErr := v.ValidateYAML(body)
	if parseErr != nil {
		return &exitError{code: 3, err: fmt.Errorf("%s: %w", path, parseErr)}
	}

	if len(violations) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: ok\n", path)
		return nil
	}

	// Print violations to stderr so success can be piped without
	// interference; error lines are diagnostic, not data.
	fmt.Fprintf(cmd.ErrOrStderr(), "%s: schema validation failed (%d issue%s)\n",
		path, len(violations), plural(len(violations)))
	fmt.Fprintln(cmd.ErrOrStderr(), schema.FormatErrors(violations))
	return &exitError{code: 2, err: nil} // err==nil so cobra doesn't print "Error: ..." on top
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// exitError carries a process exit code alongside (or instead of)
// an error message. main.go inspects for this type before calling
// os.Exit, mapping the code through. Other handlers can opt in to
// specific exit codes by returning an exitError.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

// Code returns the process exit code main() should propagate.
func (e *exitError) Code() int { return e.code }
