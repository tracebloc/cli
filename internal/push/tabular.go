package push

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// reservedColumns are framework-managed columns the ingestor adds
// itself; a user schema must not redeclare them — data-ingestors'
// database.create_table rejects collisions with a clear error. Schema
// auto-inference skips them so a CSV that happens to carry an `id`
// (or data_id, filename, …) column doesn't produce a schema the
// ingestor refuses. `label` is intentionally NOT reserved — it's the
// mapped label column. Mirrors database.py's _RESERVED set.
var reservedColumns = map[string]bool{
	"id":          true,
	"created_at":  true,
	"updated_at":  true,
	"status":      true,
	"data_intent": true,
	"data_id":     true,
	"filename":    true,
	"extension":   true,
	"annotation":  true,
	"ingestor_id": true,
}

// schemaInferenceSampleRows caps how many data rows InferSchema reads
// to decide each column's type. The whole CSV would be more accurate
// but a few thousand rows is plenty to distinguish INT/FLOAT/text in
// practice, and bounds the work for large files. A column whose true
// type only reveals itself past the sample (e.g. an int column that
// turns float on row 10k) is the case --schema exists to override.
const schemaInferenceSampleRows = 5000

// DiscoverTabular validates a local directory for a tabular /
// time-series ingestion. Unlike the image layout, tabular categories
// have NO sidecar files — the dataset IS a single CSV. The directory
// must contain exactly one .csv file; that becomes the labels/data
// CSV staged for the ingestor.
//
// The returned LocalLayout reuses the image layout's LabelsCSV field
// (staged as labels.csv) with an empty Images slice, so the existing
// tar/stream machinery handles it unchanged.
func DiscoverTabular(rootDir string) (*LocalLayout, error) {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolving %q: %w", rootDir, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("reading dataset directory %q: %w", abs, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf(
			"%q is not a directory; pass the directory containing the dataset CSV", abs)
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", abs, err)
	}
	var csvs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".csv") {
			csvs = append(csvs, e.Name())
		}
	}
	sort.Strings(csvs)
	switch len(csvs) {
	case 0:
		return nil, fmt.Errorf(
			"no .csv file found in %q. Tabular / time-series categories expect a "+
				"single CSV holding the dataset (one column per feature, plus the "+
				"label column).", abs)
	case 1:
		// happy path
	default:
		return nil, fmt.Errorf(
			"found %d .csv files in %q (%s); the tabular layout expects exactly one. "+
				"Put the dataset CSV in its own directory and re-run.",
			len(csvs), abs, strings.Join(csvs, ", "))
	}

	csvPath := filepath.Join(abs, csvs[0])
	// Lstat (not Stat) so a symlinked CSV is rejected rather than
	// silently followed — mirrors the image layout's symlink guard.
	info, err := os.Lstat(csvPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", csvs[0], err)
	}
	if err := rejectSymlink(info, csvs[0]); err != nil {
		return nil, err
	}
	if info.Size() > MaxSingleFileBytes {
		return nil, sizeError(csvs[0], info.Size(), MaxSingleFileBytes)
	}

	layout := &LocalLayout{Root: abs, LabelsCSV: csvPath, TotalBytes: info.Size()}
	if layout.TotalBytes > MaxTotalBytes {
		return nil, fmt.Errorf(
			"dataset is %s, exceeds v0.1 cap of %s. For larger datasets, the "+
				"cloud-source path is on the v0.2 roadmap (tracebloc/client#147).",
			HumanBytes(layout.TotalBytes), HumanBytes(MaxTotalBytes))
	}
	return layout, nil
}

// ParseSchema parses a --schema flag value of the form
// "col:TYPE,col:TYPE,..." into a column→type map. Types are passed
// through verbatim (the ingestor validates them against the SQL types
// it supports: INT, BIGINT, FLOAT, BOOLEAN, DATE, DATETIME,
// TIMESTAMP, TIME, TEXT, VARCHAR(n), ...). Whitespace around tokens
// is trimmed.
func ParseSchema(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		col, typ, ok := strings.Cut(pair, ":")
		col, typ = strings.TrimSpace(col), strings.TrimSpace(typ)
		if !ok || col == "" || typ == "" {
			return nil, fmt.Errorf(
				"schema entry %q must be col:TYPE (e.g. age:INT,price:FLOAT)", pair)
		}
		out[col] = typ
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--schema is empty; expected col:TYPE,col:TYPE,...")
	}
	return out, nil
}

// InferSchema reads the CSV header and a sample of rows and infers a
// column→SQL-type map: all-integer columns → INT, otherwise
// all-numeric → FLOAT, otherwise VARCHAR(255). Empty cells are
// ignored when judging a column; a column with NO non-empty sampled
// value is typed as a nullable FLOAT (not VARCHAR — an all-NULL VARCHAR
// is exactly what the ingestor's string validator rejects) and returned
// in `empty` so the caller can warn.
//
// It's a convenience so customers don't hand-write a --schema for the
// common case. Non-numeric specials (timestamps, dates, booleans)
// infer as VARCHAR(255); pass --schema to declare them precisely.
//
// Framework-managed columns (see reservedColumns — id, data_id, …)
// are skipped and returned as the second value so the caller can tell
// the customer they weren't included.
func InferSchema(csvPath string) (schema map[string]string, skipped, empty []string, err error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = f.Close() }()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reading CSV header from %s: %w", csvPath, err)
	}
	if len(header) == 0 {
		return nil, nil, nil, fmt.Errorf("CSV %s has no columns", csvPath)
	}

	// Per-column running judgement.
	couldBeInt := make([]bool, len(header))
	couldBeFloat := make([]bool, len(header))
	sawValue := make([]bool, len(header))
	for i := range header {
		couldBeInt[i] = true
		couldBeFloat[i] = true
	}

	for n := 0; n < schemaInferenceSampleRows; n++ {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("reading CSV row from %s: %w", csvPath, err)
		}
		for i := 0; i < len(header) && i < len(row); i++ {
			v := strings.TrimSpace(row[i])
			if v == "" {
				continue
			}
			sawValue[i] = true
			if couldBeInt[i] {
				if _, e := strconv.ParseInt(v, 10, 64); e != nil {
					couldBeInt[i] = false
				}
			}
			if couldBeFloat[i] {
				if _, e := strconv.ParseFloat(v, 64); e != nil {
					couldBeFloat[i] = false
				}
			}
		}
	}

	schema = make(map[string]string, len(header))
	for i, col := range header {
		col = strings.TrimSpace(col)
		if reservedColumns[col] {
			// Framework-managed (id, data_id, …): the ingestor adds it
			// and rejects a schema that redeclares it. Skip + report.
			skipped = append(skipped, col)
			continue
		}
		switch {
		case sawValue[i] && couldBeInt[i]:
			schema[col] = "INT"
		case sawValue[i] && couldBeFloat[i]:
			schema[col] = "FLOAT"
		case !sawValue[i]:
			// Entirely empty in the sample (e.g. an unmeasured analyte in a
			// sparse panel). It can't be typed from data; default to a
			// nullable FLOAT rather than VARCHAR — a tabular feature column
			// is numeric far more often than text, and an all-NULL VARCHAR
			// is exactly the shape the ingestor's string validator rejects.
			// Reported in `empty` so the caller can warn / the user can
			// --schema-override.
			schema[col] = "FLOAT"
			empty = append(empty, col)
		default:
			schema[col] = "VARCHAR(255)"
		}
	}
	return schema, skipped, empty, nil
}
