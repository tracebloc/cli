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
	Name      string   // table name = dataset name
	Intent    string   // "train" / "test" / "" (from the data_intent column)
	Records   int64    // COUNT(*) — images / documents / rows, per modality
	Classes   int64    // COUNT(DISTINCT label); 0 when unlabelled
	Extension string   // per-row file extension (jpg/png/txt); "" for CSV tasks
	SizeBytes int64    // data_length + index_length
	CreatedAt string   // table create_time, "YYYY-MM-DDTHH:MM:SS" (empty if unknown)
	Columns   []string // all column names — drives modality inference
	System    bool     // a framework table (no data_id), e.g. the ingest-run journal
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
	if err := exec.Exec(ctx, namespace, pod, container,
		[]string{"sh", "-c", "du -sk " + SharedRoot + "/* 2>/dev/null"},
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
// the exec + parse path is unit-testable with a fake Executor.
func listDatasetsDetailedWith(ctx context.Context, exec Executor, namespace, pod, container string) ([]DatasetInfo, error) {
	// Size is NOT taken from information_schema: for a file-bearing dataset the
	// DB holds only metadata rows (the images/text live on the shared PVC), and
	// for a tiny table InnoDB reports its padded page allocation, not the logical
	// size — both misleading. Real sizes come from a `du` of the PVC below.
	schemaQ := "SELECT t.table_name," +
		" COALESCE(MAX(DATE_FORMAT(t.create_time,'%Y-%m-%dT%H:%i:%s')),'')," +
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
		if !hasColumn(d.Columns, "data_id") {
			d.System = true
			continue
		}
		if !identRe.MatchString(d.Name) {
			continue // never interpolate a non-identifier table name
		}
		// Qualify the table with the schema: mysql runs without -D (so an
		// empty cluster with no ingestion DB still lists cleanly), and a bare
		// table reference would fail with "No database selected".
		selects = append(selects, fmt.Sprintf(
			"SELECT '%s',COALESCE(MAX(data_intent),''),COUNT(*),COUNT(DISTINCT label),COALESCE(MAX(extension),'') FROM `%s`.`%s`",
			d.Name, IngestionDatabase, d.Name))
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
// (name, size, create-time, columns). Malformed/short lines are skipped.
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
		d := DatasetInfo{Name: strings.TrimSpace(f[0]), CreatedAt: strings.TrimSpace(f[1])}
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
