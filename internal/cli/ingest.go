package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/schema"
)

// newIngestCmd implements the `tracebloc ingest` subtree — kept as a HIDDEN
// alias for one deprecation cycle: its only verb, `validate`, moved to
// `tracebloc data validate`, so the top level no longer carries two different
// things named "ingest" (`ingest validate` vs `data ingest`). Scripts using
// the old path keep working; help and docs point at the new one.
func newIngestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "ingest",
		Short:  "Deprecated alias for `tracebloc data validate`",
		Hidden: true,
		// A bare path here is almost always someone meaning `data ingest`
		// (the home screen advertises it) — redirect instead of dumping a
		// confusing deprecation usage block.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf(
					"`tracebloc ingest` doesn't stage datasets — did you mean:\n"+
						"    tracebloc data ingest %s",
					strings.Join(args, " "))
			}
			return cmd.Help()
		},
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

Useful as a pre-flight before running ` + "`tracebloc data ingest`" + ` —
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
		return &exitError{code: exitLocalEnv, err: fmt.Errorf("reading %s: %w", path, err)}
	}

	v, err := schema.NewV1Validator()
	if err != nil {
		// Schema-compilation failures are infrastructure-side, not
		// customer-side — we bundle the schema, so this only fires
		// if the build is broken. Treat as exit-code-2 so CI can
		// distinguish from a customer file problem.
		return &exitError{code: exitBadInput, err: fmt.Errorf("loading embedded schema: %w", err)}
	}

	doc, violations, parseErr := v.ValidateYAML(body)
	if parseErr != nil {
		return &exitError{code: exitLocalEnv, err: fmt.Errorf("%s: %w", path, parseErr)}
	}

	// The jsonschema types every `schema` value as a bare string, so a bogus
	// SQL type (e.g. {age: BANANA}) passes it — a false green that only fails
	// in-cluster at CREATE TABLE. `data validate` is a local preview of what
	// the cluster accepts, so mirror the ingestor's accepted-type set here too
	// (cli#213), reusing the SAME push.ValidateSchemaType the --schema flag
	// path uses so the two can't diverge.
	violations = append(violations, schemaTypeViolations(doc)...)

	if len(violations) == 0 {
		// Explicit discard: Fprintf returns an error when the
		// underlying writer fails (closed pipe, etc.). For the
		// success summary we'd rather still exit 0 even if the
		// downstream consumer dropped the connection — they got
		// what they needed (the exit code), and propagating a
		// pipe-write error would convert success into failure for
		// reasons unrelated to validation.
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: ok\n", path)
		return nil
	}

	// Print violations to stderr so success can be piped without
	// interference; error lines are diagnostic, not data. Same
	// pipe-write rationale as the ok-path above: don't let a
	// stderr-write failure mask the real exit-2 schema-violation
	// signal.
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: schema validation failed (%d issue%s)\n",
		path, len(violations), plural(len(violations)))
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), schema.FormatErrors(violations))
	return &exitError{code: exitBadInput, err: nil} // err==nil so cobra doesn't print "Error: ..." on top
}

// schemaTypeViolations previews the ingestor's accepted-SQL-type check over a
// parsed ingest doc's `schema` block (cli#213). It returns one ValidationError
// per column whose declared type the ingestor would reject, anchored at
// "schema.<column>" so it renders in the same JSON-pointer format as every
// other violation. A missing / non-map schema, or a non-string value, is left
// to the jsonschema layer — this only adds the type-vocabulary check the
// schema itself can't express.
func schemaTypeViolations(doc map[string]any) []schema.ValidationError {
	if doc == nil {
		return nil
	}
	sch, ok := doc["schema"].(map[string]any)
	if !ok {
		return nil
	}
	cols := make([]string, 0, len(sch))
	for col := range sch {
		cols = append(cols, col)
	}
	sort.Strings(cols) // deterministic order (FormatErrors re-sorts, but keep it stable)
	var out []schema.ValidationError
	for _, col := range cols {
		typ, ok := sch[col].(string)
		if !ok {
			continue
		}
		if err := push.ValidateSchemaType(col, typ); err != nil {
			out = append(out, schema.ValidationError{
				Path:    "schema." + col,
				Message: err.Error(),
			})
		}
	}
	return out
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
