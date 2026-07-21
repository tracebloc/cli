package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// runDataListArgs is the resolved input to runDataList — same
// shape convention as the other data verbs, keeping the RunE a thin
// flag-to-struct adapter.
type runDataListArgs struct {
	Kubeconfig string
	Context    string
	Namespace  string
	OutputJSON bool
	Printer    *ui.Printer
	JSONOut    io.Writer
}

// newDataListCmd implements `tracebloc data list` — a read-only
// listing of the datasets ingested into the cluster. The kubeconfig
// flags are all zero-value-safe, so the minimal `tracebloc data list`
// runs against the current context + its namespace; the flags only
// override that (same convention as `cluster info`).
func newDataListCmd() *cobra.Command {
	var (
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
		outputJSON      bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List datasets ingested in the cluster",
		Long: `Lists the datasets ingested into your client —
the tables in ` + push.IngestionDatabase + ` on the cluster.

With no flags it uses your current kubeconfig context and its namespace;
the flags below override that, same as ` + "`cluster info`" + ` and ` + "`data ingest`" + `.
For the full catalog (with metadata), see the dashboard at
https://ai.tracebloc.io/metadata.

Exit codes:
  0  listed successfully (including an empty list)
  3  kubeconfig error
  4  cluster reachable but no tracebloc client in the namespace
  7  couldn't query the cluster for datasets`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// In --output-json mode, human output (the header + listing) goes
			// to stderr so stdout carries only the JSON — same split as ingest.
			printer := printerFor(cmd)
			var jsonOut io.Writer
			if outputJSON {
				printer = printerForWriter(cmd, cmd.ErrOrStderr())
				jsonOut = cmd.OutOrStdout()
			}
			return runDataList(cmd.Context(), runDataListArgs{
				Kubeconfig: kubeconfigPath,
				Context:    contextOverride,
				Namespace:  nsOverride,
				OutputJSON: outputJSON,
				Printer:    printer,
				JSONOut:    jsonOut,
			})
		},
	}

	addKubeconfigFlags(cmd, &kubeconfigPath, &contextOverride, kubeconfigFlagUsage, contextFlagUsage)
	addNamespaceFlag(cmd, &nsOverride, namespaceFlagUsage)
	cmd.Flags().BoolVar(&outputJSON, "output-json", false,
		"emit the dataset list as JSON on stdout (human output → stderr)")

	return cmd
}

// runDataList discovers the cluster, enumerates the ingested tables,
// and renders them. Mirrors the other data verbs' discovery so the
// exit-code contract is consistent.
func runDataList(ctx context.Context, a runDataListArgs) (err error) {
	// In --output-json mode, guarantee stdout always carries JSON: the
	// success path emits the listing and sets jsonEmitted; this defer
	// covers the early-failure returns (kubeconfig, no release, query)
	// with a JSON error object, mirroring data ingest. (Bugbot #53)
	jsonEmitted := false
	defer func() {
		if a.OutputJSON && err != nil && !jsonEmitted {
			code := 1
			var ee *exitError
			if errors.As(err, &ee) {
				code = ee.Code()
			}
			writeDataListErrorJSON(a.JSONOut, err, code)
		}
	}()

	p := a.Printer

	opts := cluster.KubeconfigOptions{Path: a.Kubeconfig, Context: a.Context, Namespace: a.Namespace}
	binding := bindActiveClientNamespace(&opts)
	target, err := resolveClusterTarget(ctx, p, opts, binding, false)
	if err != nil {
		return binding.explain(err)
	}
	resolved, cs, release := target.Resolved, target.Clientset, target.Release

	tables, err := listDatasetsFn(ctx, cs, resolved.RestConfig, resolved.Namespace)
	if err != nil {
		return &exitError{code: exitQueryFailed, err: err}
	}

	if a.OutputJSON {
		writeDataListJSON(a.JSONOut, resolved.Namespace, release.ReleaseName, tables)
		jsonEmitted = true
		return nil
	}
	renderDataList(p, resolved.Namespace, tables)
	return nil
}

// renderDataList prints the human-facing listing. Split out so it's
// unit-testable with a buffer-backed Printer.
func renderDataList(p *ui.Printer, namespace string, tables []string) {
	p.Section(fmt.Sprintf("Datasets in %s (%d)", namespace, len(tables)))
	if len(tables) == 0 {
		p.Newline()
		p.Para(fmt.Sprintf("No datasets yet — ingest one with `%s data ingest`.", invokedName()))
		return
	}
	for _, t := range tables {
		p.Infof("%s", t)
	}
}

// dataListJSON is the --output-json shape (owned by the CLI layer).
type dataListJSON struct {
	Namespace string   `json:"namespace"`
	Release   string   `json:"release"`
	Count     int      `json:"count"`
	Datasets  []string `json:"datasets"`
}

func writeDataListJSON(w io.Writer, namespace, release string, tables []string) {
	if tables == nil {
		tables = []string{} // emit [] not null
	}
	res := dataListJSON{
		Namespace: namespace,
		Release:   release,
		Count:     len(tables),
		Datasets:  tables,
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, string(b))
}

// writeDataListErrorJSON emits a minimal JSON error object for
// --output-json runs that fail before the listing is produced, so
// stdout is never empty on failure (parallels data ingest). (Bugbot #53)
func writeDataListErrorJSON(w io.Writer, e error, code int) {
	res := struct {
		Status   string `json:"status"`
		Error    string `json:"error"`
		ExitCode int    `json:"exit_code"`
	}{Status: "error", Error: e.Error(), ExitCode: code}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, string(b))
}
