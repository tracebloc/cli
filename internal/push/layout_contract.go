package push

import (
	"encoding/json"
	"fmt"
	"slices"
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
}

// ManifestLayout describes the task's manifest CSV.
type ManifestLayout struct {
	Kind                   string `json:"kind"` // labels_csv | data_csv
	RequiresFilenameColumn bool   `json:"requires_filename_column"`
	HasLabelColumn         bool   `json:"has_label_column"`
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

func mustLoadLayoutContract() *LayoutContract {
	var c LayoutContract
	if err := json.Unmarshal(schema.LayoutV1Bytes, &c); err != nil {
		panic(fmt.Sprintf("parsing embedded layout.v1.json: %v", err))
	}
	return &c
}

// LayoutFor returns the layout contract for a task category and whether it is
// present in the contract.
func LayoutFor(category string) (TaskLayout, bool) {
	t, ok := layoutContract.Tasks[category]
	return t, ok
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
