package push

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/tracebloc/cli/internal/schema"
)

// The per-task local dataset-layout contract, mirrored from data-ingestors'
// tracebloc_ingestor/schema/layout.v1.json (data-ingestors#347/#353), vendored
// into internal/schema/ and drift-checked by scripts/sync-schema.sh.
//
// The ingestor is the source of truth for what a task's local dataset looks
// like on disk. The CLI reads this contract so its discovery + staging is a
// VERIFIED MIRROR of the ingestor's rules rather than a Go fork of them
// (RFC-0002 Principle 6). Two things drive real behaviour here:
//
//   - RecordFormat — the structure inside each .txt for the structured text
//     tasks. For the ENFORCED formats (sentence_pair_classification,
//     embeddings) the CLI rejects a malformed file before staging, exactly as
//     the ingestor's TabSeparatedRecordValidator would in-cluster.
//   - The manifest/family/subdir facts — pinned against the Go category
//     registry by layout_contract_test.go, so category.go can't silently drift
//     from the ingestor's truth.

// LayoutContract is the top-level shape of layout.v1.json.
type LayoutContract struct {
	Version string                `json:"version"`
	Tasks   map[string]TaskLayout `json:"tasks"`
}

// TaskLayout is one task's on-disk layout.
type TaskLayout struct {
	Family        string         `json:"family"` // image | text | tabular
	Manifest      ManifestLayout `json:"manifest"`
	PrimarySubdir *string        `json:"primary_subdir"` // images | texts | sequences | null
	Sidecars      []SidecarSpec  `json:"sidecars"`
	RecordFormat  *RecordFormat  `json:"record_format"` // structured-text tasks only
	Grouping      *GroupingSpec  `json:"grouping"`      // sequence-grouped tasks only (time_series_classification)
}

// GroupingSpec is the sequence-grouping trait a grouped task declares
// (backend#1054 Decision-4: grouping is a ModalitySpec TRAIT the contract
// carries, never a category if/else in consuming code). GroupColumn names the
// column whose value groups the timestep rows of one sequence; TimeColumn
// orders the rows WITHIN each group; CountUnit is the SAMPLE UNIT the platform
// counts in ("sequences" — labels payloads, data_per_class, metrics are all
// per-sequence, not per-row). Mirrors the ingestor registry's
// ModalitySpec.grouping (data-ingestors modalities/spec.py).
type GroupingSpec struct {
	GroupColumn string `json:"group_column"` // sequence_id (fixed, Decision-2)
	TimeColumn  string `json:"time_column"`  // timestamp (fixed, Decision-2)
	CountUnit   string `json:"count_unit"`   // "sequences"
}

// ManifestLayout describes the task's manifest CSV. Kind "none" means the task
// has NO manifest CSV at all — object_detection, whose records are enumerated
// from the annotations/*.xml sidecar and whose label is derived, not read from
// a column (backend#1006). A "none" task carries neither a filename nor a label
// column, and a consumer must skip every labels-CSV read for it.
type ManifestLayout struct {
	Kind                   string `json:"kind"` // labels_csv | data_csv | none
	RequiresFilenameColumn bool   `json:"requires_filename_column"`
	HasLabelColumn         bool   `json:"has_label_column"`
}

// manifestKindNone is the Kind a task with no manifest CSV declares (backend#1006,
// object_detection). Kept a named constant so the CSV-gating predicates below and
// the mirror test read the same token.
const manifestKindNone = "none"

// HasManifestCSV reports whether a category ships a manifest CSV (labels_csv or
// data_csv) the CLI must discover, read, and stage — false only for the
// manifest-less categories (Kind "none": object_detection). Driven by the
// vendored layout contract so a future no-manifest category is handled by
// re-vendoring, not a code edit. An unknown category (absent from the contract)
// returns true: the schema validator then produces the canonical
// unrecognized-category error, unchanged from before this predicate existed.
func HasManifestCSV(category string) bool {
	t, ok := LayoutFor(category)
	if !ok {
		return true
	}
	return t.Manifest.Kind != manifestKindNone
}

// ManifestHasLabelColumn reports whether a category's manifest carries a
// user-declared label/target column — the fact that gates emitting `label` in
// the spec and previewing the label column locally. Mirrors the ingestor's
// has_label_column: false for the self-supervised text tasks AND for
// object_detection (label derived from the XML, not a column — backend#1006).
// Unknown category returns false (nothing to point a label at).
func ManifestHasLabelColumn(category string) bool {
	t, ok := LayoutFor(category)
	return ok && t.Manifest.HasLabelColumn
}

// SidecarSpec is an extra per-row directory a file-bearing task needs beyond
// its primary subdir (object_detection's annotations/, semseg's masks/).
type SidecarSpec struct {
	Subdir     string  `json:"subdir"`
	Glob       string  `json:"glob"`
	Required   bool    `json:"required"`
	LinkColumn *string `json:"link_column"` // manifest column linking a row to its sidecar; null = paired by filename stem
}

// RecordFormat is the structure inside each .txt for the structured text
// tasks. Fields are the ordered field names separated by Separator; MinFields
// is the fewest that must be present (embeddings accepts an optional trailing
// negative, so Fields=(anchor,positive,negative) with MinFields=2). Enforced
// is true only when a structural validator rejects a malformed file in-cluster
// (sentence_pair, embeddings); false marks a documented convention the
// ingestor does NOT reject (seq2seq, causal LM accept raw free text), so a
// mirror must not reject it either.
type RecordFormat struct {
	Separator string   `json:"separator"`
	Fields    []string `json:"fields"`
	MinFields int      `json:"min_fields"`
	Enforced  bool     `json:"enforced"`
}

// layoutContract is the parsed embedded contract. Parsed once at package init;
// a parse failure means the vendored JSON is broken (a build/vendoring bug CI
// catches via sync-schema.sh --check), so we fail loudly rather than limp on.
var layoutContract = mustLoadLayoutContract()

// SupportedLayoutVersions is every layout.v1.json version this CLI knows how
// to read. A version outside it is refused at load rather than interpreted,
// because the alternative is worse than a crash.
//
// WHY THIS EXISTS (backend#3146, found by @LukasWodka reviewing
// data-ingestors#557). Until now `Version` was unmarshalled and never compared
// to anything, so the version bump that carries a shape change was not
// self-enforcing here at all. Measured on develop before this change:
//
//   - nothing compared `LayoutContract.Version` to a supported set
//   - `mustLoadLayoutContract` panicked only on a PARSE failure
//   - `scripts/sync-schema.sh --check` is a byte-drift check in CI, not a
//     runtime gate
//
// The consequence was silent and customer-facing: a CLI that had not
// re-vendored kept its embedded v3 bytes, kept `requires_filename_column: true`
// for object_detection, and kept producing the 400 that data-ingestors#555's v4
// bump exists to end -- with nothing anywhere saying so. `e2e-test-agent`'s
// harness has had this guard from the start; the CLI is the consumer that
// silently applied a stale shape instead.
//
// ADD A VERSION HERE ONLY AFTER READING THE CONTRACT DIFF. Widening this set
// is a claim that the new shape is one this code handles, which is exactly the
// question the bump is asking.
var SupportedLayoutVersions = map[string]bool{
	"4": true,
}

func mustLoadLayoutContract() *LayoutContract {
	var c LayoutContract
	if err := json.Unmarshal(schema.LayoutV1Bytes, &c); err != nil {
		panic(fmt.Sprintf("parsing embedded layout.v1.json: %v", err))
	}
	if !SupportedLayoutVersions[c.Version] {
		// PANIC, like the parse failure above, and for the same reason: the
		// contract is embedded at build time, so an unsupported version is a
		// build that must not ship rather than a runtime condition a user
		// could hit or recover from. Failing at init makes it a CI failure on
		// the re-vendor, which is where it belongs.
		panic(fmt.Sprintf(
			"embedded layout.v1.json is version %q; this CLI reads %v. "+
				"Refusing to guess: applying a contract shape we do not "+
				"understand is how object_detection kept getting a "+
				"labels.csv it no longer stages (backend#3146). Re-vendor "+
				"with scripts/sync-schema.sh, read the diff, then add the "+
				"version to SupportedLayoutVersions.",
			c.Version, sortedVersions()))
	}
	return &c
}

func sortedVersions() []string {
	out := make([]string, 0, len(SupportedLayoutVersions))
	for v := range SupportedLayoutVersions {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// LayoutFor returns the layout contract for a task category and whether it is
// present in the contract.
func LayoutFor(category string) (TaskLayout, bool) {
	t, ok := layoutContract.Tasks[category]
	return t, ok
}

// GroupingFor returns the sequence-grouping trait for a category and whether
// it declares one (today only time_series_classification). Ungrouped tasks
// return false. Consumers gate per-sequence behaviour on THIS trait, never on
// a category id (Decision-4) — a future grouped task is handled the moment
// the vendored contract declares it, with zero CLI edits.
func GroupingFor(category string) (GroupingSpec, bool) {
	t, ok := layoutContract.Tasks[category]
	if !ok || t.Grouping == nil {
		return GroupingSpec{}, false
	}
	return *t.Grouping, true
}

// RecordFormatFor returns the record format for a text category and whether it
// declares one. Tasks without a structured .txt shape (text_classification,
// token_classification, MLM) return false.
func RecordFormatFor(category string) (RecordFormat, bool) {
	t, ok := layoutContract.Tasks[category]
	if !ok || t.RecordFormat == nil {
		return RecordFormat{}, false
	}
	return *t.RecordFormat, true
}

// AllowedFieldCounts is the set of field counts a valid record may have —
// MinFields..len(Fields), inclusive. Mirrors the ingestor's
// TabSeparatedRecordValidator.ALLOWED_FIELD_COUNTS (sentence_pair: {2};
// embeddings: {2, 3}).
func (rf RecordFormat) AllowedFieldCounts() []int {
	var out []int
	for n := rf.MinFields; n <= len(rf.Fields); n++ {
		out = append(out, n)
	}
	return out
}

// sepLabel renders the separator for an error message — a literal tab becomes
// "<TAB>" so the message is readable in a terminal.
func (rf RecordFormat) sepLabel() string {
	if rf.Separator == "\t" {
		return "<TAB>"
	}
	return rf.Separator
}

// shape renders the canonical record shape, e.g. "text_a<TAB>text_b" or
// "anchor<TAB>positive<TAB>negative".
func (rf RecordFormat) shape() string {
	return strings.Join(rf.Fields, rf.sepLabel())
}

// countPhrase renders the allowed field-count clause: "exactly 2" for a single
// allowed count, "2 or 3" for a range.
func (rf RecordFormat) countPhrase() string {
	counts := rf.AllowedFieldCounts()
	if len(counts) == 1 {
		return fmt.Sprintf("exactly %d", counts[0])
	}
	parts := make([]string, len(counts))
	for i, n := range counts {
		parts[i] = fmt.Sprintf("%d", n)
	}
	return strings.Join(parts, " or ")
}

// ValidateTextRecord mirrors the ingestor's TabSeparatedRecordValidator
// per-file structural check for the ENFORCED record-format text tasks
// (sentence_pair_classification, embeddings): the file must be a single line
// of MinFields..len(Fields) non-empty separator-delimited fields.
//
// For unenforced formats (causal_language_modeling, seq2seq) it returns nil —
// the ingestor accepts raw free text for those, so a mirror must not reject it
// (RFC-0002 Principle 6). An empty / whitespace-only file also returns nil: the
// ingestor leaves that to its TextContentValidator (which warns), so rejecting
// it here would diverge.
func ValidateTextRecord(rf RecordFormat, content string) error {
	if !rf.Enforced {
		return nil
	}
	// Drop only surrounding blank lines / trailing newline — NOT interior
	// separators, so a leading/trailing empty field is still caught below.
	record := strings.Trim(content, "\r\n")
	if strings.TrimSpace(record) == "" {
		return nil
	}
	// One record per file: a surviving interior line break means several
	// records were crammed in (or a field holds a newline) — ambiguous.
	if strings.ContainsAny(record, "\r\n") {
		return fmt.Errorf(
			"expected a single %s record but the file spans multiple lines. "+
				"Put one %s per .txt", rf.shape(), rf.shape())
	}
	parts := strings.Split(record, rf.Separator)
	if !slices.Contains(rf.AllowedFieldCounts(), len(parts)) {
		// Separator comes from the contract (sepLabel renders a tab as "<TAB>"),
		// so a future non-tab task isn't misdescribed as "tab-separated".
		return fmt.Errorf(
			"expected %s %s-separated fields (%s), found %d. "+
				"Separate each field with exactly one %s",
			rf.countPhrase(), rf.sepLabel(), rf.shape(), len(parts), rf.sepLabel())
	}
	for i, p := range parts {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf(
				"field %d is empty — every field (%s) must be non-empty",
				i+1, strings.Join(rf.Fields[:len(parts)], ", "))
		}
	}
	return nil
}
