package push

import (
	"encoding/json"
	"os"
	"testing"
)

// TestSchemaInferenceParity pins the Go CLI's tabular type inference against
// data-ingestors' committed value->type contract
// (testdata/schema_inference_parity.json, vendored from di#349's
// tests/fixtures/schema_inference_parity.json). The ingestor's
// schema_inference.infer_column_type is the source of truth (Principle 6 /
// backend#1009); this test fails if the Go mirror drifts from it — a
// static, venv-free parity check the CI can run on every PR.
func TestSchemaInferenceParity(t *testing.T) {
	data, err := os.ReadFile("testdata/schema_inference_parity.json")
	if err != nil {
		t.Fatalf("read parity fixture: %v", err)
	}
	var fixture struct {
		Cases []struct {
			Name     string   `json:"name"`
			Values   []string `json:"values"`
			Expected string   `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("parse parity fixture: %v", err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("parity fixture has no cases — the di#349 contract is empty")
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			// inferColumnType cleans (trim + drop empty/NA) then classifies,
			// mirroring the ingestor's per-column path exactly.
			if got := inferColumnType(c.Values); got != c.Expected {
				t.Errorf("inferColumnType(%v) = %q, want %q (di#349 contract)", c.Values, got, c.Expected)
			}
		})
	}
}
