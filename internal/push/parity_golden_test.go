package push

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The validator-parity harness (backend#828 P3). Two assertions per case:
//
//  1. the Go preflight's verdict matches the manifest's cli_verdict —
//     pins the CLI side;
//  2. the COMMITTED goldens (generated from the real data-ingestors
//     validators by scripts/gen-validator-goldens.py) match the
//     manifest's ingestor_verdict — so when the ingestor's rules change,
//     regenerating the goldens fails this test until the manifest (and,
//     where needed, the Go preview) is consciously updated.
//
// Deliberate divergences (the CLI previewing read-/transfer-time failures
// the ingestor's preflight can't see) are explicit in the manifest, never
// silent.

type parityCase struct {
	Name            string `json:"name"`
	Category        string `json:"category"`
	CSV             string `json:"csv"`
	LabelColumn     string `json:"label_column"`
	Extension       string `json:"extension"`
	TargetSize      []int  `json:"target_size"`
	CLIVerdict      string `json:"cli_verdict"`
	IngestorVerdict string `json:"ingestor_verdict"`
	Note            string `json:"note"`
}

func TestValidatorParity(t *testing.T) {
	var manifest struct {
		Cases []parityCase `json:"cases"`
	}
	mustLoad(t, filepath.Join("testdata", "parity", "cases.json"), &manifest)

	var goldens struct {
		Verdicts map[string]struct {
			Verdict string   `json:"verdict"`
			Errors  []string `json:"errors"`
		} `json:"verdicts"`
	}
	mustLoad(t, filepath.Join("testdata", "parity", "goldens.json"), &goldens)

	for _, c := range manifest.Cases {
		t.Run(c.Name, func(t *testing.T) {
			golden, ok := goldens.Verdicts[c.Name]
			if !ok {
				t.Fatalf("no golden for %s — run scripts/gen-validator-goldens.py", c.Name)
			}
			if golden.Verdict != c.IngestorVerdict {
				t.Errorf("the REAL ingestor validators say %q but the manifest expects %q — "+
					"the ingestor's rules changed; update cases.json (and the Go preview if needed). "+
					"Golden errors: %v", golden.Verdict, c.IngestorVerdict, golden.Errors)
			}
			got := runGoPreflight(t, c)
			if got != c.CLIVerdict {
				t.Errorf("Go preflight = %q, manifest expects %q (note: %s)", got, c.CLIVerdict, c.Note)
			}
		})
	}
}

// runGoPreflight runs THE production dispatch (push.PreflightDataset) over
// the case — the same code path runDataIngest executes, so a check deleted
// or rewired in production fails parity here.
func runGoPreflight(t *testing.T, c parityCase) string {
	t.Helper()
	dir := filepath.Join("testdata", "parity", "cases", c.Name)
	layout := &LocalLayout{
		Root:      dir,
		LabelsCSV: filepath.Join(dir, c.CSV),
		Images:    listImages(t, dir),
	}
	spec := SpecArgs{
		Category:    c.Category,
		LabelColumn: c.LabelColumn,
		Extension:   c.Extension,
		TargetSize:  c.TargetSize,
	}
	if IsTabular(c.Category) {
		// Mirror runDataIngest: the schema is inferred from the CSV before
		// the preflight runs (the schema-columns preview needs it).
		if sch, _, _, err := InferSchema(layout.LabelsCSV); err == nil {
			spec.Schema = sch
		}
	}
	_, problem := PreflightDataset(spec, layout)
	if problem != nil {
		return "reject"
	}
	return "accept"
}

func listImages(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "images"))
	if err != nil {
		return nil // tabular/text cases have no images/
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, filepath.Join(dir, "images", e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func mustLoad(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
