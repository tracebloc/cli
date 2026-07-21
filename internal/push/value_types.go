package push

import (
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

// isIntegerSQLType / isFloatSQLType classify a schema base type (as produced by
// sqlBaseType) into the two numeric families whose per-value contract
// CheckColumnValueTypes previews. They mirror DataValidator.type_validators
// (validators/data_validator.py): the INT family shares one integer contract
// (parseable AND whole), the FLOAT family (incl. DECIMAL/NUMERIC) one
// real-number contract.
func isIntegerSQLType(base string) bool {
	switch base {
	case "INT", "INTEGER", "TINYINT", "SMALLINT", "MEDIUMINT", "BIGINT":
		return true
	}
	return false
}

func isFloatSQLType(base string) bool {
	switch base {
	case "FLOAT", "DOUBLE", "DECIMAL", "NUMERIC":
		return true
	}
	return false
}

// CheckColumnValueTypes previews the NUMERIC portion of the ingestor's
// DataValidator (validators/data_validator.py), composed for every tabular /
// time-series category that carries a schema (modalities/validators.py).
// DataValidator scans the FULL column and rejects a value that does not match
// its declared SQL type BEFORE the table is created; the CLI otherwise only
// proves the schema's COLUMNS exist (CheckSchemaColumns), so a schema/value TYPE
// mismatch — a non-numeric string in an INT/FLOAT column, or a fractional value
// in an INT column — passed preflight and then rejected in-cluster after the
// upload, orphaning a freshly-created empty table (cli#352). The gap bites an
// explicit --schema that disagrees with the data AND an inferred schema whose
// bad value first appears past the inference sample (InferSchema samples 5000
// rows; the ingestor scans them all).
//
// SCOPE — numeric types only, and only the SAFE (never-over-reject) subset:
//   - INT family (INT/INTEGER/TINYINT/SMALLINT/MEDIUMINT/BIGINT): a present,
//     non-NA cell that isn't a finite number, or that has a fractional part
//     (the ingestor's `to_numeric` + `numeric % 1 != 0`);
//   - FLOAT family (FLOAT/DOUBLE/DECIMAL/NUMERIC): a present, non-NA cell that
//     isn't a number.
//
// The string (VARCHAR/CHAR/TEXT length), BOOLEAN, and DATE/DATETIME/TIMESTAMP/
// TIME per-value checks are DELIBERATELY not mirrored, and integer OVERFLOW and
// non-finite (inf) are left to the ingestor: reproducing pandas' to_datetime /
// boolean-vocabulary / length / range coercion in Go risks REJECTING a value
// the cluster accepts — the dangerous direction (a burned upload), the same
// reason perGroupTimeViolation leaves the date branch an under-preview. Those
// stay a documented under-preview; the CLI only ever UNDER-rejects here, never
// over-rejects (the parity contract's cardinal rule).
//
// Value read (mirrors how the ingestor reads a TYPED numeric column:
// na_values=coercion.build_csv_na_values(schema) — which is NA_SENTINELS for
// every schema column — with keep_default_na=False): a cell is "present" iff its
// RAW cell is not an NA sentinel (matched on the raw cell, since pandas tokenises
// NA before stripping) and is not whitespace-only. A missing value is stored
// NULL, never flagged — the ingestor's `numeric.isna() & series.notna()` mask.
// Numbers are parsed after trimming (pd.to_numeric tolerates surrounding
// whitespace); ParseFloat accepts a superset of pandas' numeric grammar for real
// inputs, so a ParseFloat failure is a value pandas also rejects (no over-reject).
//
// Columns are resolved by their STRIPPED header name, case-SENSITIVELY, exactly
// as DataValidator matches schema keys after `columns.str.strip()`. The
// category's excluded time column (time_series_classification's grouping time
// column, checked by PerGroupTimeOrderedValidator — which accepts a fractional
// step index as a valid position, not by DataValidator) is skipped so a
// fractional index the cluster ingests fine isn't over-rejected here.
func CheckColumnValueTypes(csvPath string, schema map[string]string, category string) error {
	if len(schema) == 0 {
		return nil
	}
	// The time column the category's DataValidator composition drops (mirrors
	// modalities/validators.py): time_series_classification hands DataValidator
	// the schema MINUS its grouping time column. A time_series_forecasting
	// "timestamp" is TIMESTAMP-typed and is already skipped as a non-numeric
	// type, so it needs no separate name rule here.
	excludedTime := ""
	if g, grouped := GroupingFor(category); grouped {
		excludedTime = strings.TrimSpace(g.TimeColumn)
	}
	// Which schema columns carry a numeric per-value contract, keyed by their
	// stripped name (matching the header resolution below).
	intCols := map[string]bool{}
	floatCols := map[string]bool{}
	for col, typ := range schema {
		name := strings.TrimSpace(col)
		if name == excludedTime {
			continue
		}
		switch base := sqlBaseType(typ); {
		case isIntegerSQLType(base):
			intCols[name] = true
		case isFloatSQLType(base):
			floatCols[name] = true
		}
	}
	if len(intCols) == 0 && len(floatCols) == 0 {
		return nil // no numeric-typed columns — nothing this preview covers
	}

	r, closer, err := openCSVReader(csvPath)
	if err != nil {
		return nil // unreadable file is another check's diagnostic (CheckCSVEncoding)
	}
	defer func() { _ = closer.Close() }()
	header, err := r.Read()
	if err != nil {
		return nil // no header — another check's diagnostic
	}

	// Resolve each numeric column to its header index by STRIPPED name,
	// case-SENSITIVELY — how DataValidator matches schema keys after
	// columns.str.strip(). First occurrence wins; a duplicate header is
	// CheckDuplicateHeaders' diagnostic, not this one's.
	type colCheck struct {
		name    string
		idx     int
		integer bool
	}
	var checks []colCheck
	seenHdr := map[string]bool{}
	for i, h := range header {
		name := strings.TrimSpace(h)
		if seenHdr[name] {
			continue
		}
		seenHdr[name] = true
		switch {
		case intCols[name]:
			checks = append(checks, colCheck{name, i, true})
		case floatCols[name]:
			checks = append(checks, colCheck{name, i, false})
		}
	}
	if len(checks) == 0 {
		return nil // schema columns absent from the header is CheckSchemaColumns' diagnostic
	}

	const maxSample = 5
	type offense struct {
		integer bool
		count   int
		rows    []string
	}
	offenses := map[string]*offense{}
	row := 0
	for {
		rec, rerr := r.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			// Mid-read failure: the unread tail could hide (or clear) offenders.
			// Fail closed rather than pass a partial scan (matches
			// CrossCheckLabels / CheckSequenceRows, #221).
			return fmt.Errorf("reading %s: %w", filepath.Base(csvPath), rerr)
		}
		row++
		for _, c := range checks {
			if c.idx >= len(rec) {
				continue // short/ragged row — the read path owns that diagnostic
			}
			if !numericCellBad(rec[c.idx], c.integer) {
				continue
			}
			o := offenses[c.name]
			if o == nil {
				o = &offense{integer: c.integer}
				offenses[c.name] = o
			}
			o.count++
			if len(o.rows) < maxSample {
				o.rows = append(o.rows, fmt.Sprintf("data row %d", row))
			}
		}
	}
	if len(offenses) == 0 {
		return nil
	}
	// Report the first offending column in header order, for a stable message.
	for _, c := range checks {
		o := offenses[c.name]
		if o == nil {
			continue
		}
		kind := "numbers"
		if o.integer {
			kind = "whole numbers (integers)"
		}
		return fmt.Errorf(
			"column %q isn't all %s: %d value(s) don't match its declared type (e.g. %s). "+
				"The cluster's data-type check rejects these after the table is created — fix the "+
				"values, or correct the column's type with --schema, then re-run.",
			c.name, kind, o.count, strings.Join(o.rows, ", "))
	}
	return nil
}

// numericCellBad reports whether a RAW cell violates its column's numeric
// contract, in the SAFE (never-over-reject) direction. See CheckColumnValueTypes
// for the full read semantics; in brief: NA-sentinel / whitespace-only cells are
// missing (not flagged); a present cell is bad iff it doesn't parse as a number
// (INT and FLOAT) or, for INT, parses to a finite value with a fractional part.
// inf/nan parse but are left to the ingestor's own non-finite handling
// (under-reject, safe).
func numericCellBad(raw string, integer bool) bool {
	if _, isNA := naSentinels[raw]; isNA {
		return false
	}
	t := strings.TrimSpace(raw)
	if t == "" {
		return false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return true // non-numeric — pandas' to_numeric coerces this to NaN too
	}
	if integer {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return false // left to the ingestor's non-finite / NA handling
		}
		if f != math.Trunc(f) {
			return true // a fractional value in an INT column
		}
	}
	return false
}
