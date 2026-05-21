package schema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Build the validator once; reuse across cases. Compilation cost
// shouldn't be in the critical path of every test.
func mustValidator(t *testing.T) *Validator {
	t.Helper()
	v, err := NewV1Validator()
	if err != nil {
		t.Fatalf("compile embedded schema: %v", err)
	}
	return v
}

func TestEmbeddedSchemaCompiles(t *testing.T) {
	// Compiling is the most basic invariant — if the bundled JSON
	// is malformed, every other test fails too, but this gives the
	// clearest first signal.
	_ = mustValidator(t)
}

func TestEmbeddedSchemaIsV1ByID(t *testing.T) {
	// Spot-check that the bundled bytes are actually the v1 schema
	// (not, say, a stale draft or a v2 that got pulled by accident).
	// We grep on the canonical $id, not on schema structure, so a
	// future field rename or example update doesn't break this test
	// for spurious reasons.
	if !strings.Contains(string(V1Bytes), V1SchemaID) {
		t.Fatalf("embedded schema doesn't mention its expected $id %q", V1SchemaID)
	}
}

// Each of the canonical examples in tracebloc/data-ingestors must
// validate cleanly — if any fail, either the example is broken or
// the schema's drifted from what the examples assume. Pin them by
// inlining (the alternative is loading from an out-of-tree path
// which makes the test brittle to data-ingestors layout changes).
//
// The fixtures intentionally mirror examples/yaml/<category>.yaml
// from data-ingestors. When that set grows, this slice grows too.
func TestValidate_HappyPath_AllCategories(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "image_classification",
			yaml: `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: chest_xrays_train
intent: train
category: image_classification
csv: /data/labels.csv
images: /data/images/
label: image_label
`,
		},
		{
			name: "object_detection",
			yaml: `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: visdrone_train
intent: train
category: object_detection
csv: /data/labels.csv
images: /data/images/
annotations: /data/annotations/
label: image_label
`,
		},
		{
			name: "semantic_segmentation",
			yaml: `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: tumors_train
intent: train
category: semantic_segmentation
csv: /data/labels.csv
images: /data/images/
masks: /data/masks/
label: image_label
`,
		},
		{
			name: "text_classification",
			yaml: `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: support_tickets_train
intent: train
category: text_classification
csv: /data/labels.csv
texts: /data/texts/
schema:
  text_id: VARCHAR(255)
  label: VARCHAR(64)
label: label
`,
		},
		{
			name: "tabular_classification",
			yaml: `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: churn_train
intent: train
category: tabular_classification
csv: /data/customers.csv
schema:
  age: INT
  tenure_months: INT
  churned: VARCHAR(8)
label: churned
`,
		},
		{
			name: "tabular_regression_requires_bucket_policy",
			yaml: `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: house_prices_train
intent: train
category: tabular_regression
csv: /data/houses.csv
schema:
  square_feet: FLOAT
  price: FLOAT
label:
  column: price
  policy: bucket
`,
		},
	}

	v := mustValidator(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errs, parseErr := v.ValidateYAML([]byte(c.yaml))
			if parseErr != nil {
				t.Fatalf("YAML parse failed: %v", parseErr)
			}
			if len(errs) > 0 {
				t.Errorf("expected clean validation, got %d issue(s):\n%s",
					len(errs), FormatErrors(errs))
			}
		})
	}
}

// Negative cases — each exercises a different schema rule. The
// assertion is on the path the error reports (not the exact message
// wording, which is jsonschema/v6's responsibility to define) so
// library version bumps don't break the test for cosmetic reasons.
func TestValidate_NegativeCases(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantPaths []string // substring match on Path field of any violation
	}{
		{
			name: "missing apiVersion + intent",
			yaml: `
kind: IngestConfig
category: image_classification
table: t
csv: /data/labels.csv
images: /data/images/
label: my_label
`,
			wantPaths: []string{"<root>"}, // both go to root level
		},
		{
			name: "category not in enum",
			yaml: `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: t
intent: train
category: not_a_category
csv: /data/labels.csv
images: /data/images/
label: my_label
`,
			wantPaths: []string{"category"},
		},
		{
			name: "image category missing images key",
			yaml: `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: t
intent: train
category: image_classification
csv: /data/labels.csv
label: my_label
`,
			wantPaths: []string{"<root>"}, // missing required at root
		},
		{
			name: "regression without explicit label.policy",
			yaml: `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: t
intent: train
category: tabular_regression
csv: /data/houses.csv
schema:
  price: FLOAT
label: price
`,
			wantPaths: []string{"label"}, // schema enforces object form for regression
		},
		{
			name: "tabular missing schema block",
			yaml: `
apiVersion: tracebloc.io/v1
kind: IngestConfig
table: t
intent: train
category: tabular_classification
csv: /data/customers.csv
label: churned
`,
			wantPaths: []string{"<root>"},
		},
	}

	v := mustValidator(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errs, parseErr := v.ValidateYAML([]byte(c.yaml))
			if parseErr != nil {
				t.Fatalf("YAML parse failed: %v", parseErr)
			}
			if len(errs) == 0 {
				t.Fatalf("expected at least one schema violation, got none")
			}

			gotPaths := make([]string, 0, len(errs))
			for _, e := range errs {
				gotPaths = append(gotPaths, e.Path)
			}

			for _, want := range c.wantPaths {
				found := false
				for _, got := range gotPaths {
					if strings.Contains(got, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected a violation with path containing %q, got paths: %v\nfull errors:\n%s",
						want, gotPaths, FormatErrors(errs))
				}
			}
		})
	}
}

// Parse-level failures (vs schema violations) need a separate
// failure mode so callers can render them differently in the UI —
// "your file isn't YAML" vs "your file is YAML but doesn't match
// the schema" are different problems with different remediations.
func TestValidate_ParseFailures(t *testing.T) {
	v := mustValidator(t)

	t.Run("empty input", func(t *testing.T) {
		_, _, parseErr := v.ValidateYAML([]byte(""))
		if parseErr == nil {
			t.Fatal("expected parseErr on empty input, got nil")
		}
		if !strings.Contains(parseErr.Error(), "empty") {
			t.Errorf("expected message to mention empty, got: %v", parseErr)
		}
	})

	t.Run("whitespace only", func(t *testing.T) {
		_, _, parseErr := v.ValidateYAML([]byte("   \n\t  \n"))
		if parseErr == nil {
			t.Fatal("expected parseErr on whitespace-only input")
		}
	})

	t.Run("non-mapping top level (sequence)", func(t *testing.T) {
		_, _, parseErr := v.ValidateYAML([]byte("- one\n- two\n"))
		if parseErr == nil {
			t.Fatal("expected parseErr on top-level sequence")
		}
		if !strings.Contains(parseErr.Error(), "mapping") {
			t.Errorf("expected message to mention 'mapping', got: %v", parseErr)
		}
	})

	t.Run("malformed YAML", func(t *testing.T) {
		// Trailing colon + missing value, mixed indent — yaml.v3
		// flags this.
		_, _, parseErr := v.ValidateYAML([]byte("foo:\n  bar:\n\tbaz: 1\n"))
		if parseErr == nil {
			t.Fatal("expected parseErr on malformed YAML")
		}
	})
}

// FormatErrors is the contract with the UI layer; pin its exact
// output shape so any change is intentional. The leading two
// spaces mirror tracebloc_ingestor.cli.run._format_errors so
// customers see one wording across both implementations.
func TestFormatErrors_ContractShape(t *testing.T) {
	errs := []ValidationError{
		{Path: "category", Message: "value must be one of …"},
		{Path: "<root>", Message: "missing properties 'foo'"},
	}
	got := FormatErrors(errs)

	// Sorted output: <root> < category alphabetically.
	want := "  <root>: missing properties 'foo'\n  category: value must be one of …"
	if got != want {
		t.Errorf("FormatErrors mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// Round-trip test against the canonical example YAMLs in the
// data-ingestors repo. If that checkout is missing, skip — we
// don't want CI to fail for a local layout mismatch. CI runs the
// test in the runner's filesystem where data-ingestors isn't
// checked out alongside us, so this only fires for local devs.
func TestValidate_AgainstRealExamplesIfPresent(t *testing.T) {
	examplesDir := "/Volumes/VPPD/projects/tracebloc/data-ingestors/examples/yaml"
	entries, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Skipf("examples dir not present, skipping (this test is for local dev): %v", err)
	}

	v := mustValidator(t)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(examplesDir, e.Name()))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			_, errs, parseErr := v.ValidateYAML(body)
			if parseErr != nil {
				t.Fatalf("parse: %v", parseErr)
			}
			if len(errs) > 0 {
				// custom_processor.yaml may legitimately not match
				// (the example shows a feature deferred to v1.1);
				// skip rather than fail so the test stays useful as
				// the canonical examples evolve.
				if strings.Contains(e.Name(), "custom_processor") {
					t.Skipf("custom_processor example exercises deferred features; %d issue(s)",
						len(errs))
				}
				t.Errorf("expected clean, got %d issue(s):\n%s",
					len(errs), FormatErrors(errs))
			}
		})
	}
}
