package push

import (
	"errors"
	"fmt"
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
	if rf, ok := RecordFormatFor(category); ok && rf.Enforced {
		if err := validateTextRecords(dirName, files, rf); err != nil {
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

// validateTextRecords runs the enforced record-format check over every
// discovered .txt in dirName, mirroring the ingestor's per-file
// TabSeparatedRecordValidator. The first malformed file fails discovery with a
// message naming the offending file (relative to the dataset root, e.g.
// "texts/bad.txt"), so the fix is obvious without reaching the cluster.
func validateTextRecords(dirName string, files []string, rf RecordFormat) error {
	for _, f := range files {
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
