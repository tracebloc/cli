package push

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FamilySniff is the result of a thin, read-only layout preview: which
// family the data looks like, whether we're confident enough to skip
// asking, and a friendly one-liner to echo back when we are.
//
// It is a HINT, not a lock (RFC-0002 §5.1): an explicit --task always
// wins and skips the sniff entirely, and an ambiguous layout (Confident
// == false) means "ask the user plainly" rather than guessing. The sniff
// never validates — it only looks for the layout markers Discover*
// keys on, so it can't drift from (or replace) the real walk that runs
// later. If the sniff and the real walk ever disagree, the walk wins and
// reports the real error.
type FamilySniff struct {
	Family    Family
	Confident bool
	// Echo is the plain-language confirmation for the confident case,
	// e.g. "Found a CSV table — this is tabular data." Empty when not
	// confident.
	Echo string
}

// SniffFamily previews the family of the dataset at path by looking for
// the same layout markers Discover / DiscoverText / DiscoverTabular key
// on — labels.csv + an images/ dir (image), labels.csv + a texts/ or
// sequences/ dir (text), exactly one CSV in a directory with none of
// those (tabular), or a bare .csv file (tabular). It reads directory
// entries only; it opens no files and validates nothing.
//
// It never claims more than the matching Discover* would accept: the
// marker directories (images/, texts/, sequences/) and labels.csv are
// matched with the SAME literal, case-sensitive names the walk joins and
// Lstats — a mis-cased "Images/" is not the walk's marker, so it is not
// sniffed as confident image. Image / text are confident only when BOTH
// labels.csv AND the subdir are present, mirroring Discover / DiscoverText.
// Tabular is confident on EXACTLY ONE CSV in a directory, mirroring
// DiscoverTabular's findSingleCSV count rule — a directory with two or more
// CSVs is a layout the tabular walk refuses, so the sniff must not
// confidently place it either — OR on a bare .csv file, which
// DiscoverTabular now stages as the one CSV under the dataset (#181). Only
// the .csv extension match stays case-insensitive, mirroring
// DiscoverTabular's EqualFold.
//
// The media/label families (image, text) require a directory, so a bare
// file that is not a .csv is never a confident sniff. Anything we can't
// place — a missing path, a non-.csv bare file, a directory with no
// recognizable marker, an image+text mix, an image/text dir without
// labels.csv, a multi-CSV directory the tabular walk would reject — comes
// back Confident=false so the caller asks the family plainly.
func SniffFamily(path string) FamilySniff {
	abs, err := filepath.Abs(path)
	if err != nil {
		return FamilySniff{}
	}
	st, err := os.Stat(abs)
	if err != nil {
		// Missing / unreadable: can't sniff. The real walk will produce the
		// actionable error once a task is chosen.
		return FamilySniff{}
	}

	// A bare file: only a .csv is placeable (tabular), mirroring the
	// bare-file shape DiscoverTabular accepts (#181). The image / text
	// families need a directory, so any other bare file stays ambiguous.
	//
	// Lstat first: os.Stat above followed a symlink, but DiscoverTabular
	// stats the CSV with Lstat and rejectSymlinks it — so a symlinked .csv
	// is a layout the walk REFUSES. Sniffing it as confident tabular would
	// break this func's "never claims more than the matching Discover* would
	// accept" contract (it'd lock the guided flow to tabular, then hard-fail
	// on the symlink guard). Treat a symlink like any other unplaceable file.
	if !st.IsDir() {
		if li, lerr := os.Lstat(abs); lerr == nil &&
			li.Mode()&os.ModeSymlink == 0 && isCSV(abs) {
			return FamilySniff{Family: FamilyTabular, Confident: true,
				Echo: "Found a CSV table — this is tabular data."}
		}
		return FamilySniff{}
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return FamilySniff{}
	}
	// Match the walk's markers with the literal, case-sensitive names it
	// uses (filepath.Join + os.Lstat on "images" / "texts" / "sequences" /
	// "labels.csv"). The .csv extension check is case-insensitive to mirror
	// DiscoverTabular's EqualFold.
	var hasImages, hasTexts, hasSequences, hasLabels bool
	// miscasedMarker flags a subdir that matches a marker name only
	// case-insensitively (e.g. "Images", "Texts") — a likely mis-cased
	// media folder. The walk keys on the literal lowercase name, so such a
	// dir is NOT a marker to it; but its lone labels.csv would otherwise
	// fall through to the confident-tabular branch and get silently
	// ingested as a table, images/texts dropped. When we see one, stay
	// ambiguous and ask the family plainly.
	miscasedMarker := false
	csvCount := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			switch name {
			case "images":
				hasImages = true
			case "texts":
				hasTexts = true
			case "sequences":
				hasSequences = true
			default:
				if isMarkerFold(name) {
					miscasedMarker = true
				}
			}
			continue
		}
		if name == "labels.csv" {
			hasLabels = true
		}
		if isCSV(name) {
			csvCount++
		}
	}
	hasText := hasTexts || hasSequences

	// An images/ directory is the image layout's tell; a texts/ or
	// sequences/ directory is the text family's. Both require labels.csv
	// (as Discover / DiscoverText do). If a tree has both marker dirs
	// (unusual), stay ambiguous rather than guess.
	switch {
	case hasImages && !hasText && hasLabels:
		return FamilySniff{Family: FamilyImage, Confident: true,
			Echo: "Found labels.csv and an images/ folder — this is image data."}
	case hasText && !hasImages && hasLabels:
		dir := "texts/"
		if hasSequences {
			dir = "sequences/"
		}
		// "looks like", not "is": the family is confident, but texts/ and
		// sequences/ map to DIFFERENT tasks (DiscoverText keys the sidecar
		// dir off TextSidecarDir — texts/ for classification, sequences/ for
		// MLM). So a confident text sniff must not imply the task the user
		// then picks will load; the picker offers the whole text family, and
		// the walk gives the authoritative error if the layout and the
		// chosen task disagree.
		return FamilySniff{Family: FamilyText, Confident: true,
			Echo: fmt.Sprintf("Found labels.csv and a %s folder — this looks like text data.", dir)}
	case !hasImages && !hasText && !miscasedMarker && csvCount == 1:
		// Exactly one CSV, mirroring DiscoverTabular's findSingleCSV rule.
		// Two or more CSVs is a directory the tabular walk rejects, so stay
		// ambiguous rather than confidently promise a layout it refuses.
		// A mis-cased marker dir alongside the CSV (miscasedMarker) also
		// bails to ambiguous: the lone labels.csv of an image/text layout
		// whose media folder was mis-cased must not masquerade as a table.
		return FamilySniff{Family: FamilyTabular, Confident: true,
			Echo: "Found a CSV table — this is tabular data."}
	default:
		return FamilySniff{}
	}
}

// isMarkerFold reports whether name is one of the media-folder markers
// (images / texts / sequences) ignoring case. Used only to detect a
// mis-cased marker dir; the confident image/text branches still require an
// EXACT match, mirroring the walk's literal os.Lstat.
func isMarkerFold(name string) bool {
	for _, m := range []string{"images", "texts", "sequences"} {
		if strings.EqualFold(name, m) {
			return true
		}
	}
	return false
}

// PreviewLabelHeaders returns the column names of the CSV a label column
// would be chosen from, so the interactive flow can offer the REAL header
// row instead of free text — an exact-match choice that kills the
// case-mismatch silent-null-label class (data-ingestors#340). It's a
// preview read: it locates the CSV the way the matching Discover* would
// (the single CSV for tabular — or the file itself if a file was passed —
// labels.csv for image / text) and reads only its header.
//
// It validates nothing; any failure (no CSV, unreadable, empty) comes
// back as an error the caller treats as "fall back to free-text entry",
// never a hard stop.
func PreviewLabelHeaders(category, root string) ([]string, error) {
	csvPath, err := previewLabelCSVPath(category, root)
	if err != nil {
		return nil, err
	}
	return ReadCSVHeader(csvPath)
}

// previewLabelCSVPath resolves which CSV holds the label column for a
// category's layout, mirroring the Discover* file conventions without
// re-validating them.
func previewLabelCSVPath(category, root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if !IsTabular(category) {
		// image / text families: the label lives in labels.csv.
		return filepath.Join(abs, "labels.csv"), nil
	}
	// Tabular: the dataset IS a single CSV. Accept a direct file path (the
	// "point at your table" case) or resolve the lone .csv in a directory
	// via the SAME single-CSV rule DiscoverTabular enforces — including its
	// exactly-one requirement, so a multi-CSV directory errors here (and the
	// caller falls back to free-text entry) instead of silently reading the
	// alphabetically-first file's header.
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return abs, nil
	}
	return findSingleCSV(abs)
}
