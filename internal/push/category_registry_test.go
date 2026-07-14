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
		"semantic_segmentation",
		"text_classification", "token_classification",
		"masked_language_modeling", "causal_language_modeling", "seq2seq",
		"sentence_pair_classification", "embeddings",
		"tabular_classification", "tabular_regression",
		"time_series_forecasting", "time_series_classification",
		"time_to_event_prediction",
	}
	if got := allCategoryIDs(); !equalSet(got, want) {
		t.Fatalf("allCategoryIDs() = %v, want set %v", got, want)
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
	// RFC-0002 phase 4 wired the last pending category — semantic_segmentation
	// (#182; its blockers landed — di#358 shipped the ingestor's mask_id
	// require-and-enforce in v0.7.0, and backend#816 closed). So ALL 16 schema
	// categories are pushable now, none gated out.
	if len(got) != 16 {
		t.Fatalf("SupportedCategoryIDs() len = %d, want 16: %v", len(got), got)
	}
	for _, id := range got {
		if !IsCLISupported(id) {
			t.Errorf("SupportedCategoryIDs returned %q but IsCLISupported is false", id)
		}
	}
	// Every known category is now CLI-supported AND carries no stale pending
	// note (the picker only greys out categories with an UnsupportedNote).
	for _, id := range allCategoryIDs() {
		if !IsCLISupported(id) {
			t.Errorf("%s should be CLI-supported (every category is wired now)", id)
		}
		if spec, _ := Lookup(id); spec.UnsupportedNote != "" {
			t.Errorf("%s is supported but still carries an UnsupportedNote: %q", id, spec.UnsupportedNote)
		}
	}
	// semantic_segmentation specifically — the phase-4 finale (#182) — must be
	// pushable now, closing out the known-but-pending set.
	if !IsCLISupported("semantic_segmentation") {
		t.Error("semantic_segmentation should be CLI-supported after #182")
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

// schemaCategoryEnum returns the category enum from the embedded ingest.v1
// schema — the single source of truth the registry is pinned against (#1005).
// The schema is vendored + drift-checked against data-ingestors by
// scripts/sync-schema.sh, so this ties the registry transitively to upstream.
func schemaCategoryEnum(t *testing.T) []string {
	t.Helper()
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
	return doc.Properties.Category.Enum
}

// registryAliases are registry category IDs deliberately NOT in the ingest.v1
// schema enum — declared placeholders. Empty today: instance_segmentation used
// to sit here unchecked, but it's dead (it half-ingested with no validators or
// file transfer — data-ingestors #240/#99) and was removed, not kept. A future
// known-but-unschema'd placeholder must be DECLARED here, so TestRegistryWithinSchema
// flags undeclared drift while allowing an intentional superset (#1005).
var registryAliases = map[string]bool{}

// TestRegistryCoversSchemaCategories pins schema ⊆ registry: every category the
// ingest schema accepts must be known to the registry, or a schema-valid
// `dataset push --category=X` is wrongly rejected as "unrecognized" (the
// token_classification drift, Bugbot v0.4.0 RC).
func TestRegistryCoversSchemaCategories(t *testing.T) {
	for _, id := range schemaCategoryEnum(t) {
		if !IsKnown(id) {
			t.Errorf("schema category %q missing from the registry — `dataset push --category=%s` "+
				"would be rejected as unrecognized despite passing schema validation", id, id)
		}
	}
}

// TestRegistryWithinSchema pins registry ⊆ schema (+ declared aliases): the
// registry must not carry a category the ingest schema — and therefore the
// ingestor — doesn't accept. An undeclared extra is exactly the
// instance_segmentation half-ingest class: the backend/CLI would accept a
// `--category` the pipeline can't handle, and the config half-ingests (DB rows
// + API records, zero files staged; #1005, data-ingestors #240/#99). Together
// with TestRegistryCoversSchemaCategories this pins registry == schema, modulo
// explicitly declared placeholders in registryAliases.
func TestRegistryWithinSchema(t *testing.T) {
	inSchema := make(map[string]bool)
	for _, id := range schemaCategoryEnum(t) {
		inSchema[id] = true
	}
	for _, id := range allCategoryIDs() {
		if !inSchema[id] && !registryAliases[id] {
			t.Errorf("registry category %q is not in the ingest.v1 schema enum and not a declared "+
				"alias — add it to the schema (data-ingestors) if it's real, or declare it in "+
				"registryAliases if it's an intentional placeholder", id)
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
