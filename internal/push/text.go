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
//   - <root>/tokenizer.json      (required for masked_language_modeling)
//
// The returned layout stages the CSV (as labels.csv), the text files
// under "<sidecar>/", and — for MLM — tokenizer.json at the table root
// (the ingestor reads it from SRC_PATH/tokenizer.json for [MASK]/[PAD]).
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
		Root:       abs,
		Sidecars:   map[string][]string{},
		ExtraFiles: map[string]string{},
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

	// masked_language_modeling needs a tokenizer.json at the root.
	if category == "masked_language_modeling" {
		tokPath := filepath.Join(abs, "tokenizer.json")
		tokStat, terr := os.Lstat(tokPath)
		if terr != nil {
			if errors.Is(terr, os.ErrNotExist) {
				return nil, fmt.Errorf(
					"missing tokenizer.json in %q. masked_language_modeling requires a "+
						"tokenizer.json (HuggingFace tokenizers format) at the dataset root; "+
						"the ingestor reads it for the [MASK]/[PAD] tokens.", abs)
			}
			return nil, fmt.Errorf("stat tokenizer.json: %w", terr)
		}
		if err := rejectSymlink(tokStat, "tokenizer.json"); err != nil {
			return nil, err
		}
		if tokStat.IsDir() {
			return nil, fmt.Errorf("%q is a directory, not a file", tokPath)
		}
		if tokStat.Size() > MaxSingleFileBytes {
			return nil, sizeError("tokenizer.json", tokStat.Size(), MaxSingleFileBytes)
		}
		layout.ExtraFiles["tokenizer.json"] = tokPath
		layout.TotalBytes += tokStat.Size()
	}

	if layout.TotalBytes > MaxTotalBytes {
		return nil, fmt.Errorf(
			"dataset is %s, exceeds v0.1 cap of %s. For larger datasets, the "+
				"cloud-source path is on the v0.2 roadmap (tracebloc/client#147).",
			HumanBytes(layout.TotalBytes), HumanBytes(MaxTotalBytes))
	}
	return layout, nil
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
