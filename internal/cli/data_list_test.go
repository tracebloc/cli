package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// sample datasets spanning every modality + a system table.
func sampleInfos() []push.DatasetInfo {
	return []push.DatasetInfo{
		{Name: "image_train", Intent: "train", Records: 20, Classes: 2, Extension: "jpg", SizeBytes: 13210,
			Columns: []string{"id", "label", "data_intent", "data_id", "filename", "extension"}},
		{Name: "text_test", Intent: "test", Records: 10, Classes: 2, Extension: "txt", SizeBytes: 770,
			Columns: []string{"id", "label", "data_intent", "data_id", "filename", "extension"}},
		{Name: "tabular_train", Intent: "train", Records: 20, Classes: 2, Extension: "", SizeBytes: 206,
			Columns: []string{"id", "label", "data_intent", "data_id", "age", "income"}},
		{Name: "timeseries_train", Intent: "train", Records: 36, Classes: 2, Extension: "", SizeBytes: 695,
			Columns: []string{"id", "label", "data_intent", "data_id", "sequence_id", "timestamp", "hr", "temp"}},
		{Name: "tracebloc_ingest_runs", SizeBytes: 4096, System: true,
			Columns: []string{"ingestor_id", "table_name", "registered"}},
	}
}

// TestRunDataList_OutputJSONEarlyFailureEmitsJSON: with --output-json, a failure
// before the listing (broken kubeconfig, exit 3) still writes a JSON error
// object to stdout — the stdout-always-JSON contract from #49. (Bugbot #53)
func TestRunDataList_OutputJSONEarlyFailureEmitsJSON(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(bad, []byte("}{ not valid kubeconfig"), 0o644); err != nil {
		t.Fatal(err)
	}
	var jsonBuf, human bytes.Buffer
	err := runDataList(context.Background(), runDataListArgs{
		Kubeconfig: bad,
		OutputJSON: true,
		Printer:    ui.New(&human, ui.WithColor(false)),
		JSONOut:    &jsonBuf,
	})

	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 3 {
		t.Fatalf("err = %v, want *exitError code 3", err)
	}
	var got map[string]any
	if e := json.Unmarshal(jsonBuf.Bytes(), &got); e != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", e, jsonBuf.String())
	}
	if got["status"] != "error" || got["exit_code"] != float64(3) {
		t.Errorf("got %+v, want status=error exit_code=3", got)
	}
}

func TestRenderDataList_Empty(t *testing.T) {
	var buf bytes.Buffer
	renderDataList(ui.New(&buf, ui.WithColor(false)), "ap-workspace", nil, false)
	out := buf.String()
	if !strings.Contains(out, "Datasets in ap-workspace (0)") {
		t.Errorf("missing header/count:\n%s", out)
	}
	if !strings.Contains(out, "data ingest") {
		t.Errorf("empty state should point at `data ingest`:\n%s", out)
	}
}

// TestRenderDataList_RichAndGrouped: datasets group by modality with per-row
// detail; the system table is hidden by default (with a hint).
func TestRenderDataList_RichAndGrouped(t *testing.T) {
	var buf bytes.Buffer
	renderDataList(ui.New(&buf, ui.WithColor(false)), "test0721", sampleInfos(), false)
	out := buf.String()

	for _, want := range []string{
		"Datasets in test0721 — 4 · ",                             // 4 shown (system excluded), total size
		"Image · 1", "Text · 1", "Tabular · 1", "Time-series · 1", // modality groups
		"image_train", "20 images", "12.9 KiB", "jpg · 2 classes", "train", // rich image row
		"tabular_train", "20 rows", "csv · 2 cols", // tabular row + feature-col count
		"timeseries_train", "36 rows", // time-series grouped by sequence_id/timestamp
		"1 system table(s) hidden", // hint
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tracebloc_ingest_runs") {
		t.Errorf("system table must be hidden by default:\n%s", out)
	}
}

// --all reveals the system table under its own group.
func TestRenderDataList_ShowAll(t *testing.T) {
	var buf bytes.Buffer
	renderDataList(ui.New(&buf, ui.WithColor(false)), "test0721", sampleInfos(), true)
	out := buf.String()
	if !strings.Contains(out, "System · 1") || !strings.Contains(out, "tracebloc_ingest_runs") {
		t.Errorf("--all should show the system table:\n%s", out)
	}
}

// An ingested-but-empty dataset is flagged with ⚠, not ✔.
func TestDatasetRow_EmptyIsWarned(t *testing.T) {
	empty := push.DatasetInfo{Name: "objdet_train", Extension: "jpg", Records: 0, Intent: "train",
		Columns: []string{"data_id", "filename", "extension"}}
	row := datasetRow(empty, datasetModality(empty), 16, 16)
	if !strings.HasPrefix(row, "⚠") {
		t.Errorf("0-record dataset should lead with ⚠, got: %q", row)
	}
}

func TestDatasetModality(t *testing.T) {
	cases := []struct {
		d    push.DatasetInfo
		want string
	}{
		{push.DatasetInfo{Extension: "jpg"}, "Image"},
		{push.DatasetInfo{Extension: "PNG"}, "Image"},
		{push.DatasetInfo{Extension: "txt"}, "Text"},
		{push.DatasetInfo{Columns: []string{"sequence_id", "timestamp", "hr"}}, "Time-series"},
		{push.DatasetInfo{Columns: []string{"timestamp", "value"}}, "Time-series"},
		{push.DatasetInfo{Columns: []string{"time", "event"}}, "Time-series"},
		{push.DatasetInfo{Columns: []string{"age", "income"}, Records: 3}, "Tabular"},
	}
	for _, c := range cases {
		if got := datasetModality(c.d); got != c.want {
			t.Errorf("modality(%+v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 2048: "2.0 KiB", 13210: "12.9 KiB", 5 * 1024 * 1024: "5.0 MiB"}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestWriteDataListJSON: rich per-dataset objects; system excluded unless
// --all; an empty result marshals datasets as [] (not null).
func TestWriteDataListJSON(t *testing.T) {
	var buf bytes.Buffer
	writeDataListJSON(&buf, "ns1", "tracebloc", sampleInfos(), false)

	var got dataListJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if got.Namespace != "ns1" || got.Release != "tracebloc" || got.Count != 4 {
		t.Errorf("unexpected top-level: %+v", got)
	}
	var img *datasetJSON
	for i := range got.Datasets {
		if got.Datasets[i].Name == "image_train" {
			img = &got.Datasets[i]
		}
		if got.Datasets[i].System {
			t.Errorf("system table must be excluded without --all: %+v", got.Datasets[i])
		}
	}
	if img == nil || img.Modality != "Image" || img.Records != 20 || img.Format != "jpg · 2 classes" {
		t.Errorf("image dataset JSON wrong: %+v", img)
	}

	buf.Reset()
	writeDataListJSON(&buf, "ns1", "tracebloc", nil, false)
	if !strings.Contains(buf.String(), `"datasets": []`) {
		t.Errorf("nil datasets should marshal as []:\n%s", buf.String())
	}

	buf.Reset()
	writeDataListJSON(&buf, "ns1", "tracebloc", sampleInfos(), true)
	_ = json.Unmarshal(buf.Bytes(), &got)
	if got.Count != 5 {
		t.Errorf("--all JSON should include the system table (count 5), got %d", got.Count)
	}
}
