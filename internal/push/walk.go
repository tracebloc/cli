package push

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Size limits enforced before we touch the cluster. Both caps are
// soft engineering choices, not protocol limits — they exist
// because tar-over-exec via client-go's remotecommand executor has
// a memory profile that degrades steeply past ~1 GB total transfer.
// Customers hitting these get pointed at the v0.2 cloud-source
// story (S3/GCS/HTTPS sources, currently in epic non-goals).
//
// The single-file cap is stricter than the total cap because the
// streaming buffer for one file lives in memory longer than the
// inter-file overhead — a 1 GB single file is worse than ten 100 MB
// files for the executor's working-set.
const (
	// MaxTotalBytes is the v0.1 ceiling on the sum of all files in
	// a single `dataset push`. Picked from the epic's stated
	// "anything above ~1GB needs the cloud-source story (v0.2)."
	MaxTotalBytes int64 = 1 * 1024 * 1024 * 1024

	// MaxSingleFileBytes caps any single file. Tuned via the
	// data-ingestors templates' largest sample image (~30 KB) and
	// a 10000x safety margin for typical user uploads. Files in the
	// hundreds-of-MB range work in testing but degrade noticeably.
	MaxSingleFileBytes int64 = 500 * 1024 * 1024
)

// LocalLayout describes a validated local directory ready to stage.
// All paths are absolute, resolved against the customer's working
// directory before this struct is returned.
type LocalLayout struct {
	// Root is the absolute path the customer passed (after cleanup).
	Root string

	// LabelsCSV is the absolute path to labels.csv inside Root.
	// Required for image_classification.
	LabelsCSV string

	// Images is the list of absolute paths to image files under
	// Root/images/. Order is filesystem-walk order — Discover
	// doesn't sort, so callers that need determinism (e.g.
	// reproducible-build tests) sort before use.
	Images []string

	// TotalBytes is the sum of all files Discover will stage —
	// labels.csv plus every entry in Images. Pre-computed during
	// the walk so the size-cap check + the progress bar (PR-b)
	// can read it without re-stat'ing.
	TotalBytes int64
}

// imageExtensions accepts the file types the chart's
// image_classification ingestor processes by default. From
// data-ingestors' FileTypeValidator(images) defaults: .jpg, .jpeg,
// .png. The chart's defaults file (see chartversion 1.3.5+) also
// accepts .webp; we mirror that here so customers on a recent
// chart can stage webp files without hitting "no images found."
//
// Comparison is case-insensitive — filesystems vary (case-sensitive
// on Linux, case-preserving-but-insensitive on macOS default APFS).
var imageExtensions = map[string]struct{}{
	".jpg":  {},
	".jpeg": {},
	".png":  {},
	".webp": {},
}

// Discover walks rootDir and validates it matches the layout Phase 3
// expects for image_classification:
//
//   - <root>/labels.csv  (required)
//   - <root>/images/*.{jpg,jpeg,png,webp}  (at least one file)
//
// Returns specific errors keyed to the layout mistakes a customer
// is most likely to hit — these surface as the CLI's diagnostic
// output before any cluster work, so they're a primary UX surface.
//
// Enforces both v0.1 size caps (MaxTotalBytes, MaxSingleFileBytes);
// over-cap returns ErrTooBig with a pointer to the cloud-source
// story.
func Discover(rootDir string) (*LocalLayout, error) {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolving %q: %w", rootDir, err)
	}

	st, err := os.Stat(abs)
	if err != nil {
		// Stat covers both "path doesn't exist" and "permission
		// denied" via the wrapped fs.PathError; the customer sees
		// the underlying message which is already clear.
		return nil, fmt.Errorf("reading dataset directory %q: %w", abs, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf(
			"%q is not a directory; pass the directory containing labels.csv + images/",
			abs)
	}

	layout := &LocalLayout{Root: abs}

	// labels.csv (required).
	labelsPath := filepath.Join(abs, "labels.csv")
	labelsStat, err := os.Stat(labelsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf(
				"missing labels.csv in %q. The CLI expects "+
					"<dir>/labels.csv + <dir>/images/ for image_classification; "+
					"see https://github.com/tracebloc/client/issues/147 for the "+
					"v0.1 layout contract.",
				abs)
		}
		return nil, fmt.Errorf("stat labels.csv: %w", err)
	}
	if labelsStat.Size() > MaxSingleFileBytes {
		return nil, sizeError("labels.csv", labelsStat.Size(), MaxSingleFileBytes)
	}
	layout.LabelsCSV = labelsPath
	layout.TotalBytes += labelsStat.Size()

	// images/ subdir (required, must contain at least one
	// image-extension file).
	imagesDir := filepath.Join(abs, "images")
	imagesStat, err := os.Stat(imagesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf(
				"missing images/ subdirectory in %q. The CLI expects "+
					"<dir>/labels.csv + <dir>/images/*.{jpg,jpeg,png,webp}.",
				abs)
		}
		return nil, fmt.Errorf("stat images/: %w", err)
	}
	if !imagesStat.IsDir() {
		return nil, fmt.Errorf("%q exists but is not a directory", imagesDir)
	}

	// Walk just the images/ directory — we don't recurse, image
	// classification's layout is flat. If a customer has nested
	// subdirs (e.g. images/cats/ + images/dogs/), that's a
	// different category convention and out of scope for v0.1.
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		return nil, fmt.Errorf("reading images/: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			// Silently skip subdirectories so a stray .DS_Store or
			// thumbnails dir doesn't error out the whole walk.
			// We DO surface the count of accepted images at the
			// end, so a customer with all-nested-subdirs gets
			// "0 images found" which is the right diagnostic.
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if _, ok := imageExtensions[ext]; !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat %q: %w", entry.Name(), err)
		}
		if info.Size() > MaxSingleFileBytes {
			return nil, sizeError(filepath.Join("images", entry.Name()),
				info.Size(), MaxSingleFileBytes)
		}
		layout.Images = append(layout.Images, filepath.Join(imagesDir, entry.Name()))
		layout.TotalBytes += info.Size()
	}

	if len(layout.Images) == 0 {
		return nil, fmt.Errorf(
			"no image files found in %q. Expected .jpg, .jpeg, .png, or .webp; "+
				"got %d non-image entries.",
			imagesDir, len(entries))
	}

	if layout.TotalBytes > MaxTotalBytes {
		return nil, fmt.Errorf(
			"dataset is %s, exceeds v0.1 cap of %s. "+
				"For larger datasets, the cloud-source path (S3/GCS/HTTPS) "+
				"is on the v0.2 roadmap — see tracebloc/client#147 non-goals. "+
				"Workaround for v0.1: split the push into multiple smaller "+
				"tables, or stage directly via the existing helm flow.",
			humanBytes(layout.TotalBytes), humanBytes(MaxTotalBytes))
	}

	return layout, nil
}

// sizeError builds the over-the-single-file-cap error with the same
// human-readable framing as the total-cap branch above. Centralized
// so the message stays consistent if we tune the wording later.
func sizeError(relPath string, got, cap int64) error {
	return fmt.Errorf(
		"file %q is %s, exceeds v0.1 single-file cap of %s. "+
			"For larger files, see tracebloc/client#147's v0.2 cloud-source story.",
		relPath, humanBytes(got), humanBytes(cap))
}

// humanBytes renders a byte count in the shortest readable unit.
// Not internationalized — the CLI is English-only for v0.1.
func humanBytes(n int64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
