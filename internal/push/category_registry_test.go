package push

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/tracebloc/cli/internal/schema"
)

// The registry is the single source of truth; these pin its contents and
// that the family predicates + the supported set all derive from it, so a
// future edit can't reintroduce the "5 of 9" drift (cli#74).

func TestRegistryKnownCategories(t *testing.T) {
	want := []string{
		"image_classification", "object_detection", "keypoint_detection",
		"semantic_segmentation", "instance_segmentation",
		"text_classification", "token_classification",
		"masked_language_modeling", "causal_language_modeling", "seq2seq",
		"sentence_pair_classification", "embeddings",
		"tabular_classification", "tabular_regression",
		"time_series_forecasting", "time_to_event_prediction",
	}
	if got := AllCategoryIDs(); !equalSet(got, want) {
		t.Fatalf("AllCategoryIDs() = %v, want set %v", got, want)
	}
	for _, id := range want {
		if !IsKnown(id) {
			t.Errorf("IsKnown(%q) = false, want true", id)
		}
	}
	if IsKnown("not_a_category") {
		t.Error(`IsKnown("not_a_category") = true, want false`)
	}
}

func TestSupportedCategories(t *testing.T) {
	got := SupportedCategoryIDs()
	if len(got) != 9 {
		t.Fatalf("SupportedCategoryIDs() len = %d, want 9: %v", len(got), got)
	}
	for _, id := range got {
		if !IsCLISupported(id) {
			t.Errorf("SupportedCategoryIDs returned %q but IsCLISupported is false", id)
		}
	}
	// segmentation + the self-supervised text categories (CLM, seq2seq) +
	// token_classification are known but not yet pushable, and must explain why.
	for _, id := range []string{"semantic_segmentation", "instance_segmentation", "causal_language_modeling", "seq2seq", "token_classification", "sentence_pair_classification", "embeddings"} {
		if !IsKnown(id) {
			t.Errorf("%s should be known", id)
		}
		if IsCLISupported(id) {
			t.Errorf("%s should not be CLI-supported yet", id)
		}
		if spec, _ := Lookup(id); spec.UnsupportedNote == "" {
			t.Errorf("%s should carry an UnsupportedNote", id)
		}
	}
}

func TestPredicatesDeriveFromRegistry(t *testing.T) {
	for _, c := range categoryRegistry {
		switch c.Family {
		case FamilyImage:
			if !IsImage(c.ID) || IsTabular(c.ID) || IsText(c.ID) {
				t.Errorf("%s: predicates disagree with FamilyImage", c.ID)
			}
		case FamilyTabular:
			if !IsTabular(c.ID) || IsImage(c.ID) || IsText(c.ID) {
				t.Errorf("%s: predicates disagree with FamilyTabular", c.ID)
			}
		case FamilyText:
			if !IsText(c.ID) || IsImage(c.ID) || IsTabular(c.ID) {
				t.Errorf("%s: predicates disagree with FamilyText", c.ID)
			}
		}
		if IsRegressionClass(c.ID) != c.RegressionClass {
			t.Errorf("%s: IsRegressionClass = %v, want %v", c.ID, IsRegressionClass(c.ID), c.RegressionClass)
		}
	}
	// An unknown category: every predicate false (no panic on missing key).
	if IsImage("nope") || IsTabular("nope") || IsText("nope") ||
		IsRegressionClass("nope") || IsCLISupported("nope") {
		t.Error("predicates should all be false for an unknown category")
	}
}

// TestRegistryCoversSchemaCategories pins registry⇄schema parity: every
// category the ingest schema accepts must be known to the registry, or a
// schema-valid `dataset push --category=X` is wrongly rejected as
// "unrecognized" (the token_classification drift, Bugbot v0.4.0 RC). The
// existing tests only pin the registry against a hand-written list, which
// stays internally consistent while drifting from the schema — this closes
// that gap. The reverse direction isn't required: the registry may carry a
// known-but-unsupported alias the v1 schema doesn't list yet (e.g.
// instance_segmentation), which is gated out before schema validation.
func TestRegistryCoversSchemaCategories(t *testing.T) {
	var doc struct {
		Properties struct {
			Category struct {
				Enum []string `json:"enum"`
			} `json:"category"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema.V1Bytes, &doc); err != nil {
		t.Fatalf("parse embedded schema: %v", err)
	}
	if len(doc.Properties.Category.Enum) == 0 {
		t.Fatal("no category enum found in the embedded schema (parse path wrong?)")
	}
	for _, id := range doc.Properties.Category.Enum {
		if !IsKnown(id) {
			t.Errorf("schema category %q missing from the registry — `dataset push --category=%s` "+
				"would be rejected as unrecognized despite passing schema validation", id, id)
		}
	}
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
