package push

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// reproducible-build tests) sort before use. Empty for non-image
	// categories (which use Sidecars instead).
	Images []string

	// Sidecars maps a sidecar directory name (e.g. "texts",
	// "sequences", "annotations", "masks") to the absolute paths of
	// the files in it. Each is staged under "<name>/<basename>" — the
	// generic counterpart to Images, used by the text family (and,
	// later, object detection / segmentation). nil for image_
	// classification (which uses Images) and tabular (no sidecars).
	Sidecars map[string][]string

	// TotalBytes is the sum of all files Discover will stage —
	// labels.csv plus every entry in Images / Sidecars.
	// Pre-computed during the walk so the size-cap check + the
	// progress bar can read it without re-stat'ing.
	TotalBytes int64
}

// FileCount returns the total number of files this layout stages:
// labels.csv, and every Images / Sidecars entry. Used for the
// "staging N files" messaging so it's accurate across all category
// families.
func (l *LocalLayout) FileCount() int {
	n := 1 // labels.csv
	n += len(l.Images)
	for _, files := range l.Sidecars {
		n += len(files)
	}
	return n
}

// imageExtensions accepts the file types the in-cluster ingestor can
// actually validate: data-ingestors' FileExtension enum + the
// ingest.v1 schema's file_options.extension enum both allow exactly
// .jpg, .jpeg, and .png for images. (.webp was listed here previously
// on the strength of a chart-defaults comment, but the ingestor's
// FileTypeValidator REJECTS it at construction — accepting it locally
// meant a full upload followed by a guaranteed in-cluster failure.
// cli#68 / backend#828 P5: this list and the emitted
// file_options.extension keep the two sides in lock-step.)
//
// Comparison is case-insensitive — filesystems vary (case-sensitive
// on Linux, case-preserving-but-insensitive on macOS default APFS).
var imageExtensions = map[string]struct{}{
	".jpg":  {},
	".jpeg": {},
	".png":  {},
}

// Discover walks rootDir and validates it matches the layout Phase 3
// expects for image_classification:
//
//   - <root>/labels.csv  (required)
//   - <root>/images/*.{jpg,jpeg,png}  (at least one file, one type)
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

	// labels.csv (required). Use Lstat — NOT Stat — so a symlink
	// shows up as a symlink (mode includes ModeSymlink) rather
	// than being silently followed. v0.1 rejects symlinks entirely
	// (see rejectSymlink); without Lstat the size cap below would
	// see the symlink's own ~100-byte size while writeTarFile
	// (which uses os.Stat → follows symlinks) would happily stream
	// the target's full contents — a size-cap bypass and an
	// arbitrary-local-file disclosure to the cluster PVC. Bugbot
	// flagged this as Medium-severity security on PR-b round 4.
	labelsPath := filepath.Join(abs, "labels.csv")
	labelsStat, err := os.Lstat(labelsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf(
				"missing labels.csv in %q. The CLI expects "+
					"<dir>/labels.csv + <dir>/images/ for image_classification; "+
					"see https://docs.tracebloc.io for the dataset layout.",
				abs)
		}
		return nil, fmt.Errorf("stat labels.csv: %w", err)
	}
	if err := rejectSymlink(labelsStat, "labels.csv"); err != nil {
		return nil, err
	}
	if labelsStat.IsDir() {
		// A directory literally named "labels.csv" passes the
		// os.Stat above — without this check the pre-flight would
		// accept it, and PR-b's tar stream would fail confusingly
		// trying to read a directory as a CSV. Symmetric with the
		// imagesStat.IsDir() check below.
		return nil, fmt.Errorf(
			"%q is a directory, not a file. labels.csv must be the "+
				"CSV file holding the filename,label rows.",
			labelsPath)
	}
	if err := checkFileSize("labels.csv", labelsStat.Size()); err != nil {
		return nil, err
	}
	layout.LabelsCSV = labelsPath
	layout.TotalBytes += labelsStat.Size()

	// images/ subdir (required, must contain at least one
	// image-extension file). Use os.Lstat — NOT Stat — so a
	// symlinked-directory case (e.g. `ln -s /etc images`) gets
	// caught here, not silently followed into the linked path.
	// Without Lstat, the symlink-image fixes from the previous
	// commit don't matter: the whole directory could be a link.
	// Bugbot flagged the missing dir-level Lstat on PR-b round 5.
	imagesDir := filepath.Join(abs, "images")
	imagesStat, err := os.Lstat(imagesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf(
				"missing images/ subdirectory in %q. The CLI expects "+
					"<dir>/labels.csv + <dir>/images/*.{jpg,jpeg,png}.",
				abs)
		}
		return nil, fmt.Errorf("stat images/: %w", err)
	}
	if err := rejectSymlink(imagesStat, "images"); err != nil {
		return nil, err
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
	skippedExts := map[string]int{}
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
			if ext != "" {
				skippedExts[ext]++
			}
			continue
		}
		// entry.Info() returns Lstat-like metadata for the
		// directory entry (the symlink's own mode if it's a
		// symlink, not the target's). That's exactly what we
		// want here — combined with rejectSymlink it closes the
		// symlink-bypass-size-caps hole Bugbot flagged on PR-b
		// round 4.
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat %q: %w", entry.Name(), err)
		}
		if err := rejectSymlink(info, filepath.Join("images", entry.Name())); err != nil {
			return nil, err
		}
		if err := checkFileSize(filepath.Join("images", entry.Name()), info.Size()); err != nil {
			return nil, err
		}
		layout.Images = append(layout.Images, filepath.Join(imagesDir, entry.Name()))
		layout.TotalBytes += info.Size()
	}

	if len(layout.Images) == 0 {
		if len(skippedExts) > 0 {
			return nil, fmt.Errorf(
				"no usable image files in %q — found %s, but the ingestor "+
					"accepts only .jpg, .jpeg, or .png. Convert the images "+
					"and re-run.",
				imagesDir, extCounts(skippedExts))
		}
		return nil, fmt.Errorf(
			"no image files found in %q. Expected .jpg, .jpeg, or .png; "+
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
			HumanBytes(layout.TotalBytes), HumanBytes(MaxTotalBytes))
	}

	return layout, nil
}

// rejectSymlink returns a non-nil error if info describes a symlink.
// v0.1 refuses symlinks under <root>/{labels.csv, images, images/*}
// entirely because:
//
//   - SECURITY: writeTarFile uses os.Open (which follows symlinks).
//     Discover sized entries via DirEntry.Info() (which does not).
//     A symlink whose target is a multi-GB local file would pass
//     Discover's size cap (the symlink itself is ~100 bytes) yet
//     stream the target's full contents to the cluster PVC — a
//     size-cap bypass and arbitrary-local-file disclosure to the
//     cluster admin. Bugbot caught this on PR-b round 4.
//   - UX: legitimate image_classification datasets don't use
//     symlinks. A clear "symlinks not supported" error is better
//     than the alternative fixes (resolve + re-stat the target,
//     blanket Stat() everywhere) — both of which would let the
//     security hole creep back in on a future refactor.
//
// Customers with symlink-heavy layouts (rare; usually means their
// data lives on another filesystem) can `cp -L` to materialize
// the files before pushing.
//
// Known limitation: HARD LINKS are NOT rejected. The filesystem
// doesn't expose a Mode bit for hard links the way ModeSymlink
// distinguishes symlinks, and a high-Nlink check has too many
// false positives (legitimate hard-linked datasets where the
// dataset dir is the only entry point). The implicit security
// boundary is the CLI's process-level read permissions: a
// customer can only hard-link files they already have read
// access to, so the cluster admin reading a hard-linked
// /etc/shadow via the PVC isn't a privilege escalation — they'd
// need the CLI to be running as root for that to be exploitable,
// and the docs already recommend running as a low-privileged
// user. v0.2 may add openat(O_NOFOLLOW)-based sandboxing if a
// real customer needs harder isolation; tracked alongside the
// cloud-source story (#147 non-goals).
func rejectSymlink(info os.FileInfo, relPath string) error {
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	return fmt.Errorf(
		"%q is a symbolic link, which v0.1 does not allow in the dataset "+
			"layout (security: a symlink could escape the dataset tree or "+
			"bypass size caps). Materialize the link target (e.g. `cp -L`) "+
			"and re-run, or wait for v0.2's cloud-source story if the data "+
			"lives elsewhere.",
		relPath)
}

// checkFileSize returns a sizeError when size exceeds the single-file cap
// (MaxSingleFileBytes), else nil. It centralizes the identical
// `size > MaxSingleFileBytes` guard that the image-layout, stream, tabular, and
// text paths each repeated, so the boundary lives in exactly one place — and can be
// asserted at the exact cap in a unit test without materializing a
// MaxSingleFileBytes-sized (500 MB) fixture. The cap is `>` not `>=`: a file of
// exactly MaxSingleFileBytes is allowed.
func checkFileSize(relPath string, size int64) error {
	if size > MaxSingleFileBytes {
		return sizeError(relPath, size, MaxSingleFileBytes)
	}
	return nil
}

// sizeError builds the over-the-single-file-cap error with the same
// human-readable framing as the total-cap branch above. Centralized
// so the message stays consistent if we tune the wording later.
func sizeError(relPath string, got, cap int64) error {
	return fmt.Errorf(
		"file %q is %s, exceeds v0.1 single-file cap of %s. "+
			"For larger files, see tracebloc/client#147's v0.2 cloud-source story.",
		relPath, HumanBytes(got), HumanBytes(cap))
}

// HumanBytes renders a byte count in the shortest readable unit.
// Not internationalized — the CLI is English-only for v0.1.
//
// Exported because the CLI's pre-flight summary (internal/cli) needs
// the identical formatting — keeping one implementation here means
// the size shown in an over-cap error and the size shown in the
// dry-run summary can never drift (Bugbot flagged the earlier
// copy-pasted variant on PR #8).
func HumanBytes(n int64) string {
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

// DetectExtension returns the single file extension (lowercased, with
// dot) shared by every image in the set. The in-cluster ingestor's
// FileTypeValidator enforces ONE extension per dataset, so a mixed set
// would pass local preflight and then fail in-cluster after the full
// upload — the exact cli#68 failure class. Detecting and emitting it
// here (spec.file_options.extension) keeps the preflight's promise:
// if this passes, the ingestor's file-type check passes too.
func DetectExtension(images []string) (string, error) {
	if len(images) == 0 {
		return "", fmt.Errorf("no image files to detect a type from")
	}
	counts := map[string]int{}
	for _, img := range images {
		counts[strings.ToLower(filepath.Ext(img))]++
	}
	if len(counts) == 1 {
		for ext := range counts {
			return ext, nil
		}
	}
	return "", fmt.Errorf(
		"your images mix file types (%s) — the ingestor requires one type "+
			"per dataset. Convert them to a single type, or split them into "+
			"separate tables.",
		extCounts(counts))
}

// extCounts renders {".png": 3, ".jpg": 120} as ".jpg ×120, .png ×3"
// (sorted for stable output).
func extCounts(counts map[string]int) string {
	exts := make([]string, 0, len(counts))
	for ext := range counts {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	parts := make([]string, 0, len(exts))
	for _, ext := range exts {
		parts = append(parts, fmt.Sprintf("%s ×%d", ext, counts[ext]))
	}
	return strings.Join(parts, ", ")
}
