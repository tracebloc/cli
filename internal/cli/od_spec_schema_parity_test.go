package cli

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/schema"
)

// TestBuiltSpecValidatesAgainstVendoredSchema closes the loop backend#1006
// opens: the CLI BUILDS the ingest spec and the vendored ingest.v1.json JUDGES
// it, and those are two separate copies of the same contract (the third being
// data-ingestors' own).
//
// object_detection is the case that made this worth pinning. The schema does
// not merely stop requiring `csv` / `label` for it — it REJECTS them — so the
// old builder, which emitted both unconditionally, produces a spec that fails
// validation outright rather than one the ingestor quietly ignores. A drift
// between the builder and a re-synced schema is therefore a hard failure at
// `data ingest` time on a customer's machine, and this catches it in CI.
//
// Driven through the real validator over the real embedded schema — no
// hand-authored expectation to fall out of date.
func TestBuiltSpecValidatesAgainstVendoredSchema(t *testing.T) {
	validator, err := schema.NewV1Validator()
	if err != nil {
		t.Fatalf("compiling the vendored v1 schema: %v", err)
	}

	cases := []struct {
		name string
		args push.SpecArgs
	}{
		{
			name: "object_detection",
			args: push.SpecArgs{
				Category: "object_detection", Table: "visdrone_train", Intent: "train",
				// Deliberately SET even though the category is manifest-free:
				// the builder must drop it, not merely pass through whatever
				// the caller happened to leave populated. An interactive run
				// resolves a label column before the category is known.
				LabelColumn: "image_label",
				Extension:   ".jpg",
			},
		},
		{
			name: "image_classification",
			args: push.SpecArgs{
				Category: "image_classification", Table: "cats_dogs", Intent: "train",
				LabelColumn: "label", Extension: ".jpg",
			},
		},
		{
			name: "keypoint_detection",
			args: push.SpecArgs{
				Category: "keypoint_detection", Table: "poses", Intent: "train",
				LabelColumn: "image_label", Extension: ".jpg",
				TargetSize: []int{256, 256}, NumberOfKeypoints: 9,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := yaml.Marshal(tc.args.Build())
			if err != nil {
				t.Fatalf("marshalling the built spec: %v", err)
			}
			_, errs, parseErr := validator.ValidateYAML(raw)
			if parseErr != nil {
				t.Fatalf("parsing the built spec: %v", parseErr)
			}
			if len(errs) > 0 {
				t.Errorf("the CLI built a spec its own vendored schema rejects:\n%s\nspec:\n%s",
					schema.FormatErrors(errs), raw)
			}
		})
	}
}
