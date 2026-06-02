package push

import (
	"strings"
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
	if got := StagedPrefix("table_a"); got != "/data/shared/.tracebloc-staging/table_a" {
		t.Errorf("StagedPrefix(%q) = %q, want /data/shared/.tracebloc-staging/table_a", "table_a", got)
	}
}

// TestStagedPrefix_DoesNotCollideWithDest is the regression pin for
// the live-discovered blocker (#26): the CLI must stage SOURCE files
// somewhere the ingestor's DEST_PATH (= FinalDestPrefix = /data/
// shared/<table>) does NOT contain. If staging ever lands at or under
// the destination again, the ingestor's DuplicateValidator will
// reject the CLI's own staging as a pre-existing non-empty
// destination, and every push fails the duplicate check.
func TestStagedPrefix_DoesNotCollideWithDest(t *testing.T) {
	const table = "cats_dogs_train"
	staged := StagedPrefix(table)
	dest := FinalDestPrefix(table)

	if staged == dest {
		t.Fatalf("StagedPrefix == FinalDestPrefix == %q; the CLI would stage "+
			"into the ingestor's DEST_PATH and trip DuplicateValidator", staged)
	}
	// The destination must not contain the staging dir either —
	// otherwise DEST is non-empty (it holds the staging subtree) and
	// the validator still fails.
	if strings.HasPrefix(staged, dest+"/") {
		t.Errorf("StagedPrefix %q is under FinalDestPrefix %q; DEST would be "+
			"non-empty at ingest time", staged, dest)
	}
	if got := FinalDestPrefix(table); got != "/data/shared/"+table {
		t.Errorf("FinalDestPrefix(%q) = %q, want /data/shared/%s", table, got, table)
	}
}

// TestBuild_WithTargetSize_PassesSchema pins the #27 plumbing: when
// the CLI resolves an image resolution (via --target-size or
// auto-detect), Build emits spec.file_options.target_size and the
// result still validates against the embedded v1 schema. A drift here
// would make every push that sets a target size fail validation.
func TestBuild_WithTargetSize_PassesSchema(t *testing.T) {
	spec := SpecArgs{
		Table:       "cats_dogs_train",
		Category:    "image_classification",
		Intent:      "train",
		LabelColumn: "label",
		TargetSize:  []int{256, 256},
	}.Build()

	// The nested override must be present and well-shaped.
	specBlock, ok := spec["spec"].(map[string]any)
	if !ok {
		t.Fatalf("Build() with TargetSize didn't emit a spec block: %#v", spec["spec"])
	}
	fo, ok := specBlock["file_options"].(map[string]any)
	if !ok {
		t.Fatalf("spec.file_options missing/wrong type: %#v", specBlock["file_options"])
	}
	ts, ok := fo["target_size"].([]int)
	if !ok || len(ts) != 2 || ts[0] != 256 || ts[1] != 256 {
		t.Fatalf("spec.file_options.target_size = %#v, want [256 256]", fo["target_size"])
	}

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
		t.Fatalf("ValidateYAML parse error on our own output: %v\n%s", parseErr, specBytes)
	}
	if len(errs) != 0 {
		t.Fatalf("spec with target_size failed schema validation: %s\nspec:\n%s",
			schema.FormatErrors(errs), specBytes)
	}
}

// TestBuild_NoTargetSize_OmitsSpecBlock: when no resolution is set,
// Build must NOT emit a spec block (the ingestor's per-category
// default applies). Asserting the omission keeps the minimal-spec
// contract that the original schema test relies on.
func TestBuild_NoTargetSize_OmitsSpecBlock(t *testing.T) {
	spec := SpecArgs{
		Table:       "t",
		Category:    "image_classification",
		Intent:      "train",
		LabelColumn: "label",
	}.Build()
	if _, present := spec["spec"]; present {
		t.Errorf("Build() with no TargetSize emitted a spec block; want omitted")
	}
}

// TestBuild_Tabular_PassesSchema pins the tabular Build branch: it
// emits schema-valid specs for the three label shapes — a string
// label (tabular_classification), an object label+policy
// (tabular_regression), and an object label + time_column
// (time_to_event_prediction) — and never emits an images field.
func TestBuild_Tabular_PassesSchema(t *testing.T) {
	v, err := schema.NewV1Validator()
	if err != nil {
		t.Fatalf("NewV1Validator: %v", err)
	}
	check := func(name string, a SpecArgs, wantLabelObject bool) {
		t.Run(name, func(t *testing.T) {
			spec := a.Build()
			if _, hasImages := spec["images"]; hasImages {
				t.Errorf("tabular spec emitted an images field: %v", spec)
			}
			if _, hasSchema := spec["schema"]; !hasSchema {
				t.Errorf("tabular spec missing schema: %v", spec)
			}
			if wantLabelObject {
				if _, ok := spec["label"].(map[string]any); !ok {
					t.Errorf("label = %#v, want object form {column, policy}", spec["label"])
				}
			} else {
				if _, ok := spec["label"].(string); !ok {
					t.Errorf("label = %#v, want string form", spec["label"])
				}
			}
			b, err := yaml.Marshal(spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			_, errs, parseErr := v.ValidateYAML(b)
			if parseErr != nil {
				t.Fatalf("parse: %v\n%s", parseErr, b)
			}
			if len(errs) != 0 {
				t.Fatalf("schema validation failed: %s\n%s", schema.FormatErrors(errs), b)
			}
		})
	}

	check("tabular_classification", SpecArgs{
		Table: "t_clf", Category: "tabular_classification", Intent: "train",
		LabelColumn: "label",
		Schema:      map[string]string{"f0": "FLOAT", "f1": "FLOAT", "label": "INT"},
	}, false)

	check("tabular_regression", SpecArgs{
		Table: "t_reg", Category: "tabular_regression", Intent: "train",
		LabelColumn: "price",
		Schema:      map[string]string{"sqft": "FLOAT", "price": "FLOAT"},
	}, true)

	check("time_to_event_prediction", SpecArgs{
		Table: "t_tte", Category: "time_to_event_prediction", Intent: "train",
		LabelColumn: "DEATH_EVENT", TimeColumn: "time",
		Schema: map[string]string{"age": "INT", "time": "INT", "DEATH_EVENT": "INT"},
	}, true)
}

// TestBuild_Tabular_RegressionDefaultsPolicyBucket: regression-class
// categories default to policy=bucket so the raw numeric target never
// ships to the central backend unless the customer opts out.
func TestBuild_Tabular_RegressionDefaultsPolicyBucket(t *testing.T) {
	spec := SpecArgs{
		Table: "t", Category: "tabular_regression", Intent: "train",
		LabelColumn: "y", Schema: map[string]string{"x": "FLOAT", "y": "FLOAT"},
	}.Build()
	lbl, ok := spec["label"].(map[string]any)
	if !ok {
		t.Fatalf("label = %#v, want object", spec["label"])
	}
	if lbl["policy"] != "bucket" {
		t.Errorf("default policy = %v, want bucket", lbl["policy"])
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

// TestValidateTableName_RejectsTooLong: K8s label values are
// capped at 63 chars, and the stage Pod carries the raw table
// name as the tracebloc.io/table label. Without this rejection,
// a 100-char table name passes pre-flight then fails Pod create
// with an opaque label-validation error. Bugbot flagged on PR-b
// round 5.
func TestValidateTableName_RejectsTooLong(t *testing.T) {
	tooLong := strings.Repeat("a", MaxTableNameLength+1)
	if err := ValidateTableName(tooLong); err == nil {
		t.Fatalf("ValidateTableName(%d-char name) = nil, want length-cap error", len(tooLong))
	}
	// Right at the boundary: 63 chars must be accepted.
	atCap := strings.Repeat("a", MaxTableNameLength)
	if err := ValidateTableName(atCap); err != nil {
		t.Errorf("ValidateTableName(%d-char name) = %v, want nil (at exact cap)", len(atCap), err)
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
