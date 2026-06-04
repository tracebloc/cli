package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/ui"
)

// TestRenderDatasetList_Empty: the empty listing shows the count and
// points the user at `dataset push`.
func TestRenderDatasetList_Empty(t *testing.T) {
	var buf bytes.Buffer
	renderDatasetList(ui.New(&buf, ui.WithColor(false)), "ap-workspace", nil)
	out := buf.String()
	if !strings.Contains(out, "Datasets in ap-workspace (0)") {
		t.Errorf("missing header/count:\n%s", out)
	}
	if !strings.Contains(out, "dataset push") {
		t.Errorf("empty state should point at `dataset push`:\n%s", out)
	}
}

// TestRenderDatasetList_Items: a populated listing shows the count and
// every table name.
func TestRenderDatasetList_Items(t *testing.T) {
	var buf bytes.Buffer
	renderDatasetList(ui.New(&buf, ui.WithColor(false)), "tracebloc-templates", []string{"reg_train", "churn_test"})
	out := buf.String()
	for _, want := range []string{"Datasets in tracebloc-templates (2)", "reg_train", "churn_test"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// TestWriteDatasetListJSON: valid JSON with the expected fields, and a
// nil dataset slice marshals as [] (not null) so scripts get an array.
func TestWriteDatasetListJSON(t *testing.T) {
	var buf bytes.Buffer
	writeDatasetListJSON(&buf, "ns1", "tracebloc", []string{"a", "b"})

	var got datasetListJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if got.Namespace != "ns1" || got.Release != "tracebloc" || got.Count != 2 {
		t.Errorf("unexpected: %+v", got)
	}
	if len(got.Datasets) != 2 || got.Datasets[0] != "a" {
		t.Errorf("datasets wrong: %+v", got.Datasets)
	}

	buf.Reset()
	writeDatasetListJSON(&buf, "ns1", "tracebloc", nil)
	if !strings.Contains(buf.String(), `"datasets": []`) {
		t.Errorf("nil datasets should marshal as []:\n%s", buf.String())
	}
}
