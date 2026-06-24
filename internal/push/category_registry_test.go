package push

import (
	"sort"
	"testing"
)

// The registry is the single source of truth; these pin its contents and
// that the family predicates + the supported set all derive from it, so a
// future edit can't reintroduce the "5 of 9" drift (cli#74).

func TestRegistryKnownCategories(t *testing.T) {
	want := []string{
		"image_classification", "object_detection", "keypoint_detection",
		"semantic_segmentation", "instance_segmentation",
		"text_classification", "masked_language_modeling", "causal_language_modeling",
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
	// segmentation + causal_language_modeling are known but not yet pushable,
	// and must explain why.
	for _, id := range []string{"semantic_segmentation", "instance_segmentation", "causal_language_modeling"} {
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
