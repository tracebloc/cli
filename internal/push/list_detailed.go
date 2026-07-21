package push

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// DatasetInfo is one dataset's metadata for the rich `data list` view. It is
// assembled from two read-only queries against the mysql pod: an
// information_schema pass (name/size/create-time/columns) and a per-table data
// pass (intent/record-count/classes/extension). Everything here already exists
// in the ingested tables — no backend round-trip.
type DatasetInfo struct {
	Name        string   // table name = dataset name
	Intent      string   // "train" / "test" / "" (from the data_intent column)
	Records     int64    // COUNT(*) — images / documents / rows, per modality
	Classes     int64    // COUNT(DISTINCT label); 0 when unlabelled
	Extension   string   // per-row file extension (jpg/png/txt); "" for CSV tasks
	SizeBytes   int64    // real dataset size from a du of the shared PVC; 0 if unavailable
	CreatedUnix int64    // create_time as a UTC epoch (tz-safe) — the sole time SoT, for both "ago" and JSON
	Columns     []string // all column names — drives modality inference
	System      bool     // a framework table (no data_id), e.g. the ingest-run journal
}

// identRe guards table names before they are interpolated into the data query.
// Ingest already restricts dataset names to this shape; the guard is defence in
// depth so a surprising name can never break out of the SQL.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ListDatasetsDetailed returns per-dataset metadata for the rich listing,
// reusing the same mysql-pod exec seam + pod discovery as ListDatasets.
func ListDatasetsDetailed(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, namespace string) ([]DatasetInfo, error) {
	mysqlPod, mysqlContainer, err := findRunningPod(ctx, cs, namespace, "mysql")
	if err != nil {
		return nil, fmt.Errorf("locating mysql pod: %w", err)
	}
	exec := &SPDYExecutor{Config: cfg, Client: cs}
	infos, err := listDatasetsDetailedWith(ctx, exec, namespace, mysqlPod, mysqlContainer)
	if err != nil {
		return nil, err
	}
	// Real per-dataset sizes come from a du of the shared PVC (where the files
	// live), not the DB. Best-effort: if the jobs-manager pod or the du isn't
	// reachable, the listing still renders — those datasets just show no size.
	if sizes := datasetSizesFromShared(ctx, exec, cs, namespace); sizes != nil {
		for i := range infos {
			if b, ok := sizes[infos[i].Name]; ok {
				infos[i].SizeBytes = b
			}
		}
	}
	return infos, nil
}

// datasetSizesFromShared returns real dataset byte sizes by du-ing the shared
// PVC on the jobs-manager pod (which mounts it). Best-effort: any failure
// yields a nil map and the caller renders without sizes.
func datasetSizesFromShared(ctx context.Context, exec Executor, cs kubernetes.Interface, namespace string) map[string]int64 {
	pod, container, err := findRunningPod(ctx, cs, namespace, "jobs-manager")
	if err != nil {
		return nil
	}
	var stdout, stderr bytes.Buffer
	// `|| true`: du exits non-zero if ANY entry is unreadable (jobs-manager can
	// hit EACCES on some shared-PVC paths), but the readable entries are already
	// on stdout — keep them rather than blanking every dataset's size.
	if err := exec.Exec(ctx, namespace, pod, container,
		[]string{"sh", "-c", "du -sk " + SharedRoot + "/* 2>/dev/null || true"},
		nil, &stdout, &stderr); err != nil {
		return nil
	}
	return parseDuOutput(stdout.String())
}

// parseDuOutput parses `du -sk` output ("<KiB>\t<path>" per line) into a
// name→bytes map keyed by the path's basename (the dataset name).
func parseDuOutput(raw string) map[string]int64 {
	m := map[string]int64{}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line) // "<kb> <path>"; dataset dirs have no spaces
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		m[path.Base(fields[len(fields)-1])] = kb * 1024
	}
	return m
}

// listDatasetsDetailedWith runs the two queries through the given Executor so
// the exec + parse path is unit-testable with a fake Executor. A dataset table
// can be dropped (a concurrent `data delete`) between the schema snapshot and
// the data pass, which would fail the whole UNION ALL; retry the pair once — a
// fresh snapshot drops the vanished table and the rebuilt data query succeeds. A
// genuinely unreachable mysql fails the retry too and the error surfaces.
func listDatasetsDetailedWith(ctx context.Context, exec Executor, namespace, pod, container string) ([]DatasetInfo, error) {
	infos, err := queryDatasetsDetailed(ctx, exec, namespace, pod, container)
	if err != nil {
		infos, err = queryDatasetsDetailed(ctx, exec, namespace, pod, container)
	}
	return infos, err
}

// queryDatasetsDetailed runs the schema pass then the per-table data pass once.
func queryDatasetsDetailed(ctx context.Context, exec Executor, namespace, pod, container string) ([]DatasetInfo, error) {
	// Size is NOT taken from information_schema: for a file-bearing dataset the
	// DB holds only metadata rows (the images/text live on the shared PVC), and
	// for a tiny table InnoDB reports its padded page allocation, not the logical
	// size — both misleading. Real sizes come from a `du` of the PVC below.
	// Raise group_concat_max_len (default 1024) so a wide table's column list
	// isn't truncated mid-name — that would under-count feature columns and drop
	// late modality markers. Runs as a leading statement over the same stdin.
	schemaQ := "SET SESSION group_concat_max_len = 1048576; " +
		"SELECT t.table_name," +
		" COALESCE(MAX(UNIX_TIMESTAMP(t.create_time)),0)," + // tz-safe epoch — sole time SoT
		" COALESCE(GROUP_CONCAT(c.column_name ORDER BY c.ordinal_position SEPARATOR ','),'')" +
		" FROM information_schema.tables t" +
		" LEFT JOIN information_schema.columns c" +
		"  ON c.table_schema=t.table_schema AND c.table_name=t.table_name" +
		" WHERE t.table_schema='" + IngestionDatabase + "'" +
		" GROUP BY t.table_name ORDER BY t.table_name"
	schemaOut, err := runMySQLQuery(ctx, exec, namespace, pod, container, schemaQ)
	if err != nil {
		return nil, err
	}
	infos := parseSchemaRows(schemaOut)
	if len(infos) == 0 {
		return infos, nil
	}

	// Second pass: intent/count/classes/extension for the REAL dataset tables
	// (those with a data_id column). System tables (the run journal, salt store)
	// lack data_id and carry no ingest metadata, so they're marked System and
	// excluded from the data query — selecting data_intent there would error.
	var selects []string
	for i := range infos {
		d := &infos[i]
		// A framework table — the ingest-run journal / ingest-meta store
		// (reservedTables, shared with ListDatasets) or any table lacking a
		// data_id column — carries no dataset rows. Mark it System so it's
		// hidden by default and kept out of the data query below, which selects
		// data_intent (a column these tables don't have).
		if _, reserved := reservedTables[d.Name]; reserved || !hasColumn(d.Columns, "data_id") {
			d.System = true
			continue
		}
		if !identRe.MatchString(d.Name) {
			continue // never interpolate a non-identifier table name
		}
		// Reference only columns this table actually has. `label` is optional —
		// self-supervised tasks (MLM/CLM/seq2seq/embeddings) omit it — and
		// data_intent/extension can be absent on older tables. A missing column
		// would fail the whole UNION ALL and take the entire listing down (exit
		// 7), so fall back to a constant when a column isn't present.
		intentExpr, classesExpr, extExpr := "''", "0", "''"
		if hasColumn(d.Columns, "data_intent") {
			intentExpr = "COALESCE(MAX(data_intent),'')"
		}
		if hasColumn(d.Columns, "label") {
			classesExpr = "COUNT(DISTINCT label)"
		}
		if hasColumn(d.Columns, "extension") {
			extExpr = "COALESCE(MAX(extension),'')"
		}
		// Qualify the table with the schema: mysql runs without -D (so an empty
		// cluster with no ingestion DB still lists cleanly), and a bare table
		// reference would fail with "No database selected".
		selects = append(selects, fmt.Sprintf(
			"SELECT '%s',%s,COUNT(*),%s,%s FROM `%s`.`%s`",
			d.Name, intentExpr, classesExpr, extExpr, IngestionDatabase, d.Name))
	}
	if len(selects) > 0 {
		dataOut, err := runMySQLQuery(ctx, exec, namespace, pod, container, strings.Join(selects, " UNION ALL "))
		if err != nil {
			return nil, err
		}
		applyDataRows(infos, dataOut)
	}
	return infos, nil
}

// runMySQLQuery feeds the query to `mysql -N` over stdin (not -e) so table names
// and string literals — including backtick-quoted identifiers — never pass
// through the shell, sidestepping quoting/injection entirely.
func runMySQLQuery(ctx context.Context, exec Executor, namespace, pod, container, query string) (string, error) {
	var stdout, stderr bytes.Buffer
	script := `mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -N`
	if err := exec.Exec(ctx, namespace, pod, container,
		[]string{"sh", "-c", script}, strings.NewReader(query), &stdout, &stderr); err != nil {
		return "", fmt.Errorf("querying datasets: %w%s", err, stderrSuffix(&stderr))
	}
	return stdout.String(), nil
}

// parseSchemaRows turns the `mysql -N` TSV of the schema query into DatasetInfos
// (name, create-time epoch, columns). Malformed/short lines are skipped.
func parseSchemaRows(raw string) []DatasetInfo {
	var out []DatasetInfo
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		d := DatasetInfo{Name: strings.TrimSpace(f[0])}
		d.CreatedUnix, _ = strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		if cols := strings.TrimSpace(f[2]); cols != "" {
			d.Columns = strings.Split(cols, ",")
		}
		out = append(out, d)
	}
	return out
}

// applyDataRows merges the data query's TSV (name, intent, count, classes, ext)
// back onto the matching DatasetInfo by name.
func applyDataRows(infos []DatasetInfo, raw string) {
	by := make(map[string]*DatasetInfo, len(infos))
	for i := range infos {
		by[infos[i].Name] = &infos[i]
	}
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		d := by[strings.TrimSpace(f[0])]
		if d == nil {
			continue
		}
		d.Intent = strings.TrimSpace(f[1])
		d.Records, _ = strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64)
		d.Classes, _ = strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64)
		// The ingestor stores the extension with a leading dot (".jpg"); drop it
		// so callers can match/display a bare "jpg".
		d.Extension = strings.TrimPrefix(strings.TrimSpace(f[4]), ".")
	}
}

// hasColumn reports whether cols contains name (case-insensitive).
func hasColumn(cols []string, name string) bool {
	for _, c := range cols {
		if strings.EqualFold(strings.TrimSpace(c), name) {
			return true
		}
	}
	return false
}
