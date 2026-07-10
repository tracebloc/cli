// Package push — local preflight previews of the in-cluster ingestor's
// validators (backend#828 P3; closes cli#69/#71/#72/#73).
//
// THE CONTRACT: every check in this file previews a NAMED rule in
// tracebloc/data-ingestors, so "preflight passed" means "the ingestor's
// validation will pass" — the customer finds out BEFORE the upload, not
// after. The CLI never invents rules of its own here: stricter-than-ingestor
// checks would reject datasets the cluster accepts, looser ones burn
// uploads. Parity is pinned by internal/push/parity_golden_test.go against
// goldens generated from the real Python validators
// (scripts/gen-validator-goldens.py); regenerate them whenever the
// ingestor's rules change.
package push

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// utf8BOM is the byte-order mark Excel's "CSV UTF-8" export prepends.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// openCSVReader opens path for row-walking with any UTF-8 BOM stripped, the one
// idiom the pandas-backed checks share (cli#71): pandas strips the BOM even
// under encoding="utf-8", so a BOM'd file must read as if it had none or the
// CLI would reject what the cluster accepts. FieldsPerRecord is -1 so a ragged
// row is a per-row concern, not an abort. The caller closes the returned
// Closer. A caller that must read the rows pandas tolerates (an unescaped
// quote) sets r.LazyQuotes = true before its first Read.
func openCSVReader(path string) (*csv.Reader, io.Closer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReader(f)
	if head, _ := br.Peek(3); bytes.Equal(head, utf8BOM) {
		_, _ = br.Discard(3)
	}
	r := csv.NewReader(br)
	r.FieldsPerRecord = -1
	return r, f, nil
}

// matchColumnIndex returns the index of the header column matching want — an
// exact match first, then case-insensitively with surrounding whitespace
// stripped (the ingestor's resolve_column / _match_column rule). Returns -1
// when absent. Shared by the label-column and filename-column checks so the
// ingestor's single resolve rule has a single Go copy.
func matchColumnIndex(header []string, want string) int {
	for i, c := range header {
		if c == want {
			return i
		}
	}
	wl := strings.ToLower(strings.TrimSpace(want))
	for i, c := range header {
		if strings.ToLower(strings.TrimSpace(c)) == wl {
			return i
		}
	}
	return -1
}

// HasBOM reports whether the file starts with a UTF-8 BOM.
func HasBOM(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 3)
	n, err := io.ReadFull(f, head)
	if err != nil && n < 3 {
		return false, nil // shorter than a BOM — trivially no BOM
	}
	return bytes.Equal(head, utf8BOM), nil
}

// ReadCSVHeader returns the first record of the CSV with a UTF-8 BOM (if
// any) stripped and surrounding whitespace trimmed from each name.
//
// BOM parity (cli#71): the ingestor reads CSVs with pandas, which strips
// the BOM even under encoding="utf-8" — so for the pandas-backed checks
// (label column, row counts) a BOM'd file behaves as if it had none, and
// this reader must match or the CLI would falsely reject what the cluster
// accepts. The one in-cluster path that does NOT strip it is the tabular
// schema probe — see CheckTabularBOM.
func ReadCSVHeader(path string) ([]string, error) {
	r, closer, err := openCSVReader(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = closer.Close() }()
	header, err := r.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is empty — no header row", filepath.Base(path))
		}
		return nil, fmt.Errorf("reading %s header: %w", filepath.Base(path), err)
	}
	for i := range header {
		header[i] = strings.TrimSpace(header[i])
	}
	return header, nil
}

// CheckTabularBOM previews the in-cluster tabular schema probe
// (data-ingestors data_validator.py _validate_csv_streaming): unlike the
// pandas paths, that probe reads the header with Python's stdlib csv over
// plain utf-8, so a BOM glues U+FEFF onto the first column name and the
// schema check FALSELY rejects the file after the full upload
// ("Schema columns not present in CSV: <first column>"). Until that
// ingestor bug is fixed and deployed, a BOM'd tabular CSV is doomed
// in-cluster — reject it here, before the upload, with the actual fix.
func CheckTabularBOM(path string) error {
	bom, err := HasBOM(path)
	if err != nil {
		return fmt.Errorf("checking %s for a byte-order mark: %w", filepath.Base(path), err)
	}
	if !bom {
		return nil
	}
	return fmt.Errorf(
		"%s starts with a UTF-8 byte-order mark (Excel's \"CSV UTF-8\" export adds it), "+
			"which the cluster's schema check can't read — the ingestion would fail after "+
			"uploading everything. Re-save it without the mark (in Excel choose plain \"CSV\"; "+
			"or: tail -c +4 %s > fixed.csv) and re-run.",
		filepath.Base(path), filepath.Base(path))
}

// CheckLabelColumn previews the ingestor's LabelColumnValidator
// (label_column_validator.py): the configured label column must exist in
// the CSV header — matched exactly first, then case-insensitively with
// surrounding whitespace stripped (the ingestor's _match_column rule; it
// must stay this loose or the CLI would reject datasets the cluster
// accepts).
func CheckLabelColumn(header []string, labelColumn, csvName string) error {
	if matchColumnIndex(header, labelColumn) >= 0 {
		return nil
	}
	return fmt.Errorf(
		"label column %q isn't in %s's header (columns: %s). Pass --label-column with one of "+
			"the existing columns, or add a %q column to the CSV.",
		labelColumn, csvName, strings.Join(header, ", "), labelColumn)
}

// CheckDuplicateHeaders previews the ingestor's duplicate-header probes
// (data_validator.py preflight + csv_ingestor.py read time): duplicate
// column names — compared stripped but case-SENSITIVE, exactly as the
// ingestor compares them — are rejected there, and the read-time copy
// fires after table creation, leaving an orphaned empty table. Catch it
// locally instead. (InferSchema's map keying would also silently collapse
// the duplicate — cli#73a.)
func CheckDuplicateHeaders(header []string, csvName string) error {
	seen := make(map[string]bool, len(header))
	var dups []string
	for _, c := range header {
		if seen[c] {
			dups = append(dups, c)
		}
		seen[c] = true
	}
	if len(dups) == 0 {
		return nil
	}
	sort.Strings(dups)
	return fmt.Errorf(
		"%s has duplicate column name(s): %s. Each column must be unique — the cluster "+
			"rejects duplicates, and the schema would map onto the wrong column. Rename them and re-run.",
		csvName, strings.Join(dups, ", "))
}

// CheckHasDataRows previews the ingestor's IngestableRecordsValidator
// (ingestable_records_validator.py _check_has_rows, run for every
// category): a header-only CSV has zero ingestable records and is
// rejected in-cluster before any table is created.
func CheckHasDataRows(path string) error {
	r, closer, err := openCSVReader(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = closer.Close() }()
	if _, err := r.Read(); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s is empty — add a header and at least one data row, then re-run", filepath.Base(path))
		}
		return fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	if _, err := r.Read(); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf(
				"%s has a header but no data rows (0 ingestable records). Add at least one data row and re-run.",
				filepath.Base(path))
		}
		return fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	return nil
}

// ValidateImages previews the ingestor's ImageResolutionValidator
// (image_validator.py): it opens EVERY image (header-only decode — cheap)
// and rejects zero-byte files, undecodable files, images below the
// minimum-size floor, and any image whose resolution differs from the
// expected size (exact equality, zero tolerance — the ingestor validates,
// it does not resize). Previously the CLI decoded only the first image, so
// a single odd-sized or corrupt file failed in-cluster after the full
// upload (cli#72b/c).
//
// expectedW/expectedH of 0 skips the resolution comparison (the caller
// couldn't establish a target size — the ingestor would then auto-detect
// from its first file, which the CLI's detection already mirrors).
//
// minW/minH is the minimum-size floor (#348), mirroring the ingestor's
// _meets_min_size: an image is too small when EITHER side is below the
// floor; an image exactly at the floor passes. 0/0 disables the floor.
// PreflightDataset passes a non-zero floor ONLY when the customer set
// --min-size — it does NOT default to MinImageSize, because the deployed
// ingestor has no floor yet (see the PreflightDataset image branch), so a
// default block would reject an ingest the live cluster accepts. The
// too-small check takes precedence over the resolution mismatch, exactly
// as data-ingestors #348 returns the too_small error before the
// target_size uniformity error.
func ValidateImages(images []string, expectedW, expectedH, minW, minH int) error {
	const maxListed = 5
	var broken, tooSmall, mismatched []string
	for _, path := range images {
		name := filepath.Base(path)
		f, err := os.Open(path)
		if err != nil {
			broken = append(broken, fmt.Sprintf("%s (unreadable: %v)", name, err))
			continue
		}
		cfg, _, err := image.DecodeConfig(f)
		_ = f.Close()
		if err != nil {
			if st, serr := os.Stat(path); serr == nil && st.Size() == 0 {
				broken = append(broken, name+" (empty file, 0 bytes)")
			} else {
				broken = append(broken, name+" (not a valid image — corrupt or unsupported format)")
			}
			continue
		}
		if minW > 0 && minH > 0 && (cfg.Width < minW || cfg.Height < minH) {
			tooSmall = append(tooSmall,
				fmt.Sprintf("%s (%dx%d)", name, cfg.Width, cfg.Height))
		}
		if expectedW > 0 && expectedH > 0 && (cfg.Width != expectedW || cfg.Height != expectedH) {
			mismatched = append(mismatched,
				fmt.Sprintf("%s (%dx%d)", name, cfg.Width, cfg.Height))
		}
	}
	// Floor first: an image below the minimum size simply can't be trained
	// on, so it's the most fundamental, actionable failure — data-ingestors
	// #348 returns it ahead of the uniformity / target_size mismatch.
	if len(tooSmall) > 0 {
		return fmt.Errorf(
			"%d image(s) are smaller than the %dx%d minimum you set with --min-size: %s. "+
				"Provide larger images, or lower the floor with --min-size, then re-run.",
			len(tooSmall), minW, minH, TruncateList(tooSmall, maxListed))
	}
	if len(broken) > 0 {
		return fmt.Errorf(
			"%d image(s) can't be ingested: %s. The cluster rejects these after the upload — "+
				"fix or remove them and re-run.",
			len(broken), TruncateList(broken, maxListed))
	}
	if len(mismatched) > 0 {
		return fmt.Errorf(
			"%d image(s) don't match the %dx%d resolution: %s. The cluster validates the size, "+
				"it does not resize — make them uniform, or pass --target-size to match your data.",
			len(mismatched), expectedW, expectedH, TruncateList(mismatched, maxListed))
	}
	return nil
}

// CrossCheckLabels previews the transfer-time fate of each labels.csv row
// for image_classification (file_transfer.py _find_src): a row whose image
// file doesn't exist under images/ is dropped as a failed record — the run
// then "completes with failures" (exit 9) after the full upload. The
// filename column may omit the extension; the ingestor appends the
// dataset's extension in that case, and this check mirrors that. Files
// with no CSV row are NOT an error (the ingestor never checks that
// direction for image_classification) — the caller may surface them as a
// note.
//
// The image filename is read from the column NAMED "filename", not positionally:
// the ingestor does record.get("filename") over the header-keyed record
// (file_transfer.py / record_processor.py), so a `label,filename` header (filename
// not first) resolves to that column — reading rec[0] would treat the LABEL value
// as the filename and false-reject a layout the cluster ingests cleanly.
func CrossCheckLabels(csvPath string, images []string, extension string) (missing []string, orphans []string, err error) {
	r, closer, err := openCSVReader(csvPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", filepath.Base(csvPath), err)
	}
	defer func() { _ = closer.Close() }()

	present := make(map[string]bool, len(images))
	for _, img := range images {
		present[filepath.Base(img)] = true
	}
	referenced := make(map[string]bool)

	header, err := r.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, nil // emptiness is CheckHasDataRows' diagnostic
		}
		return nil, nil, fmt.Errorf("reading %s: %w", filepath.Base(csvPath), err)
	}
	fnIdx := imageFileColIndex(header)
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading %s: %w", filepath.Base(csvPath), err)
		}
		if fnIdx >= len(rec) {
			continue // short/ragged row — no filename cell to check
		}
		name := strings.TrimSpace(rec[fnIdx])
		if name == "" {
			continue
		}
		// Mirror the ingestor's extension handling (_has_extension): the
		// dataset extension is appended unless the name already ends in a
		// KNOWN media extension — a dotted stem like "img.2024" is not an
		// extension to the ingestor, and must not be one here either.
		if !hasKnownExtension(name) && extension != "" {
			name += extension
		}
		referenced[name] = true
		if !present[name] {
			missing = append(missing, name)
		}
	}
	for name := range present {
		if !referenced[name] {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(orphans)
	return missing, orphans, nil
}

// imageFileColIndex returns the header index of the column the ingestor reads each
// image's file key from: "filename" if present, else "data_id" — the ingestor's own
// precedence (image_paths.prepare_classification_pytorch_image_df / image_loader),
// position-independent for both. A label,data_id CSV (no filename column) is ingested
// cleanly by the cluster, so matching only "filename" and falling back to index 0
// would read the label column as filenames and false-reject it (exit 3).
//
// Each name is matched exactly first, then case-insensitively with surrounding
// whitespace stripped (the ingestor's _match_column rule). "filename" wins over
// "data_id" when both resolve. Falls back to 0 only when NEITHER column exists — a
// labels.csv the ingestor rejects at validate_data regardless, not this check's job
// to diagnose.
func imageFileColIndex(header []string) int {
	for _, want := range []string{"filename", "data_id"} {
		for i, h := range header {
			if strings.TrimSpace(h) == want {
				return i
			}
		}
		for i, h := range header {
			if strings.EqualFold(strings.TrimSpace(h), want) {
				return i
			}
		}
	}
	return 0
}

// CheckAnnotationPairing previews the ingestor's FilePairingValidator
// (file_pairing_validator.py) for object_detection: every image must have
// an annotation with the same filename stem and vice versa — a mismatch in
// either direction fails in-cluster after the upload.
func CheckAnnotationPairing(images, annotations []string) error {
	const maxListed = 5
	stems := func(paths []string) map[string]bool {
		m := make(map[string]bool, len(paths))
		for _, p := range paths {
			base := filepath.Base(p)
			m[strings.TrimSuffix(base, filepath.Ext(base))] = true
		}
		return m
	}
	imgStems, annStems := stems(images), stems(annotations)
	var noAnn, noImg []string
	for s := range imgStems {
		if !annStems[s] {
			noAnn = append(noAnn, s)
		}
	}
	for s := range annStems {
		if !imgStems[s] {
			noImg = append(noImg, s)
		}
	}
	if len(noAnn) == 0 && len(noImg) == 0 {
		return nil
	}
	sort.Strings(noAnn)
	sort.Strings(noImg)
	var parts []string
	if len(noAnn) > 0 {
		parts = append(parts, fmt.Sprintf("%d image(s) without an annotation (%s)",
			len(noAnn), TruncateList(noAnn, maxListed)))
	}
	if len(noImg) > 0 {
		parts = append(parts, fmt.Sprintf("%d annotation(s) without an image (%s)",
			len(noImg), TruncateList(noImg, maxListed)))
	}
	return fmt.Errorf(
		"images/ and annotations/ don't pair up: %s. Every image needs a same-named .xml "+
			"annotation (and vice versa) — the cluster rejects mismatches after the upload.",
		strings.Join(parts, "; "))
}

// TruncateList joins up to max items, appending "… and N more" past that.
func TruncateList(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s, … and %d more", strings.Join(items[:max], ", "), len(items)-max)
}

// CheckLabelDiversity previews the ingestor's LabelDiversityValidator
// (label_diversity_validator.py, wired for every classification category):
// a classification dataset needs at least 2 distinct label values
// (whitespace-stripped) — a single class can't train a classifier, and the
// in-cluster rejection otherwise lands after the full upload. Mirrors the
// validator's benign-skip when the label column isn't found (that's
// CheckLabelColumn's diagnostic, not this one's).
//
// dropNASentinels and collapseNumeric mirror the ingestor's per-column
// read (LabelDiversityValidator._label_read_kwargs): for a SCHEMA-TYPED
// tabular label pandas drops NA-sentinel values (na_values) and, for
// NUMERIC types only, numeric inference collapses "1"/"1.0" — but a
// string-family type (VARCHAR/CHAR/TEXT/STRING) is pinned to dtype=str, so
// numeric-looking labels stay distinct (data-ingestors #252). Image/text
// labels are read untyped with keep_default_na=False (both flags false), so
// even an empty string is a real class and every distinct trimmed string
// counts. The caller derives the two flags from the label's schema type.
func CheckLabelDiversity(csvPath, labelColumn string, dropNASentinels, collapseNumeric bool) error {
	v := readLabelColumnValues(csvPath, labelColumn, dropNASentinels, collapseNumeric)
	// Benign-skip when the column is absent (that's CheckLabelColumn's
	// diagnostic) or an unreadable file (another check's) — both leave Found
	// false. Two or more classes is diverse enough.
	if !v.Found || len(v.Classes) >= 2 {
		return nil
	}
	return fmt.Errorf(
		"the label column %q has %d distinct value(s) — a classification dataset needs at "+
			"least 2 classes. The cluster rejects this after the upload; check the labels and re-run.",
		labelColumn, len(v.Classes))
}

// LabelReadValues is the value-level view of a label column: the header the
// read path RESOLVES the configured name to (case/whitespace-insensitively —
// the ingestor's rule), the sorted distinct classes the ingestor counts, and
// the data-row count. It is what the value-level parity harness pins, so a
// preview that says "N rows, K classes" cannot silently diverge from what the
// ingestor actually reads — the accept/accept-with-divergent-label class the
// verdict-only harness is blind to (data-ingestors #340).
type LabelReadValues struct {
	Resolved string   `json:"resolved_label"`
	Classes  []string `json:"classes"`
	RowCount int      `json:"row_count"`
	Found    bool     `json:"-"`
}

// ReadLabelValues is the exported value-level read used by the parity harness
// (and, later, the RFC-0002 "check your data" preview). It shares the exact
// read/resolve/NA/collapse rules with CheckLabelDiversity via
// readLabelColumnValues, so the value preview and the diversity verdict cannot
// drift from each other.
func ReadLabelValues(csvPath, labelColumn string, dropNASentinels, collapseNumeric bool) LabelReadValues {
	return readLabelColumnValues(csvPath, labelColumn, dropNASentinels, collapseNumeric)
}

// readLabelColumnValues reads csvPath's label column once and returns its
// value-level view. The column is resolved exactly, then case/whitespace-
// insensitively (mirroring the ingestor's resolve_column rule); each row value
// is whitespace-trimmed; NA sentinels are dropped and numeric values collapsed
// per the caller's flags (see CheckLabelDiversity's doc for how those mirror
// the ingestor's per-column read). Unlike the previous early-exit diversity
// scan, this reads the whole column to build the full class set + row count —
// one scan now backs both the diversity verdict and the value-level preview.
func readLabelColumnValues(csvPath, labelColumn string, dropNASentinels, collapseNumeric bool) LabelReadValues {
	r, closer, err := openCSVReader(csvPath)
	if err != nil {
		return LabelReadValues{} // Found=false: unreadable file is another check's diagnostic
	}
	defer func() { _ = closer.Close() }()
	header, err := r.Read()
	if err != nil {
		return LabelReadValues{}
	}
	col, resolved := -1, ""
	for i, c := range header {
		if strings.TrimSpace(c) == labelColumn {
			col, resolved = i, strings.TrimSpace(c)
			break
		}
	}
	if col == -1 {
		want := strings.ToLower(strings.TrimSpace(labelColumn))
		for i, c := range header {
			if strings.ToLower(strings.TrimSpace(c)) == want {
				col, resolved = i, strings.TrimSpace(c)
				break
			}
		}
	}
	if col == -1 {
		return LabelReadValues{} // Found=false — benign skip, like the ingestor
	}
	distinct := map[string]bool{}
	rowCount := 0
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			continue
		}
		rowCount++
		if len(rec) <= col {
			continue
		}
		v := strings.TrimSpace(rec[col])
		if dropNASentinels {
			if _, isNA := naSentinels[v]; isNA {
				continue
			}
		}
		if collapseNumeric {
			// Numeric inference collapses "1" and "1.0" into one value
			// in-cluster; normalize the same way before counting.
			if fv, err := strconv.ParseFloat(v, 64); err == nil {
				v = strconv.FormatFloat(fv, 'g', -1, 64)
			}
		}
		distinct[v] = true
	}
	classes := make([]string, 0, len(distinct))
	for k := range distinct {
		classes = append(classes, k)
	}
	sort.Strings(classes)
	return LabelReadValues{Resolved: resolved, Classes: classes, RowCount: rowCount, Found: true}
}

// knownMediaExtensions mirrors the ingestor's FileExtension.get_all_extensions
// (utils/constants.py): the ONLY suffixes file_transfer._has_extension treats
// as extensions. Anything else — img.2024, photo.v2 — gets the dataset
// extension appended by the ingestor, and CrossCheckLabels must mirror that
// or it rejects dotted stems the cluster resolves fine.
var knownMediaExtensions = map[string]struct{}{
	".jpeg": {}, ".jpg": {}, ".png": {}, ".xml": {}, ".txt": {}, ".text": {},
}

// hasKnownExtension previews file_transfer._has_extension: true only when the
// name's final dot-suffix is one of the ingestor's known extensions
// (case-insensitive).
func hasKnownExtension(name string) bool {
	_, ok := knownMediaExtensions[strings.ToLower(filepath.Ext(name))]
	return ok
}

// CheckCSVEncoding previews the ingestor's preflight.check_csv_encoding —
// the FIRST gate validate_data runs in-cluster: the CSV must be valid UTF-8
// and free of NUL bytes, or the whole run aborts (after the upload).
func CheckCSVEncoding(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxSingleFileBytes+1))
	if err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	if bytes.IndexByte(data, 0x00) >= 0 {
		return fmt.Errorf(
			"%s contains a NUL byte — the file is corrupt or not really a CSV. The cluster "+
				"rejects it after the upload; re-export the file and re-run.",
			filepath.Base(path))
	}
	if !utf8.Valid(data) {
		return fmt.Errorf(
			"%s isn't valid UTF-8 (likely a Latin-1/Windows-1252 export). The cluster rejects "+
				"non-UTF-8 CSVs after the upload — re-save it as UTF-8 and re-run.",
			filepath.Base(path))
	}
	return nil
}

// naSentinels mirrors the ingestor's coercion.NA_SENTINELS: values pandas
// drops to NaN for SCHEMA-TYPED columns (tabular labels), and therefore
// values the in-cluster LabelDiversityValidator does not count as classes.
var naSentinels = map[string]struct{}{
	"": {}, "NA": {}, "N/A": {}, "n/a": {}, "NULL": {}, "null": {},
	"None": {}, "none": {}, "NaN": {}, "nan": {}, "<NA>": {}, "#N/A": {},
}

// labelSchemaType resolves the label column's declared SQL type from the
// schema, matched case- and whitespace-insensitively — mirrors the ingestor's
// LabelDiversityValidator._schema_type_for. ok is false when the label isn't
// a schema column (an untyped read, in-cluster).
func labelSchemaType(schema map[string]string, labelColumn string) (sqlType string, ok bool) {
	if t, found := schema[labelColumn]; found {
		return t, true
	}
	target := strings.ToLower(strings.TrimSpace(labelColumn))
	for k, v := range schema {
		if strings.ToLower(strings.TrimSpace(k)) == target {
			return v, true
		}
	}
	return "", false
}

// isStringSQLType reports whether an SQL type declaration is a string family
// (VARCHAR/CHAR/TEXT/STRING) — the types the ingestor pins to dtype=str,
// which suppresses pandas numeric inference on the label column. Mirrors the
// base-type check in LabelDiversityValidator._label_read_kwargs.
func isStringSQLType(sqlType string) bool {
	base := strings.ToUpper(strings.TrimSpace(sqlType))
	if i := strings.IndexByte(base, '('); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	switch base {
	case "VARCHAR", "CHAR", "TEXT", "STRING":
		return true
	}
	return false
}

// CheckSchemaColumns previews DataValidator's missing-schema-column probe
// ("Schema columns not present in CSV: …"): every schema column must appear
// in the header, compared stripped but case-SENSITIVE — exactly the probe's
// set difference.
func CheckSchemaColumns(header []string, schema map[string]string, csvName string) error {
	present := make(map[string]bool, len(header))
	for _, c := range header {
		present[strings.TrimSpace(c)] = true
	}
	var missing []string
	for col := range schema {
		if !present[strings.TrimSpace(col)] {
			missing = append(missing, col)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"--schema names column(s) that aren't in %s: %s. The cluster rejects this after the "+
			"upload — fix the schema or the CSV header, then re-run.",
		csvName, strings.Join(missing, ", "))
}

// CheckSequenceSchemaColumns previews the ingest.v1 schema's sequence-grouped
// conditional (the time_series_classification if/then) plus the presence
// probes of SequenceGroupValidator / PerGroupTimeOrderedValidator: a grouped
// task's schema must declare BOTH fixed sequence columns — the group key
// (sequence_id) and the time column (timestamp). The names are FIXED by the
// platform (backend#1054 Decision-2); there is no flag to rename them, so the
// fix is always renaming the CSV columns (or extending an explicit --schema).
// Compared as exact schema-map keys, matching the JSON-schema `required`
// semantics — the vendored-schema validation would reject the same YAML, this
// check just fails earlier with a friendlier message.
func CheckSequenceSchemaColumns(schema map[string]string, g GroupingSpec) error {
	var missing []string
	for _, col := range []string{g.GroupColumn, g.TimeColumn} {
		if _, ok := schema[col]; !ok {
			missing = append(missing, col)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"this task's data is sequence-grouped: the schema must declare %q (groups the timestep "+
			"rows of one sequence — e.g. a patient/device/session id) and %q (orders the rows "+
			"within each sequence). Missing: %s. The column names are fixed by the platform — "+
			"rename your CSV columns to match and re-run.",
		g.GroupColumn, g.TimeColumn, strings.Join(missing, ", "))
}

// CheckSequenceRows previews the SequenceGroupValidator's null-id rule
// (sequence_group_validator.py): every timestep row must carry a non-empty
// sequence id — a row whose group key is null/empty belongs to NO sequence,
// so it can't contribute to any per-sequence sample and the in-cluster
// rejection otherwise lands after the full upload. Together with
// CheckHasDataRows this guarantees every sequence has >= 1 real row and at
// least one sequence exists at all.
//
// NA sentinels count as null: the ingestor loads the column with pandas,
// whose NA parsing turns "NA"/"null"/… into NaN before the validator's
// isna() probe — mirrored here via naSentinels (the ingestor's
// coercion.NA_SENTINELS). The column is resolved with the shared
// case-/whitespace-insensitive rule (#340). An absent column benign-skips
// (returns 0, nil): that is CheckSequenceSchemaColumns' /
// CheckSchemaColumns' diagnostic, not this one's.
//
// sequences is the count of distinct non-null ids — the dataset's SAMPLE
// count, since the platform counts sequence-grouped data in sequences, not
// rows (backend#1054 Decision-3); the caller echoes it as a note.
func CheckSequenceRows(csvPath, groupColumn string) (sequences int, err error) {
	r, closer, err := openCSVReader(csvPath)
	if err != nil {
		return 0, nil // unreadable file is another check's diagnostic
	}
	defer func() { _ = closer.Close() }()
	header, err := r.Read()
	if err != nil {
		return 0, nil
	}
	col := matchColumnIndex(header, groupColumn)
	if col == -1 {
		return 0, nil // benign skip — the schema checks own this diagnostic
	}
	distinct := map[string]bool{}
	nullCount, rowNum, firstNullRow := 0, 0, 0
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			continue
		}
		rowNum++
		v := ""
		if len(rec) > col {
			v = strings.TrimSpace(rec[col])
		}
		if _, isNA := naSentinels[v]; isNA {
			nullCount++
			if firstNullRow == 0 {
				firstNullRow = rowNum
			}
			continue
		}
		distinct[v] = true
	}
	if nullCount > 0 {
		return len(distinct), fmt.Errorf(
			"the sequence column %q has %d empty/null value(s) (first at data row %d). Every "+
				"timestep row must carry the id of the sequence it belongs to — the cluster rejects "+
				"this after the upload; fill in the ids and re-run.",
			groupColumn, nullCount, firstNullRow)
	}
	return len(distinct), nil
}

// PreflightProblem is a preflight rejection. BadFlag marks problems whose
// fix is a flag value (the CLI maps those to exit 2); everything else is a
// data problem (exit 3).
type PreflightProblem struct {
	Err     error
	BadFlag bool
}

// PreflightDataset is THE preflight dispatch — the single place that decides
// which previews run for which category, shared verbatim by the CLI
// (runLocalPreflight) and the parity harness (parity_golden_test.go), so the
// two cannot drift: deleting or rewiring a check here fails the parity test.
// Returned notes are advisory (the CLI prints them dim); a non-nil problem
// is the rejection.
//
// Ordering mirrors the in-cluster pipeline: encoding gate first (the
// ingestor's check_csv_encoding runs before any validator), then the
// validator previews.
func PreflightDataset(spec SpecArgs, layout *LocalLayout) (notes []string, problem *PreflightProblem) {
	dataProblem := func(err error) *PreflightProblem {
		if err == nil {
			return nil
		}
		return &PreflightProblem{Err: err}
	}

	switch {
	case IsTabular(spec.Category):
		if err := CheckTabularBOM(layout.LabelsCSV); err != nil {
			return nil, dataProblem(err)
		}
		if err := CheckCSVEncoding(layout.LabelsCSV); err != nil {
			return nil, dataProblem(err)
		}
		header, err := ReadCSVHeader(layout.LabelsCSV)
		if err != nil {
			return nil, dataProblem(err)
		}
		if err := CheckDuplicateHeaders(header, "the data CSV"); err != nil {
			return nil, dataProblem(err)
		}
		if err := CheckHasDataRows(layout.LabelsCSV); err != nil {
			return nil, dataProblem(err)
		}
		if err := CheckSchemaColumns(header, spec.Schema, "the data CSV"); err != nil {
			return nil, dataProblem(err)
		}
		// A bogus --label-column otherwise fails in-cluster only at READ
		// time — after the table was created — leaving an orphaned table.
		if err := CheckLabelColumn(header, spec.LabelColumn, "the data CSV"); err != nil {
			return nil, &PreflightProblem{Err: err, BadFlag: true}
		}
		// Sequence-grouped tasks (time_series_classification), gated on the
		// vendored contract's grouping TRAIT — never the category id
		// (backend#1054 Decision-4). Previews SequenceGroupValidator +
		// the ingest.v1 sequence-column conditional: the fixed sequence_id /
		// timestamp columns must be in the schema, and every timestep row
		// must carry a sequence id. Runs before the label checks, mirroring
		// the ingestor's factory order (SequenceGroupValidator first).
		if g, grouped := GroupingFor(spec.Category); grouped {
			if err := CheckSequenceSchemaColumns(spec.Schema, g); err != nil {
				return nil, dataProblem(err)
			}
			seqs, err := CheckSequenceRows(layout.LabelsCSV, g.GroupColumn)
			if err != nil {
				return nil, dataProblem(err)
			}
			if seqs > 0 {
				// The platform counts this dataset in sequences, not rows
				// (Decision-3) — echo the sample count the customer will see.
				notes = append(notes, fmt.Sprintf(
					"Note: %d sequence(s) grouped by %q — the platform counts this dataset "+
						"in sequences, not rows", seqs, g.GroupColumn))
			}
		}
		// Label diversity for every tabular classification task — gated on
		// the registry's IsClassification (the ingestor's is_classification
		// wiring: tabular_classification + time_series_classification), not
		// a hardcoded id, so a future classification task can't silently
		// skip the preview.
		if IsClassification(spec.Category) {
			// The label is a schema-typed column: the ingestor drops NA
			// sentinels for it, and collapses numeric-looking values ONLY
			// for numeric types — a VARCHAR label is pinned to dtype=str,
			// keeping "1"/"1.0" distinct (data-ingestors #252). Derive both
			// flags from the label's declared type so the preview doesn't
			// wrongly collapse a string label and reject a diverse dataset.
			sqlType, inSchema := labelSchemaType(spec.Schema, spec.LabelColumn)
			dropNA := inSchema
			collapseNumeric := !(inSchema && isStringSQLType(sqlType))
			if err := CheckLabelDiversity(layout.LabelsCSV, spec.LabelColumn, dropNA, collapseNumeric); err != nil {
				return nil, dataProblem(err)
			}
		}

	case IsImage(spec.Category):
		if err := CheckCSVEncoding(layout.LabelsCSV); err != nil {
			return nil, dataProblem(err)
		}
		// Every image, header-only decode (cheap). Previously only the
		// first image was ever opened, so one corrupt or odd-sized file
		// failed in-cluster after the full upload (cli#72).
		expW, expH := 0, 0
		if len(spec.TargetSize) == 2 {
			expW, expH = spec.TargetSize[0], spec.TargetSize[1]
		}
		// Minimum-size floor (#348). The floor lives in data-ingestors only
		// on develop (di#348/#356); the DEPLOYED ingestor (v0.5.7/v0.6.0) has
		// no floor and ingests small images fine. So the preview must NOT
		// apply the 32x32 default on its own — a default block would reject an
		// ingest the live cluster accepts, the inverse of the tabular-BOM
		// block (whose reject mirrors a real deployed rejection). Apply the
		// floor ONLY when the customer explicitly set --min-size (spec.MinSize)
		// — their own declared requirement, honored locally regardless of the
		// cluster. Once di#348 reaches prod, default this to MinImageSize and
		// flip the imgc-too-small parity case so the floor is previewed by
		// default. The emit side already matches: it omits file_options.min_size
		// when unset, letting whichever ingestor is deployed apply its own
		// default (none today; MinImageSize post-#348).
		minW, minH := 0, 0
		if len(spec.MinSize) == 2 {
			minW, minH = spec.MinSize[0], spec.MinSize[1]
		}
		if err := ValidateImages(layout.Images, expW, expH, minW, minH); err != nil {
			return nil, dataProblem(err)
		}
		if err := CheckHasDataRows(layout.LabelsCSV); err != nil {
			return nil, dataProblem(err)
		}
		header, err := ReadCSVHeader(layout.LabelsCSV)
		if err != nil {
			return nil, dataProblem(err)
		}
		if err := CheckDuplicateHeaders(header, "labels.csv"); err != nil {
			return nil, dataProblem(err)
		}
		// LabelDiversityValidator runs in-cluster for the WHOLE image
		// family (is_classification covers object_detection + keypoint
		// too); it benign-skips when no label column resolves, and so
		// does the preview. Image labels are read untyped, so no NA drop
		// and no numeric collapse.
		if err := CheckLabelDiversity(layout.LabelsCSV, spec.LabelColumn, false, false); err != nil {
			return nil, dataProblem(err)
		}
		switch spec.Category {
		case "image_classification":
			if err := CheckLabelColumn(header, spec.LabelColumn, "labels.csv"); err != nil {
				return nil, &PreflightProblem{Err: err, BadFlag: true}
			}
			// A row whose image is missing becomes a failed record
			// in-cluster ("completed with failures", exit 9) — after the
			// upload. Extra files are only a note (the ingestor never
			// checks that direction).
			missing, orphanFiles, err := CrossCheckLabels(layout.LabelsCSV, layout.Images, spec.Extension)
			if err != nil {
				return nil, dataProblem(err)
			}
			if len(missing) > 0 {
				return nil, dataProblem(fmt.Errorf(
					"%d labels.csv row(s) reference images that aren't in images/: %s. Those records "+
						"would fail after the upload — fix the rows or add the files, then re-run.",
					len(missing), TruncateList(missing, 5)))
			}
			if len(orphanFiles) > 0 {
				notes = append(notes, fmt.Sprintf(
					"Note: %d file(s) in images/ have no labels.csv row and won't be part of the dataset: %s",
					len(orphanFiles), TruncateList(orphanFiles, 5)))
			}
		case "object_detection":
			// images↔annotations stem pairing (FilePairingValidator preview).
			if err := CheckAnnotationPairing(layout.Images, layout.Sidecars["annotations"]); err != nil {
				return nil, dataProblem(err)
			}
		}

	default:
		// Text family.
		if err := CheckCSVEncoding(layout.LabelsCSV); err != nil {
			return nil, dataProblem(err)
		}
		if err := CheckHasDataRows(layout.LabelsCSV); err != nil {
			return nil, dataProblem(err)
		}
		header, err := ReadCSVHeader(layout.LabelsCSV)
		if err != nil {
			return nil, dataProblem(err)
		}
		if err := CheckDuplicateHeaders(header, "labels.csv"); err != nil {
			return nil, dataProblem(err)
		}
		// Every SUPERVISED text task carries a label column the ingestor
		// requires present — text_classification & sentence_pair_classification
		// via LabelColumnValidator, token_classification via BIOLabelValidator —
		// so preview the header for all of them. Gating on !SelfSupervisedText
		// mirrors buildText's label emission, so a typo'd --label-column fails
		// locally (exit 2) instead of after the full upload. The self-supervised
		// tasks (MLM, CLM, seq2seq, embeddings) carry no label.
		if !SelfSupervisedText(spec.Category) {
			if err := CheckLabelColumn(header, spec.LabelColumn, "labels.csv"); err != nil {
				return nil, &PreflightProblem{Err: err, BadFlag: true}
			}
			// LabelDiversityValidator is wired only for the is_classification
			// text tasks (text_classification, sentence_pair_classification), NOT
			// token_classification — its BIO tag sequences aren't class labels,
			// so the ingestor runs BIOLabelValidator instead and never checks
			// label diversity. Mirror that exactly (Principle 6): gate on
			// IsClassification. Text labels are read untyped (like image), so no
			// NA drop and no numeric collapse.
			if IsClassification(spec.Category) {
				if err := CheckLabelDiversity(layout.LabelsCSV, spec.LabelColumn, false, false); err != nil {
					return nil, dataProblem(err)
				}
			}
		}
	}
	return notes, nil
}
