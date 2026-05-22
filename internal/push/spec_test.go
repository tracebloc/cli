package push

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/tracebloc/cli/internal/schema"
)

// TestBuild_ImageClassificationMinimum_PassesSchema is the contract
// test that pins Phase 3's flag → spec synthesis to the embedded
// schema. The whole point of Build() is "produce something the
// canonical validator accepts" — if a refactor breaks that, every
// `dataset push` invocation fails after kubeconfig load but before
// the user sees a useful error, so we want this caught in CI.
func TestBuild_ImageClassificationMinimum_PassesSchema(t *testing.T) {
	args := SpecArgs{
		Table:       "chest_xrays_train",
		Category:    "image_classification",
		Intent:      "train",
		LabelColumn: "image_label",
	}
	spec := args.Build()

	// Round-trip via YAML because the validator's public API is
	// YAML-input. The map → JSON → YAML → parse-back chain is a
	// microscopic cost per `dataset push` invocation; not worth
	// adding a Validate(parsed) method to internal/schema for v0.1.
	specBytes, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	v, err := schema.NewV1Validator()
	if err != nil {
		t.Fatalf("NewV1Validator: %v", err)
	}
	_, errs, parseErr := v.ValidateYAML(specBytes)
	if parseErr != nil {
		t.Fatalf("ValidateYAML returned parse error on our own output: %v\n%s", parseErr, specBytes)
	}
	if len(errs) != 0 {
		t.Fatalf("synthesized spec failed schema validation: %s\nspec:\n%s",
			schema.FormatErrors(errs), specBytes)
	}
}

// TestBuild_PathsMatchStagedPrefix pins the contract between Phase 3
// (where the CLI puts files on the PVC) and Phase 4 (what paths the
// submitted spec tells jobs-manager to look at). If these ever
// drift, the ingestor Job spawned by jobs-manager won't find the
// files we just staged — a silent "0 rows ingested" outcome that's
// hard to debug.
func TestBuild_PathsMatchStagedPrefix(t *testing.T) {
	const table = "cats_dogs"
	spec := SpecArgs{
		Table:       table,
		Category:    "image_classification",
		Intent:      "train",
		LabelColumn: "label",
	}.Build()

	prefix := StagedPrefix(table)
	wantCSV := prefix + "/labels.csv"
	wantImages := prefix + "/images/"

	if got := spec["csv"].(string); got != wantCSV {
		t.Errorf("spec.csv = %q, want %q", got, wantCSV)
	}
	if got := spec["images"].(string); got != wantImages {
		t.Errorf("spec.images = %q, want %q", got, wantImages)
	}
}

// TestBuild_LeavesValidationToSchema asserts that Build() does NOT
// pre-validate. A garbage category goes through unchanged so the
// schema's enum check produces the canonical error message. The
// alternative — duplicating the schema's enum in Go — would drift
// the moment data-ingestors adds a new category and we forget to
// mirror it here.
func TestBuild_LeavesValidationToSchema(t *testing.T) {
	spec := SpecArgs{
		Table:       "x",
		Category:    "definitely-not-a-real-category",
		Intent:      "train",
		LabelColumn: "label",
	}.Build()

	if got := spec["category"].(string); got != "definitely-not-a-real-category" {
		t.Errorf("Build() pre-validated category; want raw passthrough, got %q", got)
	}
}

// TestStagedPrefix_PerTableIsolation pins the contract that two
// concurrent pushes for different tables don't write to the same
// PVC subdirectory. If this ever returns the same path for
// different tables, parallel `dataset push` calls would race on
// labels.csv overwrites.
func TestStagedPrefix_PerTableIsolation(t *testing.T) {
	if a, b := StagedPrefix("cats"), StagedPrefix("dogs"); a == b {
		t.Errorf("StagedPrefix(%q) == StagedPrefix(%q) = %q, want distinct", "cats", "dogs", a)
	}
	if got := StagedPrefix("table_a"); got != "/data/shared/table_a" {
		t.Errorf("StagedPrefix(%q) = %q, want /data/shared/table_a", "table_a", got)
	}
}

// TestValidateTableName_Accepts pins the names that MUST pass —
// the real-world example tables plus a few edge shapes (single
// char, leading underscore, mixed case, digits). A regression
// that rejects a valid name would break legitimate pushes.
func TestValidateTableName_Accepts(t *testing.T) {
	for _, name := range []string{
		"cats_dogs",
		"chest_xrays_train",
		"t1",
		"ABC",
		"table_123",
		"_leading_underscore",
		"9starts_with_digit", // valid MySQL identifier + safe path segment
	} {
		if err := ValidateTableName(name); err != nil {
			t.Errorf("ValidateTableName(%q) = %v, want nil", name, err)
		}
	}
}

// TestValidateTableName_RejectsUnsafe is the security-regression
// pin. The path-traversal cases (../../etc, ../foo) are the ones
// Bugbot flagged on PR #8 — if this test ever goes green with
// those removed, the traversal hole is back open.
func TestValidateTableName_RejectsUnsafe(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"parent traversal": "../../etc",
		"single parent":    "../foo",
		"embedded slash":   "foo/bar",
		"embedded dot":     "foo.bar",
		"bare dot":         ".",
		"absolute":         "/etc/passwd",
		"space":            "my table",
		"dash":             "cats-dogs", // not a valid unquoted MySQL identifier
		"trailing newline": "foo\n",
		"shell metachar":   "foo;rm",
		"null-ish unicode": "foo\x00bar",
	}
	for desc, table := range cases {
		if err := ValidateTableName(table); err == nil {
			t.Errorf("%s: ValidateTableName(%q) = nil, want a rejection error", desc, table)
		}
	}
}

// TestStagedPrefix_PanicsOnUnsafeName pins the defense-in-depth
// backstop: if a caller skips ValidateTableName and hands a
// traversal name straight to StagedPrefix, it must panic rather
// than silently return an escape path. PR-b adds new call sites
// for StagedPrefix — this test guards against one of them
// forgetting the validation step.
func TestStagedPrefix_PanicsOnUnsafeName(t *testing.T) {
	for _, unsafe := range []string{"../../etc", "foo/bar", ""} {
		t.Run(unsafe, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("StagedPrefix(%q) did not panic; an unsafe "+
						"name must panic, not return an escape path", unsafe)
				}
			}()
			_ = StagedPrefix(unsafe)
		})
	}
}
