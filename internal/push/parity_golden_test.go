package push

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// The validator-parity harness (backend#828 P3; value-level from backend#1009).
// Per case:
//
//  1. the Go preflight's verdict matches the manifest's cli_verdict —
//     pins the CLI side;
//  2. the COMMITTED goldens (generated from the real data-ingestors
//     validators by scripts/gen-validator-goldens.py) match the
//     manifest's ingestor_verdict — so when the ingestor's rules change,
//     regenerating the goldens fails this test until the manifest (and,
//     where needed, the Go preview) is consciously updated.
//  3. for cases flagged value_parity, the Go preview's VALUE-level read of
//     the label column (resolved header + row count + class set) equals the
//     REAL ingestor's — the only assertion that catches accept/accept with
//     divergent stored data (data-ingestors #340: a case-/whitespace-
//     mismatched label passes both verdicts, then reads null in-cluster).
//
// Deliberate divergences (the CLI previewing read-/transfer-time failures
// the ingestor's preflight can't see) are explicit in the manifest, never
// silent.

type parityCase struct {
	Name            string            `json:"name"`
	Category        string            `json:"category"`
	CSV             string            `json:"csv"`
	LabelColumn     string            `json:"label_column"`
	Extension       string            `json:"extension"`
	TargetSize      []int             `json:"target_size"`
	MinSize         []int             `json:"min_size"`
	Schema          map[string]string `json:"schema"`
	CLIVerdict      string            `json:"cli_verdict"`
	IngestorVerdict string            `json:"ingestor_verdict"`
	ValueParity     bool              `json:"value_parity"`
	Note            string            `json:"note"`
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
			Values  *struct {
				Resolved string   `json:"resolved_label"`
				RowCount int      `json:"row_count"`
				Classes  []string `json:"classes"`
			} `json:"values"`
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

			if !c.ValueParity {
				return
			}
			// Value-level parity (backend#1009): the Go preview must read the
			// SAME label header, row count, and class set the real ingestor
			// does. Catches accept/accept-with-divergent-label (#340).
			if golden.Values == nil {
				t.Fatalf("case %s is value_parity but goldens.json has no values — "+
					"regenerate with scripts/gen-validator-goldens.py against a data-ingestors "+
					"checkout that includes the #340 label-resolution fix", c.Name)
			}
			gv := goLabelValues(t, c)
			if gv.Resolved != golden.Values.Resolved {
				t.Errorf("resolved label: Go preview = %q, ingestor golden = %q "+
					"(the read paths resolve the label column differently — #340 class)",
					gv.Resolved, golden.Values.Resolved)
			}
			if gv.RowCount != golden.Values.RowCount {
				t.Errorf("row count: Go preview = %d, ingestor golden = %d", gv.RowCount, golden.Values.RowCount)
			}
			if !slices.Equal(gv.Classes, golden.Values.Classes) {
				t.Errorf("class set: Go preview = %v, ingestor golden = %v", gv.Classes, golden.Values.Classes)
			}
		})
	}
}

// goLabelValues runs the Go preview's value-level label read for a case,
// deriving the NA-drop / numeric-collapse flags from the label's schema type
// exactly as PreflightDataset does — so the value comparison uses the same
// read semantics the production preflight would.
func goLabelValues(t *testing.T, c parityCase) LabelReadValues {
	t.Helper()
	csvPath := filepath.Join("testdata", "parity", "cases", c.Name, c.CSV)
	schema := c.Schema
	if IsTabular(c.Category) && len(schema) == 0 {
		if res, err := InferSchema(csvPath); err == nil {
			schema = res.Schema
		}
	}
	dropNA, collapse := false, false
	if IsTabular(c.Category) {
		sqlType, inSchema := labelSchemaType(schema, c.LabelColumn)
		dropNA = inSchema
		collapse = !(inSchema && isStringSQLType(sqlType))
	}
	return ReadLabelValues(csvPath, c.LabelColumn, dropNA, collapse)
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
		Sidecars:  listSidecars(t, dir),
	}
	spec := SpecArgs{
		Category:    c.Category,
		LabelColumn: c.LabelColumn,
		Extension:   c.Extension,
		TargetSize:  c.TargetSize,
		MinSize:     c.MinSize,
	}
	if IsTabular(c.Category) {
		// Mirror runDataIngest: an explicit schema (the --schema flow) wins,
		// else it is inferred from the CSV — the golden generator derives the
		// schema the same way, so dtype-sensitive verdicts stay comparable.
		if len(c.Schema) > 0 {
			spec.Schema = c.Schema
		} else if res, err := InferSchema(layout.LabelsCSV); err == nil {
			spec.Schema = res.Schema
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

// listSidecars maps each sidecar directory in a case (any subdir that isn't
// images/ — annotations/, masks/, texts/, sequences/) to its files, mirroring
// how the production Discover populates LocalLayout.Sidecars. Without this the
// object_detection / semantic_segmentation pairing previews would see an empty
// sidecar set and reject every case (cli#288).
func listSidecars(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	sidecars := make(map[string][]string)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "images" {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range files {
			if !f.IsDir() {
				out = append(out, filepath.Join(dir, e.Name(), f.Name()))
			}
		}
		sort.Strings(out)
		sidecars[e.Name()] = out
	}
	if len(sidecars) == 0 {
		return nil
	}
	return sidecars
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
