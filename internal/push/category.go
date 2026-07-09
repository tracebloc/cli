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
// family predicates, the `--task` help text, the interactive
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
	// Gloss, when set, is the name users actually search for — it wins over
	// Label in the picker. A few tasks have a technical id + label but a
	// far more recognizable common name (time_to_event_prediction is
	// "survival analysis"; masked_language_modeling is "fill-mask";
	// seq2seq is "translation / summarization"). Empty ⇒ show Label.
	Gloss string
	// Blurb is the one-line "what is this for?" shown after the display name
	// in the picker ("Display — blurb · task_id"). Plain and concrete so a
	// user can tell tasks apart without leaving the terminal.
	Blurb string
	// RegressionClass marks categories that predict a numeric target and
	// therefore need label.policy (object label form) so the raw target
	// never ships to the central backend by default.
	RegressionClass bool
	// SelfSupervised marks text categories that train without an explicit
	// label column — no `label` travels in labels.csv. For MLM/CLM the target
	// is derived from the text itself (mask a token; predict the next one); for
	// seq2seq and embeddings it comes from the record's own paired fields
	// (source→target, anchor/positive/negative). Either way there's no label
	// column, so the interactive flow skips the "which column is the label?"
	// question. A registry fact rather than a hardcoded id list so a new
	// self-supervised task can't be added without deciding this
	// (SelfSupervisedText reads it). Mirrors the ingestor registry's
	// is_self_supervised (data-ingestors modalities/registry.py).
	SelfSupervised bool
	// IsClassification marks categories the ingestor treats as classification
	// (registry ModalitySpec.is_classification) — the ones whose validator
	// chain gets a LabelDiversityValidator, so the dataset needs >= 2 distinct
	// labels. Mirrors the ingestor exactly: the image family +
	// text_classification + sentence_pair_classification + tabular_classification
	// are true; token_classification is NOT (its labels are BIO tag sequences,
	// checked by BIOLabelValidator, not class labels), nor are the regression /
	// self-supervised tasks. The label-diversity preflight reads it so it can't
	// drift from the ingestor's wiring.
	IsClassification bool
	// CLISupported reports whether `dataset push` implements the category
	// today. semantic_segmentation is known (the schema defines it) but
	// not yet pushable.
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

// categoryRegistry is the ordered list of every category the ingest.v1
// schema defines — nothing more, nothing less. Order is the display order for
// help text and the interactive picker (CLI-supported first, in workflow
// order; the not-yet-implemented ones last). Adding a category to the schema
// means adding it here; TestRegistryCoversSchemaCategories +
// TestRegistryWithinSchema pin the set equal to the schema enum both ways, so
// it can neither fall behind (a schema category rejected as "unrecognized")
// nor carry an extra the ingestor won't accept (the instance_segmentation
// half-ingest class — data-ingestors #240/#99, #1005).
var categoryRegistry = []CategorySpec{
	{ID: "image_classification", Family: FamilyImage, Label: "Image classification", CLISupported: true, IsClassification: true,
		Blurb: "sort images into classes"},
	{ID: "object_detection", Family: FamilyImage, Label: "Object detection", CLISupported: true, IsClassification: true,
		Blurb: "draw boxes around objects in an image"},
	{ID: "keypoint_detection", Family: FamilyImage, Label: "Keypoint detection", CLISupported: true, IsClassification: true,
		Blurb: "locate landmark points on an image (e.g. pose)"},
	{ID: "text_classification", Family: FamilyText, Label: "Text classification", CLISupported: true, IsClassification: true,
		Blurb: "sort text snippets into classes"},
	{ID: "masked_language_modeling", Family: FamilyText, Label: "Masked language modeling", Gloss: "fill-mask", CLISupported: true, SelfSupervised: true,
		Blurb: "predict masked-out words — no labels needed"},
	{ID: "tabular_classification", Family: FamilyTabular, Label: "Tabular classification", CLISupported: true, IsClassification: true,
		Blurb: "predict a class from table columns"},
	{ID: "tabular_regression", Family: FamilyTabular, Label: "Tabular regression", RegressionClass: true, CLISupported: true,
		Blurb: "predict a number from table columns"},
	{ID: "time_series_forecasting", Family: FamilyTabular, Label: "Time-series forecasting", RegressionClass: true, CLISupported: true,
		Blurb: "predict future values from past ones"},
	{ID: "time_to_event_prediction", Family: FamilyTabular, Label: "Time-to-event prediction", Gloss: "Survival analysis", RegressionClass: true, CLISupported: true,
		Blurb: "predict how long until an event happens"},
	{ID: "causal_language_modeling", Family: FamilyText, Label: "Causal language modeling", CLISupported: true, SelfSupervised: true,
		Blurb: "predict the next word in a sequence"},
	{ID: "seq2seq", Family: FamilyText, Label: "Sequence-to-sequence", Gloss: "translation / summarization", CLISupported: true, SelfSupervised: true,
		Blurb: "map an input sequence to an output one"},
	{ID: "token_classification", Family: FamilyText, Label: "Token classification", CLISupported: true,
		Blurb: "label each word in a sequence"},
	{ID: "sentence_pair_classification", Family: FamilyText, Label: "Sentence-pair classification", CLISupported: true, IsClassification: true,
		Blurb: "label how two texts relate"},
	{ID: "embeddings", Family: FamilyText, Label: "Embeddings", CLISupported: true, SelfSupervised: true,
		Blurb: "learn vector representations from text pairs"},
	// semantic_segmentation stays CLI-pending: di#136 (mask sidecar) shipped,
	// but the ingestor doesn't yet populate the mask_id link column the
	// contract requires, and the training-side sign-off is tracked in
	// backend#816. Wire it once those land (RFC-0002 phase 4 follow-up).
	{ID: "semantic_segmentation", Family: FamilyImage, Label: "Semantic segmentation", CLISupported: false, IsClassification: true,
		Blurb:           "label every pixel in an image",
		UnsupportedNote: "schema-recognized; awaiting the ingestor's mask_id link column + training sign-off (backend#816)"},
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

// DisplayName is the name to show a user: the recognizable Gloss when a
// task has one, otherwise the Label. Kept a method so the picker never
// re-derives the gloss-vs-label rule itself.
func (c CategorySpec) DisplayName() string {
	if c.Gloss != "" {
		return c.Gloss
	}
	return c.Label
}

// CategoriesByFamily returns every registry spec in fam, in registry
// (display) order — CLI-supported first, then the not-yet-implemented
// ones. The data-first picker calls this once the family is known so it
// only ever offers that family's tasks, never the flat 15-item wall.
func CategoriesByFamily(fam Family) []CategorySpec {
	out := make([]CategorySpec, 0, len(categoryRegistry))
	for _, c := range categoryRegistry {
		if c.Family == fam {
			out = append(out, c)
		}
	}
	return out
}

// familyNounTable is the single source of truth pairing each Family with the
// plain word shown in prompts, echoes, and the interactive family picker. The
// slice order is the picker's display order — tabular first, since it's the
// most common family and the default when the layout sniff is ambiguous. That
// order is deliberately NOT the Family iota order (which is layout-internal).
// FamilyNoun (forward), FamilyFromNoun (reverse), and FamilyNouns (picker
// options + default) all derive from this one table, so they can't drift apart.
var familyNounTable = []struct {
	family Family
	noun   string
}{
	{FamilyTabular, "tabular"},
	{FamilyImage, "image"},
	{FamilyText, "text"},
}

// FamilyNoun is the plain word for a family, used in prompts and echoes
// ("tasks for tabular data", "this is image data"). Falls back to the picker
// default ("tabular") for an unrecognized family.
func FamilyNoun(fam Family) string {
	for _, e := range familyNounTable {
		if e.family == fam {
			return e.noun
		}
	}
	return "tabular"
}

// FamilyFromNoun maps a family noun ("image"/"text"/"tabular") back to its
// Family — the reverse of FamilyNoun. Unrecognized input falls back to
// FamilyTabular, matching the picker default so a stray answer degrades to the
// safe common case.
func FamilyFromNoun(noun string) Family {
	for _, e := range familyNounTable {
		if e.noun == noun {
			return e.family
		}
	}
	return FamilyTabular
}

// FamilyNouns returns the family nouns in picker/display order; the first
// element is the choice the picker pre-selects. The interactive family
// prompt derives both its options and its default from here.
func FamilyNouns() []string {
	nouns := make([]string, len(familyNounTable))
	for i, e := range familyNounTable {
		nouns[i] = e.noun
	}
	return nouns
}

// SelfSupervisedText reports whether a text category trains without an
// explicit label column, so the CLI skips the "which column is the label?"
// question. MLM/CLM derive the target from the text itself (mask a token;
// predict the next one); seq2seq and embeddings derive it from the record's
// own paired fields (source→target, anchor/positive/negative) — none reads a
// labels column. The answer is the registry's SelfSupervised flag, so a new
// self-supervised task is handled the moment it's added to the registry — not
// when someone remembers to edit this function.
func SelfSupervisedText(category string) bool {
	c, ok := categoryByID[category]
	return ok && c.SelfSupervised
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

// IsClassification reports whether the ingestor treats category as a
// classification task (registry ModalitySpec.is_classification) — i.e. its
// validator chain includes LabelDiversityValidator. The label-diversity
// preflight gates on this so the CLI mirrors the ingestor's wiring rather than
// hardcoding a category id (which is exactly how the text-family preflight
// drifted when it only knew text_classification).
func IsClassification(category string) bool { return categoryByID[category].IsClassification }

// SupportedCategoryIDs returns the ids `dataset push` supports, in display
// order. Used to build the --task help, the interactive picker, and
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

// TextSidecarDir returns the sidecar directory name a text category expects
// ("sequences" for masked_language_modeling, "texts" for every other text
// task). Used both as the local subdir to stage and the spec field to emit.
//
// The value is READ from the vendored layout contract's primary_subdir — the
// ingestor owns this fact (data-ingestors registry ModalitySpec.file_subdir),
// so the CLI mirrors it rather than keeping a Go fork of the same rule
// (RFC-0002 Principle 6). Falls back to "texts" for a category the contract
// doesn't pin — defensive only; the contract covers every text task and
// TestTextSidecarDirMirrorsContract enforces the two agree.
func TextSidecarDir(category string) string {
	if layout, ok := LayoutFor(category); ok && layout.PrimarySubdir != nil {
		return *layout.PrimarySubdir
	}
	return "texts"
}
