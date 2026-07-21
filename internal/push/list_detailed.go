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
// assembled from read-only queries against the mysql pod: an information_schema
// pass (name/db-size/create-time/columns), a per-table data pass
// (intent/record-count/classes/extension), and the ingest-run journal (task).
// Everything here already exists in the cluster — no backend round-trip.
type DatasetInfo struct {
	Name        string   // table name = dataset name
	Intent      string   // "train" / "test" / "" (from the data_intent column)
	Task        string   // the ingest task/category (e.g. image_classification) from the run journal; "" if not recorded (pre-persistence datasets)
	Records     int64    // COUNT(*) — images / documents / rows, per modality
	Classes     int64    // COUNT(DISTINCT label); 0 when unlabelled
	Extension   string   // per-row file extension (jpg/png/txt); "" for CSV tasks
	SizeBytes   int64    // dataset size: du of the shared PVC for file datasets, else the DB data_length; 0 if unavailable
	DBBytes     int64    // information_schema.data_length — the size source for row-based (non-file) datasets
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
	applyDatasetSizes(infos, datasetSizesFromShared(ctx, exec, cs, namespace))
	return infos, nil
}

// applyDatasetSizes picks each dataset's size from where its data actually
// lives. File-bearing datasets keep their bytes on the shared PVC, so the real
// size is the du of that PVC (duSizes) — the DB holds only metadata rows.
// Row-based datasets (tabular / time-series) live entirely in the table, so
// their information_schema.data_length (DBBytes) IS the size.
//
// The file-vs-row signal is the per-row file Extension, NOT a `filename`
// column: filename/extension/annotation are framework columns present on EVERY
// dataset table (tabular included), so only a non-empty extension actually
// marks staged files. du is best-effort: a file dataset with no du entry
// (jobs-manager unreachable) shows no size rather than the misleading
// metadata-row DBBytes.
//
// DBBytes is used only for row-based datasets that actually have rows. An empty
// table (0 records) — whether a row-based or a file dataset whose ingest landed
// nothing — has an empty extension too, but its data_length is just InnoDB's
// one-page allocation, not real data; leave it sizeless (rendered "—", matching
// its ⚠ empty flag) rather than implying it holds a page of data.
func applyDatasetSizes(infos []DatasetInfo, duSizes map[string]int64) {
	for i := range infos {
		if b, ok := duSizes[infos[i].Name]; ok {
			infos[i].SizeBytes = b // file dataset: real PVC size
		} else if infos[i].Extension == "" && infos[i].Records > 0 {
			infos[i].SizeBytes = infos[i].DBBytes // row-based with rows: DB data_length
		}
	}
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
	// data_length is fetched but used ONLY for row-based datasets (tabular /
	// time-series), whose data really lives in the table. For a file-bearing
	// dataset it's just the metadata rows (images/text live on the PVC) and for
	// a tiny table it's InnoDB's padded page allocation — both misleading — so
	// those fall back to a `du` of the PVC in ListDatasetsDetailed.
	// Raise group_concat_max_len (default 1024) so a wide table's column list
	// isn't truncated mid-name — that would under-count feature columns and drop
	// late modality markers. Runs as a leading statement over the same stdin.
	schemaQ := "SET SESSION group_concat_max_len = 1048576; " +
		"SELECT t.table_name," +
		" COALESCE(MAX(UNIX_TIMESTAMP(t.create_time)),0)," + // tz-safe epoch — sole time SoT
		" COALESCE(MAX(t.data_length),0)," + // DB size — used for row-based datasets
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
	applyTaskLabels(ctx, exec, namespace, pod, container, infos)
	return infos, nil
}

// applyTaskLabels tags each dataset with its ingest task from the run journal
// (tracebloc_ingest_runs.task, persisted by data-ingestors). Best-effort
// enrichment: a cluster whose ingestor predates the task column, or a dataset
// ingested before it shipped, keeps an empty Task and the caller falls back to
// inferred modality. Guarded on the column's presence — known from the schema
// pass — so a pre-persistence journal is never queried for a column it lacks.
func applyTaskLabels(ctx context.Context, exec Executor, namespace, pod, container string, infos []DatasetInfo) {
	hasTaskColumn := false
	for i := range infos {
		if infos[i].Name == ingestRunsTable && hasColumn(infos[i].Columns, "task") {
			hasTaskColumn = true
			break
		}
	}
	if !hasTaskColumn {
		return
	}
	out, err := runMySQLQuery(ctx, exec, namespace, pod, container, fmt.Sprintf(
		"SELECT table_name, COALESCE(MAX(task),'') FROM `%s`.`%s` "+
			"WHERE task IS NOT NULL GROUP BY table_name",
		IngestionDatabase, ingestRunsTable))
	if err != nil {
		return // best-effort: the listing still renders with inferred modality
	}
	tasks := parseTaskRows(out)
	for i := range infos {
		if t, ok := tasks[infos[i].Name]; ok {
			infos[i].Task = t
		}
	}
}

// parseTaskRows parses the "table_name\ttask" TSV into a name→task map, skipping
// blank tasks (a run journalled by a pre-persistence ingestor).
func parseTaskRows(raw string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		if task := strings.TrimSpace(f[1]); task != "" {
			m[strings.TrimSpace(f[0])] = task
		}
	}
	return m
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
// (name, create-time epoch, DB size, columns). Malformed/short lines are skipped.
func parseSchemaRows(raw string) []DatasetInfo {
	var out []DatasetInfo
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		d := DatasetInfo{Name: strings.TrimSpace(f[0])}
		d.CreatedUnix, _ = strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		d.DBBytes, _ = strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64)
		if cols := strings.TrimSpace(f[3]); cols != "" {
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
