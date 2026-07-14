package push

import "testing"

// These tests pin the Go category registry as a VERIFIED MIRROR of the
// vendored layout contract (internal/schema/layout.v1.json), so category.go
// cannot silently drift from the ingestor's on-disk truth (RFC-0002
// Principle 6). The contract itself is drift-checked against data-ingestors by
// scripts/sync-schema.sh, so this ties the registry transitively to upstream.

// familyFromContract maps the contract's family string to the CLI Family enum.
func familyFromContract(t *testing.T, s string) Family {
	t.Helper()
	switch s {
	case "image":
		return FamilyImage
	case "text":
		return FamilyText
	case "tabular":
		return FamilyTabular
	default:
		t.Fatalf("unknown contract family %q", s)
		return 0
	}
}

// TestRegistryMirrorsLayoutContract: for every category the Go registry knows,
// the layout contract must agree on family and on the label-column fact, and
// vice versa (every contract task must be a known category). This is the
// single guard that keeps the hand-maintained registry honest against the
// machine-readable contract.
func TestRegistryMirrorsLayoutContract(t *testing.T) {
	// Registry ⊆ contract, with agreeing facts.
	for _, c := range categoryRegistry {
		layout, ok := LayoutFor(c.ID)
		if !ok {
			t.Errorf("category %q is in the registry but missing from layout.v1.json", c.ID)
			continue
		}
		if want := familyFromContract(t, layout.Family); c.Family != want {
			t.Errorf("%s: registry Family = %d, contract says %q (%d)", c.ID, c.Family, layout.Family, want)
		}
		// SelfSupervised (no label question) is the inverse of the contract's
		// has_label_column, for EVERY category — image/tabular carry a label
		// and are not self-supervised; the self-supervised text tasks carry
		// none. This is the fact spec.buildText + the interactive label prompt
		// both key off, so pinning it here catches a mis-set flag.
		if c.SelfSupervised == layout.Manifest.HasLabelColumn {
			t.Errorf("%s: registry SelfSupervised = %v but contract has_label_column = %v (must be opposite)",
				c.ID, c.SelfSupervised, layout.Manifest.HasLabelColumn)
		}
	}

	// Contract ⊆ registry: no task in the contract is unknown to the CLI.
	for id := range layoutContract.Tasks {
		if !IsKnown(id) {
			t.Errorf("layout.v1.json task %q is not a known CLI category", id)
		}
	}
}

// TestTextSidecarDirMirrorsContract: TextSidecarDir must return exactly the
// contract's primary_subdir for every text task — the directory the CLI stages
// into has to be the one the ingestor reads (texts/ for every text task but
// MLM, which uses sequences/).
// TestSemsegSidecarMirrorsContract pins the semseg sidecar facts the Go code
// hardcodes — "masks"/pngExtensions (image_extras.go), the mask_id link column
// (preflight.go CheckMaskIdColumn), and spec["schema"]={mask_id} (spec.go) — to
// the vendored layout contract, which the ingestor owns (RFC-0002 Principle 6).
// If the ingestor renames the link column or changes the mask subdir/glob, this
// fails rather than the CLI silently emitting and checking the stale name.
func TestSemsegSidecarMirrorsContract(t *testing.T) {
	layout, ok := LayoutFor("semantic_segmentation")
	if !ok {
		t.Fatal("semantic_segmentation missing from layout.v1.json")
	}
	if len(layout.Sidecars) != 1 {
		t.Fatalf("semseg sidecars = %d, want 1", len(layout.Sidecars))
	}
	sc := layout.Sidecars[0]
	if sc.Subdir != "masks" {
		t.Errorf(`sidecar subdir = %q, want "masks" (hardcoded in image_extras.go/spec.go)`, sc.Subdir)
	}
	if sc.Glob != "*.png" {
		t.Errorf(`sidecar glob = %q, want "*.png" (pngExtensions in image_extras.go)`, sc.Glob)
	}
	if sc.LinkColumn == nil || *sc.LinkColumn != "mask_id" {
		t.Errorf(`sidecar link_column = %v, want "mask_id" (maskIDColumn in preflight.go, schema key in spec.go)`, sc.LinkColumn)
	}
	if !sc.Required {
		t.Error("semseg masks sidecar should be required=true")
	}
}

func TestTextSidecarDirMirrorsContract(t *testing.T) {
	for _, c := range categoryRegistry {
		if c.Family != FamilyText {
			continue
		}
		layout, ok := LayoutFor(c.ID)
		if !ok || layout.PrimarySubdir == nil {
			t.Fatalf("%s: text task missing a primary_subdir in the contract", c.ID)
		}
		if got := TextSidecarDir(c.ID); got != *layout.PrimarySubdir {
			t.Errorf("%s: TextSidecarDir = %q, contract primary_subdir = %q", c.ID, got, *layout.PrimarySubdir)
		}
	}
}

// TestRecordFormatFor_Contract pins the record-format facts the CLI enforces
// against the contract: the two enforced structured tasks and the two
// unenforced conventions, plus the derived allowed field counts.
func TestRecordFormatFor_Contract(t *testing.T) {
	cases := []struct {
		category     string
		wantPresent  bool
		wantEnforced bool
		wantCounts   []int
	}{
		{"sentence_pair_classification", true, true, []int{2}},
		{"embeddings", true, true, []int{2, 3}},
		{"seq2seq", true, false, []int{1, 2}},
		{"causal_language_modeling", true, false, []int{1, 2}},
		{"token_classification", false, false, nil}, // no structured record
		{"text_classification", false, false, nil},
		{"image_classification", false, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.category, func(t *testing.T) {
			rf, ok := RecordFormatFor(tc.category)
			if ok != tc.wantPresent {
				t.Fatalf("RecordFormatFor(%s) present = %v, want %v", tc.category, ok, tc.wantPresent)
			}
			if !ok {
				return
			}
			if rf.Enforced != tc.wantEnforced {
				t.Errorf("%s: Enforced = %v, want %v", tc.category, rf.Enforced, tc.wantEnforced)
			}
			got := rf.AllowedFieldCounts()
			if len(got) != len(tc.wantCounts) {
				t.Fatalf("%s: AllowedFieldCounts = %v, want %v", tc.category, got, tc.wantCounts)
			}
			for i := range got {
				if got[i] != tc.wantCounts[i] {
					t.Errorf("%s: AllowedFieldCounts = %v, want %v", tc.category, got, tc.wantCounts)
				}
			}
		})
	}
}

// TestValidateTextRecord mirrors the ingestor's TabSeparatedRecordValidator
// cases: enforced tasks reject the wrong field count / empty fields / multiple
// lines, accept a well-formed record, and never reject on an unenforced format.
func TestValidateTextRecord(t *testing.T) {
	sp, _ := RecordFormatFor("sentence_pair_classification")
	emb, _ := RecordFormatFor("embeddings")
	s2s, _ := RecordFormatFor("seq2seq")

	// Well-formed records pass.
	if err := ValidateTextRecord(sp, "left side\tright side"); err != nil {
		t.Errorf("valid sentence pair rejected: %v", err)
	}
	if err := ValidateTextRecord(emb, "anchor\tpositive"); err != nil {
		t.Errorf("valid embeddings pair rejected: %v", err)
	}
	if err := ValidateTextRecord(emb, "anchor\tpositive\tnegative"); err != nil {
		t.Errorf("valid embeddings triplet rejected: %v", err)
	}
	// A trailing newline is stripped, not an error.
	if err := ValidateTextRecord(sp, "left\tright\n"); err != nil {
		t.Errorf("trailing newline should be tolerated: %v", err)
	}

	// Malformed records fail.
	if err := ValidateTextRecord(sp, "no tab here"); err == nil {
		t.Error("sentence pair with 1 field should fail")
	}
	if err := ValidateTextRecord(sp, "a\tb\tc"); err == nil {
		t.Error("sentence pair with 3 fields should fail")
	}
	if err := ValidateTextRecord(emb, "only one"); err == nil {
		t.Error("embeddings with 1 field should fail")
	}
	if err := ValidateTextRecord(sp, "left\t"); err == nil {
		t.Error("empty trailing field should fail")
	}
	if err := ValidateTextRecord(sp, "l1\tr1\nl2\tr2"); err == nil {
		t.Error("multi-line record should fail")
	}

	// Unenforced format never rejects, even malformed-looking content.
	if err := ValidateTextRecord(s2s, "just raw text no tab"); err != nil {
		t.Errorf("unenforced seq2seq should accept raw text: %v", err)
	}
	// An empty / whitespace-only file is the TextContentValidator's job, not
	// this structural check — it must pass here (no double reporting).
	if err := ValidateTextRecord(sp, "   \n"); err != nil {
		t.Errorf("empty file should be tolerated by the structural check: %v", err)
	}
}

// TestGroupingForMirrorsContract pins the sequence-grouping trait
// (backend#1054 Decision-4) against the vendored contract:
// time_series_classification — and ONLY it, today — declares grouping, with
// the platform's fixed column names (Decision-2) and the sequence count unit
// (Decision-3). Every other category must stay ungrouped, so the grouped
// preflight path can't accidentally fire for them.
func TestGroupingForMirrorsContract(t *testing.T) {
	g, ok := GroupingFor("time_series_classification")
	if !ok {
		t.Fatal("time_series_classification must declare a grouping trait in the vendored contract")
	}
	if g.GroupColumn != "sequence_id" || g.TimeColumn != "timestamp" || g.CountUnit != "sequences" {
		t.Errorf("grouping = %+v, want the fixed {sequence_id, timestamp, sequences} contract", g)
	}

	for _, c := range categoryRegistry {
		if c.ID == "time_series_classification" {
			continue
		}
		if _, grouped := GroupingFor(c.ID); grouped {
			t.Errorf("%s: unexpectedly declares a grouping trait — only the sequence-grouped "+
				"time-series task is grouped today; a new grouped task needs a conscious "+
				"preflight/staging review, not a silent contract edit", c.ID)
		}
	}

	// A grouped task is tabular (single data CSV) and a classification task
	// — the facts the grouped preflight path relies on.
	if !IsTabular("time_series_classification") || !IsClassification("time_series_classification") {
		t.Error("time_series_classification must be tabular-family and is_classification")
	}

	// Unknown category: no grouping, no panic.
	if _, grouped := GroupingFor("nope"); grouped {
		t.Error("unknown category must report no grouping")
	}
}
