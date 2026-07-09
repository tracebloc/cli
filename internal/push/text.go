package push

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// textExtensions are the file types the text / MLM ingestor reads by
// default (the schema's file_options.extension allows .txt / .text).
var textExtensions = map[string]struct{}{
	".txt":  {},
	".text": {},
}

// DiscoverText validates a local directory for a text-family ingestion
// (text_classification or masked_language_modeling):
//
//   - <root>/labels.csv          (required)
//   - <root>/<sidecar>/*.txt     (required; sidecar = texts | sequences)
//
// The returned layout stages the CSV (as labels.csv) and the text files
// under "<sidecar>/". masked_language_modeling needs no tokenizer.json —
// the ingestor never read one, and #805 removed the dataset-staged
// tokenizer (it diverged the vocab and broke weight averaging); the
// collaborator's tokenizer ships at model upload. A tokenizer.json left
// in the directory is simply ignored, not an error.
func DiscoverText(category, rootDir string) (*LocalLayout, error) {
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
			"%q is not a directory; pass the directory containing labels.csv + the text files", abs)
	}

	layout := &LocalLayout{
		Root:     abs,
		Sidecars: map[string][]string{},
	}
	dirName := TextSidecarDir(category)

	// labels.csv (required) — same Lstat-based symlink guard as the
	// image layout.
	labelsPath := filepath.Join(abs, "labels.csv")
	labelsStat, err := os.Lstat(labelsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf(
				"missing labels.csv in %q. Text categories expect "+
					"<dir>/labels.csv + <dir>/%s/.", abs, dirName)
		}
		return nil, fmt.Errorf("stat labels.csv: %w", err)
	}
	if err := rejectSymlink(labelsStat, "labels.csv"); err != nil {
		return nil, err
	}
	if labelsStat.IsDir() {
		return nil, fmt.Errorf("%q is a directory, not a file", labelsPath)
	}
	if labelsStat.Size() > MaxSingleFileBytes {
		return nil, sizeError("labels.csv", labelsStat.Size(), MaxSingleFileBytes)
	}
	layout.LabelsCSV = labelsPath
	layout.TotalBytes += labelsStat.Size()

	// Sidecar text dir (required).
	files, sidecarBytes, err := discoverSidecarFiles(abs, dirName, textExtensions)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf(
			"no .txt files found in %q. Text categories expect "+
				"<dir>/%s/*.txt.", filepath.Join(abs, dirName), dirName)
	}
	layout.Sidecars[dirName] = files
	layout.TotalBytes += sidecarBytes

	// Structured-text tasks whose .txt shape the ingestor ENFORCES
	// (sentence_pair_classification: text_a<TAB>text_b; embeddings:
	// anchor<TAB>positive[<TAB>negative]) get the same per-file structural
	// check here, so a malformed layout fails locally with a clear message
	// instead of after the full stage. The rule comes from the vendored
	// layout contract, not hardcoded — the CLI mirrors the ingestor's
	// TabSeparatedRecordValidator (RFC-0002 Principle 6). Unenforced formats
	// (seq2seq, causal LM) accept raw text and are not checked.
	//
	// The check is scoped to the files the manifest actually references, NOT
	// every .txt in the dir: the ingestor's validator walks labels.csv rows and
	// only opens the file each row names, so an unreferenced stray .txt (a
	// README, a scratch draft) must not fail discovery — the ingestor would
	// accept the dataset.
	if rf, ok := RecordFormatFor(category); ok && rf.Enforced {
		referenced, rerr := manifestReferencedTextNames(labelsPath)
		if rerr != nil {
			return nil, rerr
		}
		if err := validateTextRecords(dirName, files, referenced, rf); err != nil {
			return nil, err
		}
	}

	if layout.TotalBytes > MaxTotalBytes {
		return nil, fmt.Errorf(
			"dataset is %s, exceeds v0.1 cap of %s. For larger datasets, the "+
				"cloud-source path is on the v0.2 roadmap (tracebloc/client#147).",
			HumanBytes(layout.TotalBytes), HumanBytes(MaxTotalBytes))
	}
	return layout, nil
}

// validateTextRecords runs the enforced record-format check over the
// manifest-referenced .txt files in dirName, mirroring the ingestor's per-file
// TabSeparatedRecordValidator. Only files whose basename is in referenced (the
// set the manifest walk names) are checked — the ingestor never opens a file no
// row references, so validating a stray unreferenced .txt would reject a layout
// the ingestor accepts (RFC-0002 Principle 6). The first malformed file fails
// discovery with a message naming the offending file (relative to the dataset
// root, e.g. "texts/bad.txt"), so the fix is obvious without reaching the
// cluster.
func validateTextRecords(dirName string, files []string, referenced map[string]struct{}, rf RecordFormat) error {
	for _, f := range files {
		if _, ok := referenced[filepath.Base(f)]; !ok {
			continue
		}
		content, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("reading %s: %w", filepath.Join(dirName, filepath.Base(f)), err)
		}
		if verr := ValidateTextRecord(rf, string(content)); verr != nil {
			return fmt.Errorf("%s: %w", filepath.Join(dirName, filepath.Base(f)), verr)
		}
	}
	return nil
}

// manifestReferencedTextNames returns the set of text-file basenames the
// manifest (labels.csv) references, mirroring the ingestor's
// TabSeparatedRecordValidator manifest walk: it reads the "filename" column
// (matched exactly, then case-insensitively with surrounding whitespace
// stripped — the ingestor's resolve rule) and resolves each value to a filename
// exactly as file_transfer does, appending ".txt" only when the value carries
// no known extension (hasKnownExtension mirrors _has_extension). The enforced
// record-format check runs over exactly these files.
//
// A missing "filename" column (or an empty CSV) yields an empty set: the CLI
// then validates nothing rather than a superset. A genuinely absent filename
// column is the ingestor's own separate failure (its validator errors "Missing
// required column: filename"), not a record-format one, so over-strictly
// rejecting text files here would diverge.
func manifestReferencedTextNames(csvPath string) (map[string]struct{}, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, fmt.Errorf("reading labels.csv: %w", err)
	}
	defer func() { _ = f.Close() }()

	br := bufio.NewReader(f)
	if head, _ := br.Peek(3); bytes.Equal(head, utf8BOM) {
		_, _ = br.Discard(3)
	}
	r := csv.NewReader(br)
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]struct{}{}, nil // empty CSV — another check's diagnostic
		}
		return nil, fmt.Errorf("reading labels.csv: %w", err)
	}
	col := filenameColumnIndex(header)
	referenced := map[string]struct{}{}
	if col < 0 {
		return referenced, nil // no filename column — the ingestor's own failure
	}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			continue // a malformed row is another check's diagnostic
		}
		if col >= len(rec) {
			continue
		}
		name := strings.TrimSpace(rec[col])
		if name == "" {
			continue
		}
		if !hasKnownExtension(name) {
			name += ".txt"
		}
		referenced[name] = struct{}{}
	}
	return referenced, nil
}

// filenameColumnIndex finds the manifest's filename column: an exact "filename"
// match first, then case-insensitively with surrounding whitespace stripped —
// the ingestor's resolve_column rule (default column name "filename"). Returns
// -1 when absent.
func filenameColumnIndex(header []string) int {
	for i, c := range header {
		if c == "filename" {
			return i
		}
	}
	for i, c := range header {
		if strings.ToLower(strings.TrimSpace(c)) == "filename" {
			return i
		}
	}
	return -1
}

// discoverSidecarFiles walks <root>/<dirName> (non-recursive) for files
// whose extension is in exts, rejecting symlinks and enforcing the
// single-file cap. Returns the absolute paths + their total size. A
// missing directory is an error (the caller's category requires it).
//
// Shared by the text family today; object detection / segmentation
// (annotations/, masks/) will reuse it in a later increment.
func discoverSidecarFiles(root, dirName string, exts map[string]struct{}) ([]string, int64, error) {
	dir := filepath.Join(root, dirName)
	dirStat, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, fmt.Errorf("missing %s/ subdirectory in %q", dirName, root)
		}
		return nil, 0, fmt.Errorf("stat %s/: %w", dirName, err)
	}
	if err := rejectSymlink(dirStat, dirName); err != nil {
		return nil, 0, err
	}
	if !dirStat.IsDir() {
		return nil, 0, fmt.Errorf("%q exists but is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s/: %w", dirName, err)
	}
	var (
		files []string
		total int64
	)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, ok := exts[strings.ToLower(filepath.Ext(entry.Name()))]; !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, 0, fmt.Errorf("stat %q: %w", entry.Name(), err)
		}
		if err := rejectSymlink(info, filepath.Join(dirName, entry.Name())); err != nil {
			return nil, 0, err
		}
		if info.Size() > MaxSingleFileBytes {
			return nil, 0, sizeError(filepath.Join(dirName, entry.Name()), info.Size(), MaxSingleFileBytes)
		}
		files = append(files, filepath.Join(dir, entry.Name()))
		total += info.Size()
	}
	return files, total, nil
}
