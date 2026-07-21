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
	"math"
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
// row is a per-row concern, not an abort, and LazyQuotes is on so a row pandas
// tolerates (an unescaped/bare quote) is read here too rather than turned into
// a parse error — otherwise these mirror-checks would reject a layout the
// ingestor ingests cleanly (the inverse fail direction). With both set, a
// non-EOF Read error is only ever a genuine I/O failure, so callers can treat
// it as fail-closed. The caller closes the returned Closer.
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
	r.LazyQuotes = true // read the rows pandas would, don't drop or error on them
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
// Since data-ingestors #365 (in the ≥0.7.0 pin) the upstream validator also
// validates the floor VALUES at construction — each side must be a positive
// integer or the run fails with a config error. That gate needs no preview
// here: minW/minH arrive as ints by type, and ParseMinSize already rejects
// non-integer / non-positive --min-size input before it reaches the spec.
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

// CheckImageFilenameColumn enforces the ingestor's filename-column contract for
// the image family (layout.v1.json requires_filename_column=true for every image
// task). At transfer time the cluster reads each image's file key with a
// case-sensitive record.get("filename") (file_transfer.py:179); a manifest that
// omits a filename column resolves to NO key, so EVERY row fails as a
// file-transfer error ("No filename found in record") and the run exits 9 — AFTER
// the full upload. The ingestor's OWN preflight (validate_data) does not catch
// this: IngestableRecordsValidator passes when the column is absent ("not this
// validator's error to raise"), so mirroring only its preflight validators
// fails open here. We surface it locally instead — the image mirror of the text
// family's up-front check (referencedTextNames).
//
// Matching is case-insensitive + whitespace-trimmed (matchColumnIndex), the same
// rule the ingestor's _match_column applies. It deliberately does NOT accept
// "data_id" or any other alias: the ingestor never maps another column to
// "filename", so a data_id-only manifest fails transfer identically to image_id.
// (A case-VARIANT header like "Filename" is accepted here yet still fails the
// cluster's case-sensitive transfer read — the #340-class label/filename
// asymmetry, tracked separately.)
func CheckImageFilenameColumn(header []string) error {
	if matchColumnIndex(header, "filename") >= 0 {
		return nil
	}
	return fmt.Errorf(
		"labels.csv has no \"filename\" column (columns: %s) — image tasks match each "+
			"row to its file by that column, and the cluster drops every row without it "+
			"(the ingest uploads, then fails with \"No filename found in record\"). "+
			"Rename your file column to \"filename\" and re-run.",
		strings.Join(header, ", "))
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
	if fnIdx < 0 {
		// No filename column. PreflightDataset rejects this up front
		// (CheckImageFilenameColumn) before CrossCheckLabels runs, so this is
		// unreachable in production; guard against an out-of-range panic if the
		// function is exercised directly.
		return nil, nil, nil
	}
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

// imageFileColIndex returns the header index of the "filename" column — the key
// the ingestor reads each image's file from (case-sensitive record.get("filename")
// at transfer time; file_transfer.py:179). Matching mirrors the ingestor's
// _match_column: exact first, then case-insensitive with surrounding whitespace
// stripped.
//
// It deliberately does NOT accept "data_id" (or any other name): the ingestor
// never maps another column to "filename", so a data_id-only manifest fails
// transfer identically to image_id — accepting it here would false-green-light a
// doomed upload. Returns -1 when absent; PreflightDataset enforces presence up
// front (CheckImageFilenameColumn) before any caller reaches here, so a resolved
// index is guaranteed in production, and CrossCheckLabels guards the -1 case
// against a panic if exercised directly.
func imageFileColIndex(header []string) int {
	return matchColumnIndex(header, "filename")
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

// CheckMaskPairing previews the ingestor's FilePairingValidator for
// semantic_segmentation (modalities/validators.py, sidecar_suffix="_mask"):
// every image must have a PNG mask named "<image-stem>_mask.png" in masks/ and
// vice versa. The ingestor strips the documented "_mask" suffix (#196) from
// mask stems before matching, so image_001.jpg pairs with image_001_mask.png;
// a mask NOT carrying the suffix can't pair and is a non-conforming orphan. A
// mismatch in any direction fails in-cluster after the upload.
func CheckMaskPairing(images, masks []string) error {
	const maxListed = 5
	imgStems := make(map[string]bool, len(images))
	for _, p := range images {
		base := filepath.Base(p)
		if strings.HasPrefix(base, ".") {
			continue // hidden file (e.g. macOS AppleDouble ._x) — FilePairingValidator._stems skips these
		}
		imgStems[strings.TrimSuffix(base, filepath.Ext(base))] = true
	}
	// Strip the extension then the "_mask" suffix so a mask maps back to the
	// image stem it pairs with; a mask stem without the suffix is kept as a
	// non-conforming orphan (mirrors the validator's extra_orphans).
	maskFor := make(map[string]bool, len(masks))
	var nonConforming []string
	for _, p := range masks {
		base := filepath.Base(p)
		if strings.HasPrefix(base, ".") {
			continue // hidden file — mirror the ingestor's _stems, so a stray ._x.png can't fake a mismatch
		}
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		if strings.HasSuffix(stem, "_mask") {
			maskFor[strings.TrimSuffix(stem, "_mask")] = true
		} else {
			nonConforming = append(nonConforming, base)
		}
	}
	var noMask, noImg []string
	for s := range imgStems {
		if !maskFor[s] {
			noMask = append(noMask, s)
		}
	}
	for s := range maskFor {
		if !imgStems[s] {
			noImg = append(noImg, s+"_mask")
		}
	}
	if len(noMask) == 0 && len(noImg) == 0 && len(nonConforming) == 0 {
		return nil
	}
	sort.Strings(noMask)
	sort.Strings(noImg)
	sort.Strings(nonConforming)
	var parts []string
	if len(noMask) > 0 {
		parts = append(parts, fmt.Sprintf("%d image(s) without a mask (%s)",
			len(noMask), TruncateList(noMask, maxListed)))
	}
	if len(noImg) > 0 {
		parts = append(parts, fmt.Sprintf("%d mask(s) without an image (%s)",
			len(noImg), TruncateList(noImg, maxListed)))
	}
	if len(nonConforming) > 0 {
		parts = append(parts, fmt.Sprintf("%d mask(s) not named <image>_mask.png (%s)",
			len(nonConforming), TruncateList(nonConforming, maxListed)))
	}
	return fmt.Errorf(
		"images/ and masks/ don't pair up: %s. Every image needs a same-named "+
			"\"<image>_mask.png\" in masks/ (and vice versa) — the cluster rejects mismatches "+
			"after the upload.",
		strings.Join(parts, "; "))
}

// CheckMaskIDColumn previews the ingestor's MaskIdColumnValidator
// (validators/mask_id_validator.py, backend#816) for semantic_segmentation: the
// manifest must DECLARE a mask_id column AND POPULATE it on every row. The
// training client resolves each mask file from this column with no naming-
// convention fallback, so an undeclared or empty mask_id becomes a late, opaque
// FileNotFoundError at train time. The name is the exact lowercase "mask_id"
// (the stored table keys on it verbatim; a different-case column stores nothing
// the client can read), so a case variant gets a rename hint, not a silent pass.
//
// Audited against the MERGED di#358 validator at pin 8f89aec (cli#286): the
// semantics match point for point — exact-lowercase header required after
// whitespace strip (ReadCSVHeader trims like CSVIngestor's
// columns.str.strip()); case/whitespace variants get a rename hint via the
// same resolve rule (matchColumnIndex ≙ _match_column); the empty scan tests
// the RAW untrimmed cell against coercion.NA_SENTINELS (naSentinels is a
// byte-for-byte mirror) plus whitespace-only/missing — NOT trimmed-then-
// matched, the #239/#240 padded-sentinel trap; a mid-read error fails closed;
// and with duplicate stripped-equal headers both sides inspect the FIRST
// exact-match column. The validator's schema-declaration half is satisfied by
// construction on the CLI side: buildImage always declares
// {"mask_id": "VARCHAR(255)"} in spec.schema. Its csv_options dialect
// threading is N/A here — the CLI stages comma-separated UTF-8 manifests and
// emits no custom dialect.
func CheckMaskIDColumn(csvPath string) error {
	const maskIDColumn = "mask_id"
	header, err := ReadCSVHeader(csvPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(csvPath), err)
	}
	exact := -1
	for i, c := range header {
		if c == maskIDColumn {
			exact = i
			break
		}
	}
	if exact == -1 {
		// Distinguish a wrong-case/whitespace variant (rename) from truly absent
		// (add), mirroring the ingestor's undeclared-vs-miscased split.
		if v := matchColumnIndex(header, maskIDColumn); v >= 0 {
			return fmt.Errorf(
				"semantic_segmentation needs a %q column in %s, but found %q (wrong case). "+
					"Rename it to exactly %q — the training client reads that column to locate each mask.",
				maskIDColumn, filepath.Base(csvPath), header[v], maskIDColumn)
		}
		return fmt.Errorf(
			"semantic_segmentation needs a %q column in %s (columns: %s) linking each image to its "+
				"mask file. Add it — the training client reads it to locate each mask.",
			maskIDColumn, filepath.Base(csvPath), strings.Join(header, ", "))
	}
	// Populated on every row? Mirror the ingestor's NA-sentinel-aware empty scan
	// (a missing cell counts as empty), keeping a bounded sample of row numbers.
	r, closer, err := openCSVReader(csvPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(csvPath), err)
	}
	defer func() { _ = closer.Close() }()
	if _, err := r.Read(); err != nil {
		return nil // no header/rows — CheckHasDataRows owns that diagnostic
	}
	const maxSample = 5
	var emptyRows []string
	emptyCount, row := 0, 0
	for {
		rec, rerr := r.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			// Fail closed on a mid-read error rather than pass a partial scan
			// (matches CrossCheckLabels / #221).
			return fmt.Errorf("reading %s: %w", filepath.Base(csvPath), rerr)
		}
		row++
		// Mirror the ingestor's _is_empty EXACTLY: a cell is empty iff its RAW
		// (untrimmed) value is an NA-sentinel token, OR it's whitespace-only /
		// missing. Trimming BEFORE the sentinel test would treat a PADDED
		// sentinel like " NULL " as empty — but pandas keeps the spaces
		// (skipinitialspace=False), so it's a REAL value in-cluster. That
		// over-rejection is the cli#218/#239 padded-NA parity trap.
		raw := ""
		if exact < len(rec) {
			raw = rec[exact]
		}
		if _, isNA := naSentinels[raw]; isNA || strings.TrimSpace(raw) == "" {
			emptyCount++
			if len(emptyRows) < maxSample {
				emptyRows = append(emptyRows, fmt.Sprintf("data row %d", row))
			}
		}
	}
	if emptyCount > 0 {
		return fmt.Errorf(
			"%d row(s) in %s have an empty %q (e.g. %s). Every row must name its mask file — an "+
				"empty value makes the training client derive a garbage filename and fail. Fill in "+
				"%q or drop those rows, then re-run.",
			emptyCount, filepath.Base(csvPath), maskIDColumn, strings.Join(emptyRows, ", "), maskIDColumn)
	}
	return nil
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
	v, err := readLabelColumnValues(csvPath, labelColumn, dropNASentinels, collapseNumeric)
	if err != nil {
		// A mid-read failure leaves a PARTIAL column — trusting its class count
		// would false-reject good data (under-counted classes) or pass a bad
		// file. Fail closed, like the text preflight (#221) and CrossCheckLabels.
		return err
	}
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
	// The preview path tolerates a mid-read failure as "not found" (the
	// diversity GATE, CheckLabelDiversity, is the one that fails closed on it);
	// a partial preview simply reports nothing rather than a wrong count.
	v, _ := readLabelColumnValues(csvPath, labelColumn, dropNASentinels, collapseNumeric)
	return v
}

// readLabelColumnValues reads csvPath's label column once and returns its
// value-level view. The column is resolved exactly, then case/whitespace-
// insensitively (mirroring the ingestor's resolve_column rule); each row value
// is whitespace-trimmed; NA sentinels are dropped and numeric values collapsed
// per the caller's flags (see CheckLabelDiversity's doc for how those mirror
// the ingestor's per-column read). Unlike the previous early-exit diversity
// scan, this reads the whole column to build the full class set + row count —
// one scan now backs both the diversity verdict and the value-level preview.
func readLabelColumnValues(csvPath, labelColumn string, dropNASentinels, collapseNumeric bool) (LabelReadValues, error) {
	r, closer, err := openCSVReader(csvPath)
	if err != nil {
		return LabelReadValues{}, nil // Found=false: unreadable file is another check's diagnostic
	}
	defer func() { _ = closer.Close() }()
	v, err := labelColumnValuesFrom(r, labelColumn, dropNASentinels, collapseNumeric)
	if err != nil {
		// Mid-read failure: the column is now partial. Fail closed rather than
		// count a truncated class set (matches CrossCheckLabels / #221).
		return LabelReadValues{}, fmt.Errorf("reading %s: %w", filepath.Base(csvPath), err)
	}
	return v, nil
}

// labelColumnValuesFrom is the label scan over an already-opened reader — split
// out so the mid-stream read-error path can be exercised with an injected
// failing reader. A benign miss (no header, or the column absent) returns
// Found=false with a nil error; only a mid-scan read failure is a non-nil error.
func labelColumnValuesFrom(r *csv.Reader, labelColumn string, dropNASentinels, collapseNumeric bool) (LabelReadValues, error) {
	header, err := r.Read()
	if err != nil {
		return LabelReadValues{}, nil // no header (empty/unreadable) — another check's diagnostic
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
		return LabelReadValues{}, nil // Found=false — benign skip, like the ingestor
	}
	distinct := map[string]bool{}
	rowCount := 0
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return LabelReadValues{}, err // caller wraps with the filename and fails closed
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
	return LabelReadValues{Resolved: resolved, Classes: classes, RowCount: rowCount, Found: true}, nil
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
// This is the CURATED set the ingestor applies via build_csv_na_values with
// keep_default_na=False — NOT pandas' global default set. Use it only where
// the in-cluster read pins na_values to coercion.NA_SENTINELS (the
// LabelDiversityValidator label-column read). Plain-read_csv paths use
// pandasDefaultNA instead.
var naSentinels = map[string]struct{}{
	"": {}, "NA": {}, "N/A": {}, "n/a": {}, "NULL": {}, "null": {},
	"None": {}, "none": {}, "NaN": {}, "nan": {}, "<NA>": {}, "#N/A": {},
}

// pandasDefaultNA is pandas' GLOBAL default NA token set —
// pandas._libs.parsers.STR_NA_VALUES, verbatim at the pinned data-ingestors
// pandas ref (3.0.3). The grouped time_series_classification validators
// di#359 added (LabelConstantWithinGroupValidator, PerGroupTimeOrderedValidator)
// load the file with a PLAIN pd.read_csv (keep_default_na=True, no na_values),
// so their label-numeric-collapse and sequence-id dropna see EXACTLY this set —
// not the curated coercion.NA_SENTINELS. The two differ materially: the default
// set adds '#NA', '#N/A N/A', the IEEE spellings ('-nan', '-NaN', '1.#IND',
// '-1.#IND', '1.#QNAN', '-1.#QNAN') and drops the lowercase 'none'. Mirroring
// coercion.NA_SENTINELS here (the pre-fix bug) OVER-rejected: a label column
// that pandas reads as all-numeric (a default sentinel collapsing to NaN, so
// "1"/"1.0" don't flip) was seen by the CLI as a mixed object column and
// flagged as a false mid-sequence flip — rejecting a dataset the ingestor
// accepts (cli#218 parity fix). Membership is matched on the RAW cell: pandas
// tokenises NA before stripping, so " NA " (padded) is NOT NA in-cluster.
//
// Keep this a byte-for-byte copy of STR_NA_VALUES; re-confirm on a pandas bump:
//
//	python -c "from pandas._libs.parsers import STR_NA_VALUES; print(sorted(STR_NA_VALUES))"
var pandasDefaultNA = map[string]struct{}{
	"":         {},
	"#N/A":     {},
	"#N/A N/A": {},
	"#NA":      {},
	"-1.#IND":  {},
	"-1.#QNAN": {},
	"-NaN":     {},
	"-nan":     {},
	"1.#IND":   {},
	"1.#QNAN":  {},
	"<NA>":     {},
	"N/A":      {},
	"NA":       {},
	"NULL":     {},
	"NaN":      {},
	"None":     {},
	"n/a":      {},
	"nan":      {},
	"null":     {},
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
// NA sentinels count as null: SequenceGroupValidator plain-reads the column
// with pandas (keep_default_na=True) and computes null as
// `ids.isna() | (ids.astype(str).str.strip() == "")`, so the faithful null
// set is pandas' DEFAULT NA tokens (STR_NA_VALUES — matched on the RAW cell,
// since pandas tokenises NA before stripping) UNION whitespace-only cells —
// NOT the curated coercion.NA_SENTINELS. Mirrored here via pandasDefaultNA
// plus a TrimSpace=="" clause (cli#239): the coercion set the label checks use
// wrongly drops lowercase "none" (which pandas KEEPS → a real id, so the old
// naSentinels probe was a false REJECT, the dangerous direction) and lacks
// "#NA" (which pandas drops to NaN → null, an under-reject). The column is
// resolved with the shared case-/whitespace-insensitive rule (#340). An absent
// column benign-skips (returns 0, nil): that is CheckSequenceSchemaColumns' /
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
	seqs, nullErr, readErr := sequenceScanFrom(r, groupColumn)
	if readErr != nil {
		// Mid-read failure: null/missing ids in the unread tail would never
		// surface. Fail closed rather than pass a partial scan (matches
		// CrossCheckLabels / #221).
		return 0, fmt.Errorf("reading %s: %w", filepath.Base(csvPath), readErr)
	}
	return seqs, nullErr
}

// sequenceScanFrom scans the group column over an already-opened reader — split
// out so the mid-stream read-error path can be exercised with an injected
// failing reader. nullErr is the domain rejection (empty/null ids); readErr is
// a mid-scan read failure the caller wraps with the filename. A benign miss (no
// header, or the column absent) returns zero values.
func sequenceScanFrom(r *csv.Reader, groupColumn string) (sequences int, nullErr, readErr error) {
	header, err := r.Read()
	if err != nil {
		return 0, nil, nil // no header — benign
	}
	col := matchColumnIndex(header, groupColumn)
	if col == -1 {
		return 0, nil, nil // benign skip — the schema checks own this diagnostic
	}
	distinct := map[string]bool{}
	nullCount, rowNum, firstNullRow := 0, 0, 0
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, nil, err // caller wraps with the filename and fails closed
		}
		rowNum++
		raw := ""
		if len(rec) > col {
			raw = rec[col] // RAW: pandas tokenises NA pre-strip and groups the key verbatim
		}
		// Null iff the raw cell is a pandas DEFAULT NA token OR whitespace-only —
		// SequenceGroupValidator's `isna() | (astype(str).str.strip() == "")` over
		// a plain read_csv (cli#239). "" is already in pandasDefaultNA, so the
		// TrimSpace clause only adds genuine whitespace-only cells; and a padded
		// token (" NA ") is a REAL id in-cluster, so matching the raw (not the
		// trimmed) cell is what keeps parity.
		_, isNA := pandasDefaultNA[raw]
		if isNA || strings.TrimSpace(raw) == "" {
			nullCount++
			if firstNullRow == 0 {
				firstNullRow = rowNum
			}
			continue
		}
		// Distinct sequences count the RAW id — pandas groups the object key
		// verbatim (" p1 " and "p1" are two sequences), consistent with the
		// grouped previews (labelConstantViolation / perGroupTimeViolation).
		distinct[raw] = true
	}
	if nullCount > 0 {
		return len(distinct), fmt.Errorf(
			"the sequence column %q has %d empty/null value(s) (first at data row %d). Every "+
				"timestep row must carry the id of the sequence it belongs to — the cluster rejects "+
				"this after the upload; fill in the ids and re-run.",
			groupColumn, nullCount, firstNullRow), nil
	}
	return len(distinct), nil, nil
}

// tscRow is one data row's cells relevant to the grouped time_series_
// classification previews, gathered in a SINGLE pass over the CSV (the
// ingestor's grouped validators load the whole file too — a whole-group check
// is safe pre-ingest, data-ingestors #359 T15). Cells are stored RAW
// (un-trimmed): the grouped validators plain-read_csv with skipinitialspace
// defaulting False, so pandas keeps surrounding whitespace on object columns
// and groupby keys (" p1 " and "p1" are DISTINCT sequences), and matches NA
// tokens against the raw cell (" NA " is not NA). The numeric-collapse / numeric
// -time paths trim only inside the parse attempt, mirroring pandas' numeric
// coercion, which DOES tolerate surrounding whitespace (" 1" -> 1). A cell
// absent because the row is shorter than a resolved column reads as "" — which
// pandas treats as NaN, i.e. an NA sentinel here.
type tscRow struct {
	seq   string // raw sequence_id cell
	label string // raw label cell
	time  string // raw timestamp cell
	row   int    // 1-based data-row number (for human-readable refs)
}

// scanTSCRows reads csvPath once and returns every data row's sequence_id,
// label, and timestamp cells (RAW, un-trimmed — see tscRow), plus whether each
// column resolved in the header. Columns resolve case-/whitespace-insensitively
// (the ingestor's resolve_column rule, via matchColumnIndex). A benign miss
// (unreadable file or no header) returns no rows and all-false found flags;
// only a mid-scan read failure is a non-nil error (the caller fails closed,
// matching CheckSequenceRows / #221).
func scanTSCRows(csvPath, groupColumn, labelColumn, timeColumn string) (rows []tscRow, groupFound, labelFound, timeFound bool, err error) {
	r, closer, oerr := openCSVReader(csvPath)
	if oerr != nil {
		return nil, false, false, false, nil // unreadable — another check's diagnostic
	}
	defer func() { _ = closer.Close() }()
	header, herr := r.Read()
	if herr != nil {
		return nil, false, false, false, nil // no header — benign
	}
	gi := matchColumnIndex(header, groupColumn)
	li := matchColumnIndex(header, labelColumn)
	ti := matchColumnIndex(header, timeColumn)
	groupFound, labelFound, timeFound = gi >= 0, li >= 0, ti >= 0
	cell := func(rec []string, idx int) string {
		if idx < 0 || len(rec) <= idx {
			return "" // pandas reads a missing field as NaN — "" is an NA token
		}
		return rec[idx] // RAW — pandas keeps whitespace on object cols / group keys
	}
	rowNum := 0
	for {
		rec, rerr := r.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, groupFound, labelFound, timeFound, rerr // caller wraps + fails closed
		}
		rowNum++
		rows = append(rows, tscRow{
			seq:   cell(rec, gi),
			label: cell(rec, li),
			time:  cell(rec, ti),
			row:   rowNum,
		})
	}
	return rows, groupFound, labelFound, timeFound, nil
}

// CheckTSCGroupIntegrity previews the two grouped time_series_classification
// validators data-ingestors #359 added, in ONE pass over the CSV and in the
// ingestor's factory order: LabelConstantWithinGroupValidator, then
// PerGroupTimeOrderedValidator. These are the checks the presence/null-id
// previews above (CheckSequenceSchemaColumns / CheckSequenceRows) don't cover —
// so before cli#218 a mid-sequence label flip or per-group unsorted timestamps
// passed the local dry-run and were rejected only in-cluster after the upload.
//
// STRICTLY grouped-scoped: the sole caller gates on GroupingFor's grouping
// trait, so no non-grouped category runs this. That matters for deployment
// safety — time_series_classification is develop-ahead (di#359 is NOT in the
// deployed v0.7.0 ingestor), so this can only ever preview a develop-ingestor
// rule for a develop-only category; a non-TSC dataset is completely unaffected
// and the CLI can never out-strict a deployed non-grouped path.
//
// A mid-read failure fails closed (wrapped with the filename). Absent columns
// benign-skip per check — the schema / sequence / label checks own those.
func CheckTSCGroupIntegrity(csvPath, groupColumn, labelColumn, timeColumn string, schema map[string]string) error {
	rows, groupFound, labelFound, timeFound, err := scanTSCRows(csvPath, groupColumn, labelColumn, timeColumn)
	if err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(csvPath), err)
	}
	if e := labelConstantViolation(rows, groupFound, labelFound, groupColumn, labelColumn); e != nil {
		return e
	}
	return perGroupTimeViolation(rows, groupFound, timeFound, groupColumn, timeColumn, schema)
}

// labelConstantViolation previews LabelConstantWithinGroupValidator
// (label_constant_within_group_validator.py): time-series classification
// assigns ONE label per sequence, so every timestep row of a sequence_id must
// repeat the same label value. Mirrors the ingestor exactly:
//   - rows group by sequence_id with null ids DROPPED (pandas groupby
//     dropna=True — a null-id row belongs to no sequence and is
//     CheckSequenceRows' rejection, not this one's);
//   - a sequence offends when its label takes more than one DISTINCT value,
//     counting a null/empty label as its own value (nunique(dropna=False));
//   - numeric-looking labels collapse the way the pandas column dtype does —
//     ONLY when EVERY non-null label cell is numeric ("1" and "1.0" are then
//     one value, not a false flip); a mixed/object column compares exact
//     strings.
//
// NA membership uses pandasDefaultNA (STR_NA_VALUES), not the curated
// coercion set: LabelConstantWithinGroupValidator plain-reads the file
// (keep_default_na=True), so the column collapses to numeric whenever the only
// non-numeric cells are default sentinels. Matched on the raw cell (pandas
// tokenises NA pre-strip). Numeric collapse compares float64; pure-integer
// pandas columns are int64, so two DISTINCT integers past 2^53 would collapse
// here (float rounding) and miss a real flip — an UNDER-reject (safe) confined
// to astronomically large integer class labels (rare enough to accept per
// cli#218; revisit with big.Int if real class ids ever exceed 2^53).
//
// An absent sequence_id or label column benign-skips.
func labelConstantViolation(rows []tscRow, groupFound, labelFound bool, groupColumn, labelColumn string) error {
	if !groupFound || !labelFound {
		return nil // another check's diagnostic
	}
	// pandas dtype inference: the column collapses numerics only if EVERY
	// non-null cell parses as a number (else it is an object column — exact
	// strings). NA cells are ignored for the dtype decision (an int column
	// with a NaN is still numeric/float in pandas). Whitespace is trimmed only
	// for the numeric test: pandas' numeric coercion tolerates surrounding
	// space (" 1" -> 1), but object comparison keeps it (see below).
	numeric := true
	for _, r := range rows {
		if _, isNA := pandasDefaultNA[r.label]; isNA {
			continue
		}
		if !isNumericCell(strings.TrimSpace(r.label)) {
			numeric = false
			break
		}
	}
	canon := func(v string) string {
		if _, isNA := pandasDefaultNA[v]; isNA {
			return "\x00NA" // null is its OWN value (nunique dropna=False)
		}
		if numeric {
			if fv, e := strconv.ParseFloat(strings.TrimSpace(v), 64); e == nil {
				return strconv.FormatFloat(fv, 'g', -1, 64)
			}
		}
		return v // object column: pandas keeps the raw string (whitespace and all)
	}
	type acc struct {
		values   map[string]struct{}
		firstRow int
	}
	seen := map[string]*acc{}
	var order []string // first-seen sequence order → stable row refs
	for _, r := range rows {
		if _, isNA := pandasDefaultNA[r.seq]; isNA {
			continue // dropna=True (pandas default NA on the raw group key)
		}
		a := seen[r.seq]
		if a == nil {
			a = &acc{values: map[string]struct{}{}, firstRow: r.row}
			seen[r.seq] = a
			order = append(order, r.seq)
		}
		a.values[canon(r.label)] = struct{}{}
	}
	var offenderRows []int
	for _, s := range order {
		if len(seen[s].values) > 1 {
			offenderRows = append(offenderRows, seen[s].firstRow)
		}
	}
	if len(offenderRows) == 0 {
		return nil
	}
	sort.Ints(offenderRows)
	shown, extra := offenderRows, ""
	if len(shown) > 5 {
		extra = fmt.Sprintf(" (+%d more)", len(shown)-5)
		shown = shown[:5]
	}
	return fmt.Errorf(
		"%d sequence(s) grouped by %q change their %q value mid-sequence (first offending "+
			"sequences start at data row(s) %v%s). Time-series classification assigns ONE label "+
			"per sequence: every row of a sequence must repeat the same label value. The cluster "+
			"rejects this after the upload — fix the labels and re-run.",
		len(offenderRows), groupColumn, labelColumn, shown, extra)
}

// perGroupTimeViolation previews PerGroupTimeOrderedValidator
// (per_group_time_ordered_validator.py): timestep rows must be sorted
// ascending (monotonic NON-decreasing — equal successive values are allowed)
// by the time column WITHIN each sequence; interleaving sequences is fine.
//
// The ingestor branches on the SCHEMA's declared type for the time column
// (_declared_base_type): a numeric step index (INT/FLOAT/…) is parsed with
// pd.to_numeric(coerce); a TIMESTAMP/date type is parsed with pandas'
// permissive "mixed"-format parser plus a locale-ambiguity guard. This preview
// mirrors the NUMERIC branch faithfully. It deliberately does NOT preview the
// TIMESTAMP/date branch: reproducing pandas' "mixed" date parsing + the
// day-first/month-first ambiguity guard in Go would risk REJECTING a date the
// cluster accepts (the dangerous direction — a burned local run, the inverse
// of what preflight is for), so the date-typed ordering stays a documented
// under-preview gap (cli#218) — safe because it only ever under-rejects.
//
// An absent sequence_id or time column benign-skips.
func perGroupTimeViolation(rows []tscRow, groupFound, timeFound bool, groupColumn, timeColumn string, schema map[string]string) error {
	if !groupFound || !timeFound {
		return nil
	}
	if !isNumericTimeColumn(schema, timeColumn) {
		return nil // TIMESTAMP/date branch — documented under-preview (see doc)
	}
	// pd.to_numeric(errors="coerce"): a cell that is not a finite number
	// becomes NaN → invalid. Every timestep needs a valid position to order it
	// within its sequence. The invalid tally spans ALL rows (matching the
	// ingestor's whole-column invalid_mask), not just grouped ones.
	parsed := make([]float64, len(rows))
	valid := make([]bool, len(rows))
	var invalidRows []int
	for i, r := range rows {
		f, ok := parseNumericTime(r.time)
		parsed[i], valid[i] = f, ok
		if !ok {
			invalidRows = append(invalidRows, r.row)
		}
	}
	if len(invalidRows) > 0 {
		sort.Ints(invalidRows)
		shown, extra := invalidRows, ""
		if len(shown) > 5 {
			extra = fmt.Sprintf(" (+%d more)", len(shown)-5)
			shown = shown[:5]
		}
		return fmt.Errorf(
			"the time column %q has %d missing/invalid value(s) (first at data row(s) %v%s). "+
				"Every timestep row needs a valid value to order it within its sequence — the "+
				"cluster rejects this after the upload; fix the values and re-run.",
			timeColumn, len(invalidRows), shown, extra)
	}
	// Per-group monotonic non-decreasing over the valid rows in file order,
	// null-id rows dropped (dropna=True). A strict decrease vs the previous
	// in-group value flags the sequence (mirrors is_monotonic_increasing being
	// false ⟺ some diff < 0); the reported row is that first decrease.
	type gstate struct {
		last       float64
		hasLast    bool
		badRow     int
		outOfOrder bool
	}
	states := map[string]*gstate{}
	var order []string
	for i, r := range rows {
		if _, isNA := pandasDefaultNA[r.seq]; isNA {
			continue // dropna=True (pandas default NA on the raw group key)
		}
		g := states[r.seq]
		if g == nil {
			g = &gstate{}
			states[r.seq] = g
			order = append(order, r.seq)
		}
		if g.hasLast && parsed[i] < g.last && !g.outOfOrder {
			g.outOfOrder = true
			g.badRow = r.row
		}
		g.last = parsed[i]
		g.hasLast = true
	}
	var badRows []int
	for _, s := range order {
		if states[s].outOfOrder {
			badRows = append(badRows, states[s].badRow)
		}
	}
	if len(badRows) == 0 {
		return nil
	}
	sort.Ints(badRows)
	shown, extra := badRows, ""
	if len(shown) > 5 {
		extra = fmt.Sprintf(" (+%d more)", len(shown)-5)
		shown = shown[:5]
	}
	return fmt.Errorf(
		"%d sequence(s) grouped by %q have out-of-order %q values (first offending data row(s) "+
			"%v%s). Timestep rows must be sorted ascending by %q within each sequence — sort each "+
			"sequence and re-run. Interleaving different sequences is fine; ordering is only "+
			"checked within a sequence. The cluster rejects this after the upload.",
		len(badRows), groupColumn, timeColumn, shown, extra, timeColumn)
}

// isNumericCell reports whether v parses as a finite number — the pandas
// numeric-dtype test used to decide label-value collapse.
func isNumericCell(v string) bool {
	f, err := strconv.ParseFloat(v, 64)
	return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// parseNumericTime mirrors pd.to_numeric(errors="coerce") for the time column:
// a parseable finite (or ±inf, which pandas keeps) number is valid; a NaN
// result or a parse failure (empty/NA/non-numeric) is invalid. The raw cell is
// trimmed first — pd.to_numeric tolerates surrounding whitespace (" 1" -> 1),
// so a padded step index must not read as invalid (which would over-reject).
func parseNumericTime(v string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// isNumericTimeColumn reports whether the schema declares the time column as a
// numeric step-index type — the branch selector in
// PerGroupTimeOrderedValidator._declared_base_type / _NUMERIC_BASE_TYPES. No
// declared type resolves to the TIMESTAMP/date branch (returns false), which
// this preview does not check (see perGroupTimeViolation's doc).
func isNumericTimeColumn(schema map[string]string, timeColumn string) bool {
	t, ok := labelSchemaType(schema, timeColumn) // generic case-insensitive schema-type resolve
	if !ok {
		return false
	}
	base := strings.ToUpper(strings.TrimSpace(t))
	if i := strings.IndexByte(base, '('); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	if j := strings.IndexAny(base, " \t"); j >= 0 {
		base = base[:j] // "INT UNSIGNED" → INT (mirrors .split()[0])
	}
	switch base {
	case "INT", "INTEGER", "TINYINT", "SMALLINT", "MEDIUMINT", "BIGINT",
		"FLOAT", "DOUBLE", "DECIMAL", "NUMERIC":
		return true
	}
	return false
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
			// Grouped-integrity previews (cli#218): one label per sequence
			// (LabelConstantWithinGroupValidator) and per-sequence ascending
			// time order (PerGroupTimeOrderedValidator) — the two grouped
			// validators di#359 added that the presence/null-id checks above
			// don't cover. Runs after the null-id check, before label
			// diversity, mirroring the ingestor's factory order.
			if err := CheckTSCGroupIntegrity(layout.LabelsCSV, g.GroupColumn, spec.LabelColumn, g.TimeColumn, spec.Schema); err != nil {
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
		// The ingestor reads each image's file from a case-sensitive
		// record.get("filename") at transfer time; a manifest without a
		// filename column drops EVERY row ("No filename found in record",
		// exit 9) AFTER the upload, and the ingestor's own preflight does not
		// catch it. Enforce the contract's requires_filename_column locally —
		// the image mirror of the text family's up-front check.
		if tl, ok := LayoutFor(spec.Category); ok && tl.Manifest.RequiresFilenameColumn {
			if err := CheckImageFilenameColumn(header); err != nil {
				return nil, dataProblem(err)
			}
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
		case "semantic_segmentation":
			// images↔masks "_mask"-suffix pairing (FilePairingValidator preview)
			// + the mask_id link-column contract (MaskIdColumnValidator preview,
			// backend#816) + the labels.csv rows↔images/ existence check (same
			// file_transfer failed-record preview image_classification runs). We
			// deliberately do NOT run CheckLabelColumn: semseg requires a `label`
			// field, but the ingestor omits LabelColumnValidator for it (labels
			// come from the masks), so verifying the column is in the CSV would
			// over-reject a dataset the cluster accepts.
			if err := CheckMaskPairing(layout.Images, layout.Sidecars["masks"]); err != nil {
				return nil, dataProblem(err)
			}
			if err := CheckMaskIDColumn(layout.LabelsCSV); err != nil {
				return nil, dataProblem(err)
			}
			missing, orphanFiles, cerr := CrossCheckLabels(layout.LabelsCSV, layout.Images, spec.Extension)
			if cerr != nil {
				return nil, dataProblem(cerr)
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
