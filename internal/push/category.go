package push

import "strings"

// CategorySpec is the single source of truth for one task category's
// CLI-relevant rules. It mirrors data-ingestors'
// tracebloc_ingestor/cli/conventions.py groupings so the CLI's
// per-category behaviour (which local layout to expect, which spec
// fields to emit, whether a label policy is needed) stays in lock-step
// with what the ingestor actually resolves.
//
// Everything category-shaped derives from the registry below — the
// family predicates, the `--category` help text, the interactive
// picker, and the push accept-gate — so the enumerations can't drift
// apart (they used to: the flag help listed 5 of 9, cli#74).
type CategorySpec struct {
	// ID is the canonical category identifier; it matches the
	// ingest.v1 schema enum (vendored via scripts/sync-schema.sh).
	ID string
	// Family selects the local layout + staging shape.
	Family Family
	// Label is the human-friendly name shown in the interactive picker.
	Label string
	// RegressionClass marks categories that predict a numeric target and
	// therefore need label.policy (object label form) so the raw target
	// never ships to the central backend by default.
	RegressionClass bool
	// CLISupported reports whether `dataset push` implements the category
	// today. semantic_/instance_segmentation are known (the schema
	// defines them) but not yet pushable.
	CLISupported bool
	// UnsupportedNote explains why a known-but-unimplemented category
	// isn't available yet; surfaced by the push gate. Empty when supported.
	UnsupportedNote string
}

// Family groups categories by local layout.
type Family int

const (
	// FamilyImage: a labels CSV + an images/ directory (plus, for some,
	// extra sidecar dirs like annotations/ or masks/).
	FamilyImage Family = iota
	// FamilyTabular: a single CSV whose columns are described by a
	// `schema` (column → SQL type) map. No sidecar files.
	FamilyTabular
	// FamilyText: a labels CSV + a directory of text files (texts/ for
	// classification, sequences/ for masked language modeling).
	FamilyText
)

// categoryRegistry is the ordered, authoritative list of every category
// the ingest.v1 schema defines. Order is the display order for help text
// and the interactive picker (CLI-supported first, in workflow order;
// the not-yet-implemented ones last). Adding a category to the schema
// means adding it here — the parity test pins the set.
var categoryRegistry = []CategorySpec{
	{ID: "image_classification", Family: FamilyImage, Label: "Image classification", CLISupported: true},
	{ID: "object_detection", Family: FamilyImage, Label: "Object detection", CLISupported: true},
	{ID: "keypoint_detection", Family: FamilyImage, Label: "Keypoint detection", CLISupported: true},
	{ID: "text_classification", Family: FamilyText, Label: "Text classification", CLISupported: true},
	{ID: "masked_language_modeling", Family: FamilyText, Label: "Masked language modeling", CLISupported: true},
	{ID: "tabular_classification", Family: FamilyTabular, Label: "Tabular classification", CLISupported: true},
	{ID: "tabular_regression", Family: FamilyTabular, Label: "Tabular regression", RegressionClass: true, CLISupported: true},
	{ID: "time_series_forecasting", Family: FamilyTabular, Label: "Time-series forecasting", RegressionClass: true, CLISupported: true},
	{ID: "time_to_event_prediction", Family: FamilyTabular, Label: "Time-to-event prediction", RegressionClass: true, CLISupported: true},
	{ID: "semantic_segmentation", Family: FamilyImage, Label: "Semantic segmentation", CLISupported: false,
		UnsupportedNote: "blocked on the ingestor's mask-sidecar support (data-ingestors#136)"},
	{ID: "instance_segmentation", Family: FamilyImage, Label: "Instance segmentation", CLISupported: false,
		UnsupportedNote: "not implemented"},
}

// categoryByID indexes the registry for O(1) lookup, built once.
var categoryByID = func() map[string]CategorySpec {
	m := make(map[string]CategorySpec, len(categoryRegistry))
	for _, c := range categoryRegistry {
		m[c.ID] = c
	}
	return m
}()

// Lookup returns the spec for a category id and whether it is known.
func Lookup(category string) (CategorySpec, bool) {
	c, ok := categoryByID[category]
	return c, ok
}

// IsKnown reports whether category is a recognized task category (in the
// schema), supported by the CLI or not.
func IsKnown(category string) bool {
	_, ok := categoryByID[category]
	return ok
}

// IsCLISupported reports whether `dataset push` implements category today.
func IsCLISupported(category string) bool { return categoryByID[category].CLISupported }

// IsImage reports whether category uses the labels.csv + images/ layout.
func IsImage(category string) bool {
	c, ok := categoryByID[category]
	return ok && c.Family == FamilyImage
}

// IsTabular reports whether category uses the single-CSV + schema layout.
func IsTabular(category string) bool {
	c, ok := categoryByID[category]
	return ok && c.Family == FamilyTabular
}

// IsText reports whether category uses the labels.csv + text-file dir layout.
func IsText(category string) bool {
	c, ok := categoryByID[category]
	return ok && c.Family == FamilyText
}

// IsRegressionClass reports whether category predicts a numeric target and
// therefore needs label.policy (object label form).
func IsRegressionClass(category string) bool { return categoryByID[category].RegressionClass }

// SupportedCategoryIDs returns the ids `dataset push` supports, in display
// order. Used to build the --category help, the interactive picker, and
// the accept-gate's "Supported:" lists from one place.
func SupportedCategoryIDs() []string {
	ids := make([]string, 0, len(categoryRegistry))
	for _, c := range categoryRegistry {
		if c.CLISupported {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// AllCategoryIDs returns every recognized category id, in registry order.
func AllCategoryIDs() []string {
	ids := make([]string, 0, len(categoryRegistry))
	for _, c := range categoryRegistry {
		ids = append(ids, c.ID)
	}
	return ids
}

// SupportedCategoriesList is the comma-joined supported ids, for help text
// and gate error messages.
func SupportedCategoriesList() string { return strings.Join(SupportedCategoryIDs(), ", ") }

// TextSidecarDir returns the sidecar directory name a text category
// expects: "sequences" for masked_language_modeling, "texts" for
// text_classification. (Used both as the local subdir to stage and the
// spec field to emit.)
func TextSidecarDir(category string) string {
	if category == "masked_language_modeling" {
		return "sequences"
	}
	return "texts"
}
