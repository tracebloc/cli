package push

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
// on — an images/ dir (image), a texts/ or sequences/ dir (text), or a
// lone CSV with none of those (tabular). It reads directory entries only;
// it opens no files and validates nothing.
//
// A file path is treated as tabular when it's a .csv (the "point at your
// single table" case). Anything we can't place — a missing path, a
// directory with no recognizable marker, an image+text mix — comes back
// Confident=false so the caller asks the family plainly.
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

	// A single file: only a .csv is placeable (tabular). Everything else is
	// ambiguous — the families all expect a directory.
	if !st.IsDir() {
		if strings.EqualFold(filepath.Ext(abs), ".csv") {
			return FamilySniff{Family: FamilyTabular, Confident: true,
				Echo: "Found a CSV table — this is tabular data."}
		}
		return FamilySniff{}
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return FamilySniff{}
	}
	dirs := map[string]bool{}
	var csvs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs[strings.ToLower(e.Name())] = true
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".csv") {
			csvs = append(csvs, e.Name())
		}
	}

	// An images/ directory is the image layout's tell; a texts/ or
	// sequences/ directory is the text family's. If a tree somehow has both
	// (unusual), stay ambiguous rather than guess.
	hasImages := dirs["images"]
	hasText := dirs["texts"] || dirs["sequences"]
	switch {
	case hasImages && !hasText:
		return FamilySniff{Family: FamilyImage, Confident: true,
			Echo: "Found labels.csv and an images/ folder — this is image data."}
	case hasText && !hasImages:
		dir := "texts/"
		if dirs["sequences"] {
			dir = "sequences/"
		}
		return FamilySniff{Family: FamilyText, Confident: true,
			Echo: fmt.Sprintf("Found labels.csv and a %s folder — this is text data.", dir)}
	case !hasImages && !hasText && len(csvs) > 0:
		return FamilySniff{Family: FamilyTabular, Confident: true,
			Echo: "Found a CSV table — this is tabular data."}
	default:
		return FamilySniff{}
	}
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
	// "point at your table" case) or the lone .csv in a directory.
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return abs, nil
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", err
	}
	var csvs []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".csv") {
			csvs = append(csvs, e.Name())
		}
	}
	sort.Strings(csvs)
	if len(csvs) == 0 {
		return "", fmt.Errorf("no .csv found in %q", abs)
	}
	return filepath.Join(abs, csvs[0]), nil
}
