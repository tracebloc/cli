package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// listDatasetsDetailedFn is the test seam over push.ListDatasetsDetailed —
// same fn-var convention as listDatasetsFn / loadClusterFn.
var listDatasetsDetailedFn = push.ListDatasetsDetailed

// runDataListArgs is the resolved input to runDataList — a thin
// flag-to-struct adapter, same shape as the other data verbs.
type runDataListArgs struct {
	Kubeconfig string
	Context    string
	Namespace  string
	ShowAll    bool
	OutputJSON bool
	Printer    *ui.Printer
	JSONOut    io.Writer
}

// newDataListCmd implements `tracebloc data list` — a read-only listing of the
// datasets ingested into the cluster, with per-dataset size, record count,
// format, split, and freshness. The kubeconfig flags are zero-value-safe, so
// the minimal `tracebloc data list` runs against the current context + its
// namespace (same convention as `cluster info`).
func newDataListCmd() *cobra.Command {
	var (
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
		showAll         bool
		outputJSON      bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List datasets ingested in the cluster, with size / records / format",
		Long: `Lists the datasets ingested into your client — the tables in ` + push.IngestionDatabase + `
on the cluster — grouped by modality, with each dataset's split (train/test),
record count, size, format, and when it was ingested.

With no flags it uses your current kubeconfig context and its namespace;
the flags below override that, same as ` + "`cluster info`" + ` and ` + "`data ingest`" + `.
Framework tables (the ingest-run journal) are hidden unless you pass --all.
For the full catalog, see the dashboard at https://ai.tracebloc.io/metadata.

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
				ShowAll:    showAll,
				OutputJSON: outputJSON,
				Printer:    printer,
				JSONOut:    jsonOut,
			})
		},
	}

	addKubeconfigFlags(cmd, &kubeconfigPath, &contextOverride, kubeconfigFlagUsage, contextFlagUsage)
	addNamespaceFlag(cmd, &nsOverride, namespaceFlagUsage)
	cmd.Flags().BoolVar(&showAll, "all", false,
		"include framework/system tables (e.g. the ingest-run journal), normally hidden")
	cmd.Flags().BoolVar(&outputJSON, "output-json", false,
		"emit the dataset list as JSON on stdout (human output → stderr)")

	return cmd
}

// runDataList discovers the cluster, enumerates the ingested datasets with
// their metadata, and renders them. Mirrors the other data verbs' discovery so
// the exit-code contract is consistent.
func runDataList(ctx context.Context, a runDataListArgs) (err error) {
	// In --output-json mode, guarantee stdout always carries JSON: the
	// success path emits the listing; this defer covers the early-failure
	// returns with a JSON error object, mirroring data ingest. (Bugbot #53)
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

	infos, err := listDatasetsDetailedFn(ctx, cs, resolved.RestConfig, resolved.Namespace)
	if err != nil {
		return &exitError{code: exitQueryFailed, err: err}
	}

	if a.OutputJSON {
		writeDataListJSON(a.JSONOut, resolved.Namespace, release.ReleaseName, infos, a.ShowAll)
		jsonEmitted = true
		return nil
	}
	renderDataList(p, resolved.Namespace, infos, a.ShowAll)
	return nil
}

// modalityOrder is the fixed display order of the modality groups.
var modalityOrder = []string{"Image", "Text", "Tabular", "Time-series", "Other"}

// renderDataList prints the human-facing listing: a summary line, then the
// datasets grouped by modality with per-dataset detail. Split out so it's
// unit-testable with a buffer-backed Printer.
func renderDataList(p *ui.Printer, namespace string, infos []push.DatasetInfo, showAll bool) {
	var shown, system []push.DatasetInfo
	var totalBytes int64
	for _, d := range infos {
		if d.System {
			system = append(system, d)
			continue
		}
		shown = append(shown, d)
		totalBytes += d.SizeBytes
	}

	if len(shown) == 0 && !(showAll && len(system) > 0) {
		p.Section(fmt.Sprintf("Datasets in %s (0)", namespace))
		p.Newline()
		p.Para(fmt.Sprintf("No datasets yet — ingest one with `%s data ingest`.", invokedName()))
		if len(system) > 0 && !showAll {
			p.Hintf("%d system table(s) hidden — show with --all.", len(system))
		}
		return
	}

	header := fmt.Sprintf("Datasets in %s — %d", namespace, len(shown))
	if totalBytes > 0 {
		header += " · " + push.HumanBytes(totalBytes)
	}
	p.Section(header)
	if len(system) > 0 && !showAll {
		p.Hintf("%d system table(s) hidden — show with --all.", len(system))
	}

	// Column widths sized to the shown rows (name + format are variable).
	nameW, fmtW := 8, 10
	groups := map[string][]push.DatasetInfo{}
	for _, d := range shown {
		m := datasetModality(d)
		groups[m] = append(groups[m], d)
		if l := len(d.Name); l > nameW {
			nameW = l
		}
		if l := len(formatCell(d, m)); l > fmtW {
			fmtW = l
		}
	}
	if nameW > 24 {
		nameW = 24
	}
	if fmtW > 28 {
		fmtW = 28
	}

	for _, m := range modalityOrder {
		ds := groups[m]
		if len(ds) == 0 {
			continue
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i].Name < ds[j].Name })
		p.Section(fmt.Sprintf("%s · %d", m, len(ds)))
		for _, d := range ds {
			p.Para(datasetRow(d, m, nameW, fmtW))
		}
	}

	if showAll && len(system) > 0 {
		sort.Slice(system, func(i, j int) bool { return system[i].Name < system[j].Name })
		p.Section(fmt.Sprintf("System · %d", len(system)))
		for _, d := range system {
			size := "—" // system tables aren't du-sized; don't imply a measured 0 B
			if d.SizeBytes > 0 {
				size = push.HumanBytes(d.SizeBytes)
			}
			p.Para(fmt.Sprintf("· %-*s  %9s", nameW, d.Name, size))
		}
	}
}

// datasetRow formats one dataset as a fixed-width row: status glyph, name,
// split, record count (with the modality's noun), size, format, and freshness.
func datasetRow(d push.DatasetInfo, modality string, nameW, fmtW int) string {
	glyph := "✔"
	if d.Records == 0 {
		glyph = "⚠" // ingested-but-empty (e.g. an ingest that dropped every record)
	}
	name := d.Name
	if len(name) > nameW {
		name = name[:nameW-1] + "…"
	}
	split := d.Intent
	if split == "" {
		split = "—"
	}
	size := "—" // du unavailable / unknown
	if d.SizeBytes > 0 {
		size = push.HumanBytes(d.SizeBytes)
	}
	return fmt.Sprintf("%s %-*s  %-5s  %-12s  %9s  %-*s  %s",
		glyph, nameW, name, split, recordsCell(d, modality), size,
		fmtW, formatCell(d, modality), relativeTime(d.CreatedUnix))
}

// frameworkCols are the columns the ingestor adds to every dataset table; the
// rest are the user's schema columns (used for the "N cols" format hint and to
// detect real datasets vs framework tables).
var frameworkCols = map[string]bool{
	"id": true, "created_at": true, "updated_at": true, "status": true,
	"label": true, "data_intent": true, "data_id": true, "filename": true,
	"extension": true, "annotation": true, "ingestor_id": true,
}

// datasetModality infers the modality family from the on-disk shape: the file
// extension for file-bearing tasks, else the presence of time/sequence columns.
// Best-effort — the specific task isn't stored in the cluster DB.
func datasetModality(d push.DatasetInfo) string {
	switch strings.ToLower(d.Extension) {
	case "jpg", "jpeg", "png":
		return "Image"
	case "txt", "text": // the ingestor accepts both .txt and .text
		return "Text"
	}
	has := func(name string) bool {
		for _, c := range d.Columns {
			if strings.EqualFold(strings.TrimSpace(c), name) {
				return true
			}
		}
		return false
	}
	if has("sequence_id") || has("timestamp") || (has("time") && has("event")) {
		return "Time-series"
	}
	// A populated dataset with user-schema columns is tabular. Require records:
	// an empty (0-row) table has NULL extension/label, so its modality is
	// genuinely unknowable — it falls to "Other" rather than a wrong guess (an
	// empty image/semseg/keypoint table would otherwise look tabular).
	if d.Records > 0 && featureColCount(d.Columns) > 0 {
		return "Tabular"
	}
	return "Other"
}

// featureColCount is the number of user-schema columns (all columns minus the
// framework-managed ones).
func featureColCount(cols []string) int {
	n := 0
	for _, c := range cols {
		if !frameworkCols[strings.ToLower(strings.TrimSpace(c))] {
			n++
		}
	}
	return n
}

// recordsCell renders the record count with the modality's natural noun.
func recordsCell(d push.DatasetInfo, modality string) string {
	noun := "rows"
	switch modality {
	case "Image":
		noun = "images"
	case "Text":
		noun = "documents"
	}
	return fmt.Sprintf("%d %s", d.Records, noun)
}

// formatCell renders the format hint: the file extension for file-bearing
// tasks, or "csv · N cols" for tabular/time-series, plus "· N classes" when the
// dataset is labelled.
func formatCell(d push.DatasetInfo, modality string) string {
	var base string
	switch modality {
	case "Image", "Text":
		base = strings.ToLower(d.Extension)
		if base == "" {
			base = "files"
		}
	case "Tabular", "Time-series":
		base = fmt.Sprintf("csv · %d cols", featureColCount(d.Columns))
	default:
		// Undetermined modality (e.g. an empty table) — don't imply "csv".
		return "—"
	}
	// Show classes only when the label actually repeats (classes < records):
	// a continuous regression target has ~one distinct value per row, which is
	// not a class count. COUNT(DISTINCT label) can't tell the two apart, so this
	// guard keeps "N classes" to genuinely categorical datasets.
	if d.Classes >= 2 && d.Classes < d.Records {
		base += fmt.Sprintf(" · %d classes", d.Classes)
	}
	return base
}

// relativeTime renders a UTC epoch (the table's create_time via UNIX_TIMESTAMP,
// which is tz-safe regardless of the MySQL session clock) as a coarse "Xh ago".
// Zero/unknown → an em dash; a future timestamp (clock skew) → "just now".
func relativeTime(epoch int64) string {
	if epoch <= 0 {
		return "—"
	}
	d := time.Since(time.Unix(epoch, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ── JSON output (owned by the CLI layer) ──

type datasetJSON struct {
	Name      string `json:"name"`
	Modality  string `json:"modality"`
	Intent    string `json:"intent,omitempty"`
	Records   int64  `json:"records"`
	Classes   int64  `json:"classes,omitempty"`
	Format    string `json:"format"`
	SizeBytes int64  `json:"size_bytes"`
	Ingested  string `json:"ingested,omitempty"`
	System    bool   `json:"system,omitempty"`
}

type dataListJSON struct {
	Namespace string        `json:"namespace"`
	Release   string        `json:"release"`
	Count     int           `json:"count"`
	Datasets  []string      `json:"datasets"` // names — type unchanged (additive-only JSON contract)
	Details   []datasetJSON `json:"details"`  // per-dataset metadata added by the rich listing
}

func writeDataListJSON(w io.Writer, namespace, release string, infos []push.DatasetInfo, showAll bool) {
	names := []string{}
	details := []datasetJSON{}
	for _, d := range infos {
		if d.System && !showAll {
			continue
		}
		m := datasetModality(d)
		names = append(names, d.Name)
		details = append(details, datasetJSON{
			Name:      d.Name,
			Modality:  m,
			Intent:    d.Intent,
			Records:   d.Records,
			Classes:   d.Classes,
			Format:    formatCell(d, m),
			SizeBytes: d.SizeBytes,
			Ingested:  d.CreatedAt,
			System:    d.System,
		})
	}
	res := dataListJSON{
		Namespace: namespace,
		Release:   release,
		Count:     len(names),
		Datasets:  names,
		Details:   details,
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, string(b))
}

// writeDataListErrorJSON emits a minimal JSON error object for --output-json
// runs that fail before the listing is produced, so stdout is never empty on
// failure (parallels data ingest). (Bugbot #53)
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
