package push

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
// to decide each column's type. It MIRRORS data-ingestors'
// schema_inference.SAMPLE_CAP (5000) so the two implementations agree on
// the same prefix of a file (di#349). The whole CSV would be more
// accurate but a few thousand rows is plenty to distinguish the SQL
// types in practice, and bounds the work for large files. A column whose
// true type only reveals itself past the sample (e.g. an int column that
// turns float on row 10k, or a zero-padded code that first appears past
// the cap) is the case --schema exists to override.
// The value-level parity fixture pins this equality
// (internal/push/testdata/schema_inference_parity.json, "sample_cap").
const schemaInferenceSampleRows = 5000

// Signed 32-bit bounds. A parsed integer outside this range needs BIGINT,
// not INT, or it overflows a MySQL INT column on write. Mirrors
// schema_inference.INT32_MIN/INT32_MAX (di#349). The int64 bound is
// enforced by strconv.ParseInt(…, 10, 64) erroring on overflow — an
// all-digit value beyond int64 is not storable as an integer and falls
// through to VARCHAR, matching the ingestor.
const (
	int32Min = -2147483648
	int32Max = 2147483647
)

// boolText is the textual-boolean vocabulary — deliberately NOT the 0/1
// digit forms. A pure 0/1 column is inferred INT (lossless); only these
// unambiguous words map to BOOLEAN. Mirrors schema_inference._BOOL_TEXT
// (di#349), a deliberate subset of coercion.BOOL_STRINGS.
var boolText = map[string]struct{}{
	"true": {}, "false": {}, "t": {}, "f": {},
	"yes": {}, "no": {}, "y": {}, "n": {},
}

// ASCII-only digit grammars ([0-9], not \d). Restricting to ASCII routes
// Unicode-digit and underscore-grouped tokens to VARCHAR, keeping the CLI
// and the ingestor in lockstep (schema_inference._LEADING_ZERO_CODE /
// _INT_RE / _FLOAT_RE). The float grammar pre-screens the token before
// ParseFloat so Go and Python reject the same non-finite / grouped forms.
var (
	leadingZeroCodeRE = regexp.MustCompile(`^[+-]?0[0-9]+$`)
	intRE             = regexp.MustCompile(`^[+-]?[0-9]+$`)
	floatRE           = regexp.MustCompile(`^[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
)

// dateLayouts / datetimeLayouts are the naive (no-timezone) calendar forms
// the CLI recognizes when mirroring schema_inference's date rule (rule 5,
// checked AFTER numeric so an all-digit id like "20240101" stays INT).
// data-ingestors uses pandas' liberal to_datetime; the CLI cannot reproduce
// every pandas spelling in Go, so it recognizes the common ISO forms and
// otherwise falls back to VARCHAR — the SAFE direction: a mis-typed VARCHAR
// is correctable with --schema, and because the CLI EMITS the schema
// explicitly, the CLI's answer is authoritative in-cluster regardless of the
// deployed ingestor's own guess. The tz-aware RFC3339 form is parsed
// separately (parseCalendarDate) so a column of MIXED UTC offsets is detected
// and routed to VARCHAR — matching the ingestor, whose _infer_datetime
// returns None (→ VARCHAR) on mixed timezones. The value-level parity fixture
// pins the forms that must agree.
var (
	dateLayouts = []string{"2006-01-02", "2006/01/02"}
	// Naive datetime layouts only — tz-aware RFC3339 is handled explicitly
	// in parseCalendarDate (see the mixed-offset guard there).
	datetimeLayouts = []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006/01/02 15:04:05",
	}
)

// DiscoverTabular validates a local input for a tabular / time-series
// ingestion. Unlike the image layout, tabular categories have NO
// sidecar files — the dataset IS a single CSV. Two shapes are accepted
// (#181):
//
//   - a bare .csv file: the dataset itself, passed directly;
//   - a directory containing exactly one .csv file.
//
// Both resolve to the SAME staged layout — the CSV is staged as the one
// labels.csv under the dataset — so the ingestor's contract is unchanged
// (this is a CLI-side input convenience, not an ingestor-side change).
//
// The returned LocalLayout reuses the image layout's LabelsCSV field
// (staged as labels.csv) with an empty Images slice, so the existing
// tar/stream machinery handles it unchanged.
// isCSV reports whether name has a .csv extension, matched
// case-insensitively. It's the single rule DiscoverTabular's walk, its
// bare-file branch, and SniffFamily all key on — shared so the sniff's
// "confident tabular" promise can never drift from what the walk actually
// accepts (the exact lockstep the surrounding comments rely on).
func isCSV(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".csv")
}

// findSingleCSV resolves the one .csv file a tabular layout must hold in
// dir, enforcing DiscoverTabular's exactly-one rule: zero or multiple CSVs
// are errors with the same framing. dir must already be known to be a
// directory. Factored out so the interactive label-header preview
// (previewLabelCSVPath) locates the same CSV the walk would, and can never
// drift from — or silently soften — the count rule.
func findSingleCSV(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading %q: %w", dir, err)
	}
	var csvs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if isCSV(e.Name()) {
			csvs = append(csvs, e.Name())
		}
	}
	sort.Strings(csvs)
	switch len(csvs) {
	case 0:
		return "", fmt.Errorf(
			"no .csv file found in %q. Tabular / time-series categories expect a "+
				"single CSV holding the dataset (one column per feature, plus the "+
				"label column).", dir)
	case 1:
		return filepath.Join(dir, csvs[0]), nil
	default:
		return "", fmt.Errorf(
			"found %d .csv files in %q (%s); the tabular layout expects exactly one. "+
				"Put the dataset CSV in its own directory and re-run.",
			len(csvs), dir, strings.Join(csvs, ", "))
	}
}

func DiscoverTabular(rootDir string) (*LocalLayout, error) {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolving %q: %w", rootDir, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("reading dataset path %q: %w", abs, err)
	}

	// Resolve the CSV + the layout root from either shape. A directory
	// takes DiscoverTabular's exactly-one-CSV rule (findSingleCSV); a bare
	// file is accepted only when it's a .csv — the dataset IS that CSV, so
	// it stages identically to a one-CSV directory (#181). The root is the
	// directory either way (the file's parent for the bare-file case), so
	// the pre-flight summary's "root" field stays a directory.
	var csvPath, root string
	if st.IsDir() {
		root = abs
		csvPath, err = findSingleCSV(abs)
		if err != nil {
			return nil, err
		}
	} else {
		if !isCSV(abs) {
			return nil, fmt.Errorf(
				"%q is not a .csv file. Tabular / time-series data is a single CSV — "+
					"pass the .csv file itself, or a directory containing exactly one .csv.",
				abs)
		}
		root = filepath.Dir(abs)
		csvPath = abs
	}
	csvName := filepath.Base(csvPath)
	// Lstat (not Stat) so a symlinked CSV is rejected rather than
	// silently followed — mirrors the image layout's symlink guard.
	info, err := os.Lstat(csvPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", csvName, err)
	}
	if err := rejectSymlink(info, csvName); err != nil {
		return nil, err
	}
	if info.Size() > MaxSingleFileBytes {
		return nil, sizeError(csvName, info.Size(), MaxSingleFileBytes)
	}

	layout := &LocalLayout{Root: root, LabelsCSV: csvPath, TotalBytes: info.Size()}
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

// SchemaInference is the result of inferring a tabular schema from a CSV.
// Schema is the column→SQL-type map the CLI emits as spec.schema.
// Skipped/Empty/IDLike are the risky cases surfaced to the user as warnings
// so a wrong guess can be corrected with --schema before anything is
// ingested — never a silent guess (RFC-0002 §8.1).
type SchemaInference struct {
	// Schema maps each non-reserved column to its inferred SQL type.
	Schema map[string]string
	// Skipped lists framework-managed columns (see reservedColumns) that
	// were excluded — the ingestor adds them itself and rejects a schema
	// that redeclares them.
	Skipped []string
	// Empty lists columns with NO non-missing value in the sample; they
	// are typed VARCHAR(1) (mirroring the ingestor's all-missing rule) and
	// flagged because the type is a guess with no evidence behind it.
	Empty []string
	// IDLike lists INT/BIGINT columns whose sampled values are all
	// distinct — they look like identifiers. If such a column is really a
	// zero-padded code the leading zeros were stripped somewhere upstream,
	// or it should be a VARCHAR; the warning points it out.
	IDLike []string
}

// InferSchema reads the CSV header and up to schemaInferenceSampleRows
// data rows and infers a column→SQL-type map, MIRRORING data-ingestors'
// schema_inference.infer_schema (di#349) column-for-column via
// inferColumnType. The ingestor OWNS these rules; the CLI mirrors them so
// the schema it EMITS is the same answer the ingestor would compute — and,
// because it is emitted explicitly (a.Spec.Schema → spec.schema), is
// authoritative in-cluster regardless of the deployed ingestor version
// (RFC-0002 Principle 6). The value-level parity fixture pins the two
// implementations to the same per-column answer.
//
// Framework-managed columns (see reservedColumns — id, data_id, …) are
// skipped: the ingestor adds them itself and rejects a schema that
// redeclares them. The risky cases (empty-in-sample, id-like) are returned
// alongside the schema so the caller can surface them as warnings.
func InferSchema(csvPath string) (*SchemaInference, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // ragged rows are CheckDuplicateHeaders' / read-time's diagnostic, not ours
	r.LazyQuotes = true    // read the rows pandas would; a bare quote must not abort inference
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("reading CSV header from %s: %w", csvPath, err)
	}
	if len(header) == 0 {
		return nil, fmt.Errorf("CSV %s has no columns", csvPath)
	}

	// Collect each column's raw sampled values (row-capped at
	// schemaInferenceSampleRows to match the ingestor's SAMPLE_CAP), then
	// infer per column. Cleaning (trim + drop empty/NA) happens inside
	// cleanTokens so inferColumnType matches the ingestor byte-for-byte.
	cols := make([][]string, len(header))
	for n := 0; n < schemaInferenceSampleRows; n++ {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading CSV row from %s: %w", csvPath, err)
		}
		for i := 0; i < len(header) && i < len(row); i++ {
			cols[i] = append(cols[i], row[i])
		}
	}

	res := &SchemaInference{Schema: make(map[string]string, len(header))}
	for i, col := range header {
		col = strings.TrimSpace(col)
		if reservedColumns[col] {
			res.Skipped = append(res.Skipped, col)
			continue
		}
		tokens := cleanTokens(cols[i])
		typ := classifyTokens(tokens)
		res.Schema[col] = typ
		if len(tokens) == 0 {
			res.Empty = append(res.Empty, col)
		} else if isIntegerType(typ) && allDistinct(tokens) {
			res.IDLike = append(res.IDLike, col)
		}
	}
	return res, nil
}

// inferColumnType returns the SQL type for one column from its RAW values,
// a faithful mirror of schema_inference.infer_column_type (di#349). Used
// directly by the value-level parity test. See classifyTokens for the
// rule precedence.
func inferColumnType(values []string) string {
	return classifyTokens(cleanTokens(values))
}

// cleanTokens takes the first schemaInferenceSampleRows raw values, trims
// each, and drops empty / NA-sentinel tokens — mirroring
// schema_inference._clean_tokens (the cap is applied to the RAW values,
// then missing tokens are dropped). naSentinels (preflight.go) is the same
// set as the ingestor's coercion.NA_SENTINELS.
func cleanTokens(values []string) []string {
	if len(values) > schemaInferenceSampleRows {
		values = values[:schemaInferenceSampleRows]
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		s := strings.TrimSpace(v)
		if s == "" {
			continue
		}
		if _, isNA := naSentinels[s]; isNA {
			continue
		}
		out = append(out, s)
	}
	return out
}

// classifyTokens applies the di#349 rule precedence (FIRST MATCH WINS) to
// already-cleaned tokens:
//
//  0. no tokens (all missing)     -> VARCHAR(1)
//  1. any leading-zero code       -> VARCHAR(n)   (THE #349 fix: "007" is text)
//  2. all textual boolean         -> BOOLEAN      (0/1 is INT, not BOOL)
//  3. all integer                 -> INT / BIGINT (BIGINT past int32; >int64 -> text)
//  4. all finite float            -> FLOAT
//  5. all date / datetime         -> DATE / DATETIME (after numeric)
//  6. otherwise                   -> VARCHAR(n)
//
// VARCHAR(n) sizes n by RUNE count (utf8.RuneCountInString), matching
// MySQL VARCHAR(n) character semantics and the ingestor's char-count
// sizing — NOT byte length.
func classifyTokens(tokens []string) string {
	if len(tokens) == 0 {
		return "VARCHAR(1)"
	}

	// 1. Leading-zero code — one such token pins the column to text.
	for _, t := range tokens {
		if leadingZeroCodeRE.MatchString(t) {
			return varcharOf(tokens)
		}
	}

	// 2. Textual boolean.
	if allBoolText(tokens) {
		return "BOOLEAN"
	}

	// 3. Integer (INT vs BIGINT by magnitude; >int64 all-digit -> text).
	if allMatch(intRE, tokens) {
		return integerType(tokens)
	}

	// 4. Float.
	if allFiniteFloat(tokens) {
		return "FLOAT"
	}

	// 5. Date / datetime (after numeric, so numeric ids can't be mis-dated).
	if dt := inferDatetime(tokens); dt != "" {
		return dt
	}

	// 6. Fallback.
	return varcharOf(tokens)
}

func allBoolText(tokens []string) bool {
	for _, t := range tokens {
		if _, ok := boolText[strings.ToLower(t)]; !ok {
			return false
		}
	}
	return true
}

func allMatch(re *regexp.Regexp, tokens []string) bool {
	for _, t := range tokens {
		if !re.MatchString(t) {
			return false
		}
	}
	return true
}

// integerType assumes every token already matched intRE. It returns INT
// (all within signed int32), BIGINT (within int64 but past int32), or
// VARCHAR (any token beyond int64 — not storable as an integer). Mirrors
// schema_inference rule 3.
func integerType(tokens []string) string {
	widen := false
	for _, t := range tokens {
		v, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			// Beyond int64: not storable as an integer -> text.
			return varcharOf(tokens)
		}
		if v < int32Min || v > int32Max {
			widen = true
		}
	}
	if widen {
		return "BIGINT"
	}
	return "INT"
}

// allFiniteFloat reports whether every token is a finite float under the
// ASCII float grammar — the regex pre-screen rejects underscore grouping,
// Unicode digits, and the inf/nan spellings ParseFloat would otherwise
// accept, and the isfinite guard catches a regex-valid overflow ("1e400").
func allFiniteFloat(tokens []string) bool {
	for _, t := range tokens {
		if !floatRE.MatchString(t) {
			return false
		}
		fv, err := strconv.ParseFloat(t, 64)
		if err != nil || math.IsInf(fv, 0) || math.IsNaN(fv) {
			return false
		}
	}
	return true
}

// inferDatetime returns "DATE"/"DATETIME" if every token parses as a
// calendar date under the recognized layouts, else "". Guard: each token
// must contain an ASCII digit (so plain words / month names stay text) —
// mirrors schema_inference._infer_datetime's ASCII-digit guard. hasTime is
// true when any token carries a time-of-day component.
//
// Mixed-offset guard: a column whose tokens don't all share one timezone key
// (tz-aware values with differing UTC offsets, or a mix of tz-aware and
// tz-naive values) is not a single-timezone calendar column, so it falls back
// to VARCHAR. This mirrors the ingestor, where pd.to_datetime(format="mixed")
// returns None on mixed timezones (schema_inference._infer_datetime) — without
// the guard the CLI would emit a tz-naive DATETIME that silently drops the
// per-row offset.
func inferDatetime(tokens []string) string {
	hasTime := false
	tzKey := ""
	haveTZ := false
	for _, t := range tokens {
		if !containsASCIIDigit(t) {
			return ""
		}
		ok, withTime, key := parseCalendarDate(t)
		if !ok {
			return ""
		}
		if withTime {
			hasTime = true
		}
		if !haveTZ {
			tzKey, haveTZ = key, true
		} else if key != tzKey {
			// Non-uniform timezone across the column — VARCHAR, matching the
			// ingestor's None result for mixed timezones.
			return ""
		}
	}
	if hasTime {
		return "DATETIME"
	}
	return "DATE"
}

func containsASCIIDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return true
		}
	}
	return false
}

// parseCalendarDate reports whether t parses under a recognized date /
// datetime layout, whether the matched layout carries a time, and a timezone
// key used to detect a column of mixed UTC offsets. A tz-aware RFC3339 token
// keys on its offset in seconds ("z<offset>"); every naive (no-timezone) date
// or datetime layout keys as "naive". A column whose tokens don't all share
// one key is not a single-timezone calendar column (see inferDatetime's
// mixed-offset guard).
func parseCalendarDate(t string) (ok, withTime bool, tzKey string) {
	// tz-aware: RFC3339 carries an explicit offset (or Z).
	if tm, err := time.Parse(time.RFC3339, t); err == nil {
		_, off := tm.Zone()
		return true, true, "z" + strconv.Itoa(off)
	}
	for _, layout := range datetimeLayouts {
		if _, err := time.Parse(layout, t); err == nil {
			return true, true, "naive"
		}
	}
	for _, layout := range dateLayouts {
		if _, err := time.Parse(layout, t); err == nil {
			return true, false, "naive"
		}
	}
	return false, false, ""
}

// varcharOf sizes VARCHAR(n) by the longest sampled value in RUNES (code
// points), floor 1 — MySQL VARCHAR(n) counts characters, so a multibyte
// value must not be sized by its UTF-8 byte length. Mirrors
// schema_inference._varchar.
func varcharOf(tokens []string) string {
	n := 1
	for _, t := range tokens {
		if c := utf8.RuneCountInString(t); c > n {
			n = c
		}
	}
	return fmt.Sprintf("VARCHAR(%d)", n)
}

// isIntegerType reports whether an inferred type is INT or BIGINT — used
// to flag id-like columns (all-distinct integers).
func isIntegerType(typ string) bool {
	return typ == "INT" || typ == "BIGINT"
}

// allDistinct reports whether every token is unique. An all-distinct
// integer column looks like an identifier — flagged so the user can
// confirm it is a real feature (and not a code whose leading zeros were
// lost upstream).
func allDistinct(tokens []string) bool {
	seen := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		if _, dup := seen[t]; dup {
			return false
		}
		seen[t] = struct{}{}
	}
	return true
}
