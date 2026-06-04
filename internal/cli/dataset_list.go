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

// runDatasetListArgs is the resolved input to runDatasetList — same
// shape convention as the other dataset verbs, keeping the RunE a thin
// flag-to-struct adapter.
type runDatasetListArgs struct {
	Kubeconfig string
	Context    string
	Namespace  string
	OutputJSON bool
	Printer    *ui.Printer
	JSONOut    io.Writer
}

// newDatasetListCmd implements `tracebloc dataset list` — a read-only
// listing of the datasets ingested into the cluster. The kubeconfig
// flags are all zero-value-safe, so the minimal `tracebloc dataset list`
// runs against the current context + its namespace; the flags only
// override that (same convention as `cluster info`).
func newDatasetListCmd() *cobra.Command {
	var (
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
		outputJSON      bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List datasets ingested in the cluster",
		Long: `Lists the datasets pushed + ingested into the parent client release —
the tables in ` + push.IngestionDatabase + ` on the cluster.

With no flags it uses your current kubeconfig context and its namespace;
the flags below override that, same as ` + "`cluster info`" + ` and ` + "`dataset push`" + `.
For the full catalog (with metadata), see the dashboard at
https://ai.tracebloc.io/metadata.

Exit codes:
  0  listed successfully (including an empty list)
  3  kubeconfig error
  4  cluster reachable but no parent release in the namespace
  7  couldn't query the cluster for datasets`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// In --output-json mode, human output (the banner) goes to
			// stderr so stdout carries only the JSON — same split as push.
			printer := printerFor(cmd)
			var jsonOut io.Writer
			if outputJSON {
				printer = printerForWriter(cmd, cmd.ErrOrStderr())
				jsonOut = cmd.OutOrStdout()
			}
			return runDatasetList(cmd.Context(), runDatasetListArgs{
				Kubeconfig: kubeconfigPath,
				Context:    contextOverride,
				Namespace:  nsOverride,
				OutputJSON: outputJSON,
				Printer:    printer,
				JSONOut:    jsonOut,
			})
		},
	}

	cmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	cmd.Flags().StringVar(&contextOverride, "context", "",
		"name of the kubeconfig context to use (default: kubeconfig's current-context)")
	cmd.Flags().StringVarP(&nsOverride, "namespace", "n", "",
		"namespace where the parent tracebloc/client release is installed")
	cmd.Flags().BoolVar(&outputJSON, "output-json", false,
		"emit the dataset list as JSON on stdout (human output → stderr)")

	return cmd
}

// runDatasetList discovers the cluster, enumerates the ingested tables,
// and renders them. Mirrors the other dataset verbs' discovery so the
// exit-code contract is consistent.
func runDatasetList(ctx context.Context, a runDatasetListArgs) (err error) {
	// In --output-json mode, guarantee stdout always carries JSON: the
	// success path emits the listing and sets jsonEmitted; this defer
	// covers the early-failure returns (kubeconfig, no release, query)
	// with a JSON error object, mirroring dataset push. (Bugbot #53)
	jsonEmitted := false
	defer func() {
		if a.OutputJSON && err != nil && !jsonEmitted {
			code := 1
			var ee *exitError
			if errors.As(err, &ee) {
				code = ee.Code()
			}
			writeDatasetListErrorJSON(a.JSONOut, err, code)
		}
	}()

	p := a.Printer
	p.Banner("tracebloc", "datasets in the cluster")

	resolved, err := cluster.Load(cluster.KubeconfigOptions{
		Path:      a.Kubeconfig,
		Context:   a.Context,
		Namespace: a.Namespace,
	})
	if err != nil {
		return &exitError{code: 3, err: fmt.Errorf("loading kubeconfig: %w", err)}
	}
	cs, err := cluster.NewClientset(resolved)
	if err != nil {
		return &exitError{code: 3, err: err}
	}
	release, err := cluster.DiscoverParentRelease(ctx, cs, resolved.Namespace)
	if err != nil {
		return &exitError{code: 4, err: err}
	}

	tables, err := push.ListDatasets(ctx, cs, resolved.RestConfig, resolved.Namespace)
	if err != nil {
		return &exitError{code: 7, err: err}
	}

	if a.OutputJSON {
		writeDatasetListJSON(a.JSONOut, resolved.Namespace, release.ReleaseName, tables)
		jsonEmitted = true
		return nil
	}
	renderDatasetList(p, resolved.Namespace, tables)
	return nil
}

// renderDatasetList prints the human-facing listing. Split out so it's
// unit-testable with a buffer-backed Printer.
func renderDatasetList(p *ui.Printer, namespace string, tables []string) {
	p.Section(fmt.Sprintf("Datasets in %s (%d)", namespace, len(tables)))
	if len(tables) == 0 {
		p.Infof("No datasets yet — push one with `tracebloc dataset push`.")
		return
	}
	for _, t := range tables {
		p.Infof("%s", t)
	}
}

// datasetListJSON is the --output-json shape (owned by the CLI layer).
type datasetListJSON struct {
	Namespace string   `json:"namespace"`
	Release   string   `json:"release"`
	Count     int      `json:"count"`
	Datasets  []string `json:"datasets"`
}

func writeDatasetListJSON(w io.Writer, namespace, release string, tables []string) {
	if tables == nil {
		tables = []string{} // emit [] not null
	}
	res := datasetListJSON{
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

// writeDatasetListErrorJSON emits a minimal JSON error object for
// --output-json runs that fail before the listing is produced, so
// stdout is never empty on failure (parallels dataset push). (Bugbot #53)
func writeDatasetListErrorJSON(w io.Writer, e error, code int) {
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
