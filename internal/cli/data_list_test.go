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
	"time"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// sample datasets spanning every modality + a system table.
func sampleInfos() []push.DatasetInfo {
	return []push.DatasetInfo{
		{Name: "image_train", Intent: "train", Records: 20, Classes: 2, Extension: "jpg", SizeBytes: 13210,
			CreatedUnix: 1721556000,
			Columns:     []string{"id", "label", "data_intent", "data_id", "filename", "extension"}},
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
		"image_train", "20 images", "12.90 KiB", "jpg · 2 classes", "train", // rich image row
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
	row := datasetRow(empty, datasetModality(empty), 16, 8, 8, 16)
	if !strings.HasPrefix(row, "⚠") {
		t.Errorf("0-record dataset should lead with ⚠, got: %q", row)
	}
}

// TestRenderDataList_ColumnsAlign: rows with wide values (≥100 KiB sizes,
// ≥100-document counts) must stay column-aligned — the size and format columns
// should start at the same offset on every row. Regression for the fixed-width
// %9s/%-12s overflow (Bugbot, commit 1cfdcc3).
func TestRenderDataList_ColumnsAlign(t *testing.T) {
	infos := []push.DatasetInfo{
		// small: "5 documents" / "0.75 KiB" (both short)
		{Name: "text_small", Intent: "train", Records: 5, Classes: 2, Extension: "txt", SizeBytes: 770,
			Columns: []string{"id", "label", "data_intent", "data_id", "filename", "extension"}},
		// wide: "100000 documents" (16) + "100.00 KiB" (10) — both overflow the
		// old fixed %-12s / %9s and would shift later columns without dynamic sizing.
		{Name: "text_big", Intent: "test", Records: 100000, Classes: 2, Extension: "txt", SizeBytes: 102400,
			Columns: []string{"id", "label", "data_intent", "data_id", "filename", "extension"}},
	}
	var buf bytes.Buffer
	renderDataList(ui.New(&buf, ui.WithColor(false)), "ns", infos, false)
	out := buf.String()

	// Both rows are in the same "Text" group; find them and confirm the format
	// token ("txt · 2 classes") begins at the identical column on each.
	var offsets []int
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "text_small") || strings.Contains(ln, "text_big") {
			idx := strings.Index(ln, "txt · 2 classes")
			if idx < 0 {
				t.Fatalf("row missing format cell: %q", ln)
			}
			offsets = append(offsets, idx)
		}
	}
	if len(offsets) != 2 {
		t.Fatalf("want 2 text rows, found %d in:\n%s", len(offsets), out)
	}
	if offsets[0] != offsets[1] {
		t.Errorf("format column misaligned: offsets %v differ — wide values overflowed:\n%s", offsets, out)
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
		{push.DatasetInfo{Extension: "text"}, "Text"}, // .text is also a text extension
		{push.DatasetInfo{Columns: []string{"sequence_id", "timestamp", "hr"}}, "Time-series"},
		{push.DatasetInfo{Columns: []string{"timestamp", "value"}}, "Time-series"},
		{push.DatasetInfo{Columns: []string{"time", "event"}}, "Time-series"},
		{push.DatasetInfo{Columns: []string{"age", "income"}, Records: 3}, "Tabular"},
		// empty (0-row) file dataset: NULL extension, no schema cols → undetermined
		{push.DatasetInfo{Records: 0, Columns: []string{"id", "label", "filename", "extension"}}, "Other"},
		// A recorded task is authoritative — taken from the registry, not
		// inferred — and wins even over a misleading on-disk shape.
		{push.DatasetInfo{Task: "object_detection", Extension: "jpg"}, "Image"},
		{push.DatasetInfo{Task: "embeddings"}, "Text"},
		{push.DatasetInfo{Task: "tabular_regression"}, "Tabular"},
		// time-series tasks are FamilyTabular in the registry → "Tabular".
		{push.DatasetInfo{Task: "time_to_event_prediction"}, "Tabular"},
		{push.DatasetInfo{Task: "time_series_classification", Columns: []string{"sequence_id"}}, "Tabular"},
		{push.DatasetInfo{Task: "semantic_segmentation", Records: 5, Columns: []string{"a", "b"}}, "Image"},
		// Unknown task string → fall back to shape inference.
		{push.DatasetInfo{Task: "mystery_task", Extension: "jpg"}, "Image"},
	}
	for _, c := range cases {
		if got := datasetModality(c.d); got != c.want {
			t.Errorf("modality(%+v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// groupLabel uses the category registry's canonical label for a known task (so
// headers match the rest of the CLI), the raw id for an unknown task, and the
// inferred modality when no task was recorded.
func TestGroupLabel(t *testing.T) {
	cases := []struct {
		d        push.DatasetInfo
		modality string
		want     string
	}{
		{push.DatasetInfo{Task: "image_classification"}, "Image", "Image classification"},
		{push.DatasetInfo{Task: "time_series_classification"}, "Tabular", "Time-series classification"},
		{push.DatasetInfo{Task: "seq2seq"}, "Text", "Sequence-to-sequence"},
		{push.DatasetInfo{Task: "mystery_task"}, "Other", "mystery_task"}, // unknown → verbatim
		{push.DatasetInfo{Task: ""}, "Tabular", "Tabular"},                // no task → modality
	}
	for _, c := range cases {
		if got := groupLabel(c.d, c.modality); got != c.want {
			t.Errorf("groupLabel(task=%q) = %q, want %q", c.d.Task, got, c.want)
		}
	}
}

// Datasets with a recorded task group under the registry's task label (not the
// generic modality), ordered by modality family (Image before Tabular family).
func TestRenderDataList_GroupsByTask(t *testing.T) {
	infos := []push.DatasetInfo{
		{Name: "sepsis_train", Task: "time_series_classification", Intent: "train", Records: 4000, Classes: 2, SizeBytes: 20480,
			Columns: []string{"id", "label", "data_intent", "data_id", "sequence_id", "timestamp", "hr"}},
		{Name: "xray_train", Task: "image_classification", Intent: "train", Records: 50, Classes: 2, Extension: "jpg", SizeBytes: 1048576,
			Columns: []string{"id", "label", "data_intent", "data_id", "filename", "extension"}},
	}
	var buf bytes.Buffer
	renderDataList(ui.New(&buf, ui.WithColor(false)), "ns", infos, false)
	out := buf.String()

	for _, want := range []string{
		"Image classification · 1",       // registry label, not the bare "Image"
		"Time-series classification · 1", // canonical hyphenated label from the registry
		"xray_train", "50 images",
		"sepsis_train", "4000 rows", // time-series counts rows
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nImage · ") || strings.Contains(out, "\nTabular · ") {
		t.Errorf("known-task datasets must not fall back to bare modality headers:\n%s", out)
	}
	if strings.Index(out, "Image classification") > strings.Index(out, "Time-series classification") {
		t.Errorf("Image family should sort before the tabular family:\n%s", out)
	}
}

// A task that resolves to Image but whose extension wasn't recorded still reads
// "files" (the branch is reachable via task-derived modality).
func TestFormatCell_TaskImageNoExtension(t *testing.T) {
	d := push.DatasetInfo{Task: "image_classification", Records: 10, Extension: "",
		Columns: []string{"data_id", "filename"}}
	if got := formatCell(d, datasetModality(d)); got != "files" {
		t.Errorf("task-image without a recorded extension format = %q, want \"files\"", got)
	}
}

// The real task is surfaced in JSON (`task`), alongside the derived modality.
func TestWriteDataListJSON_IncludesTask(t *testing.T) {
	infos := []push.DatasetInfo{
		{Name: "xray_train", Task: "image_classification", Records: 50, Extension: "jpg", CreatedUnix: 1721556000,
			Columns: []string{"data_id", "label", "filename", "extension"}},
	}
	var buf bytes.Buffer
	writeDataListJSON(&buf, "ns", "tracebloc", infos, false)
	var got dataListJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if len(got.Details) != 1 || got.Details[0].Task != "image_classification" ||
		got.Details[0].Modality != "Image" {
		t.Errorf("details[].task/modality should carry the real task: %+v", got.Details)
	}
}

// An undetermined ("Other") dataset must not claim a "csv" format.
func TestFormatCell_OtherIsNeutral(t *testing.T) {
	d := push.DatasetInfo{Records: 0, Columns: []string{"id", "label", "filename", "extension"}}
	if got := formatCell(d, datasetModality(d)); got != "—" {
		t.Errorf("undetermined dataset format = %q, want em dash (not csv)", got)
	}
}

// A populated file table whose extension wasn't recorded can't be typed as
// Image vs Text (so it's "Other"), but it's still clearly file-based — format
// should read "files", not "—". Exercises the reachable default-case fallback.
func TestFormatCell_FileWithoutExtension(t *testing.T) {
	d := push.DatasetInfo{Records: 12, Extension: "", Columns: []string{"id", "label", "data_id", "filename"}}
	if m := datasetModality(d); m != "Other" {
		t.Errorf("extension-less file table modality = %q, want Other", m)
	}
	if got := formatCell(d, datasetModality(d)); got != "files" {
		t.Errorf("file-based (has filename, no extension) format = %q, want \"files\"", got)
	}
}

// TestWriteDataListJSON: `datasets` stays a string array (additive contract);
// the rich objects go in the new `details`. System excluded unless --all; an
// empty result marshals both as [] (not null).
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
	// `datasets` is still a []string of names (contract-preserving).
	if len(got.Datasets) != 4 {
		t.Errorf("datasets (names) count = %d, want 4", len(got.Datasets))
	}
	foundName := false
	for _, n := range got.Datasets {
		if n == "image_train" {
			foundName = true
		}
	}
	if !foundName {
		t.Errorf("datasets should list names incl image_train: %v", got.Datasets)
	}
	// `details` carries the rich objects.
	var img *datasetJSON
	for i := range got.Details {
		if got.Details[i].Name == "image_train" {
			img = &got.Details[i]
		}
		if got.Details[i].System {
			t.Errorf("system table must be excluded without --all: %+v", got.Details[i])
		}
	}
	if img == nil || img.Modality != "Image" || img.Records != 20 || img.Format != "jpg · 2 classes" {
		t.Errorf("image dataset detail wrong: %+v", img)
	}
	// `ingested` must be timezone-explicit (UTC, Z-suffixed RFC3339) so JSON
	// consumers can't misread it as a naive local time. (Bugbot: JSON tz.)
	if img != nil {
		if !strings.HasSuffix(img.Ingested, "Z") {
			t.Errorf("ingested %q must be Z-suffixed UTC, not a naive timestamp", img.Ingested)
		}
		if _, err := time.Parse(time.RFC3339, img.Ingested); err != nil {
			t.Errorf("ingested %q is not valid RFC3339: %v", img.Ingested, err)
		}
	}

	buf.Reset()
	writeDataListJSON(&buf, "ns1", "tracebloc", nil, false)
	for _, want := range []string{`"datasets": []`, `"details": []`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("nil result should marshal %s:\n%s", want, buf.String())
		}
	}

	buf.Reset()
	writeDataListJSON(&buf, "ns1", "tracebloc", sampleInfos(), true)
	_ = json.Unmarshal(buf.Bytes(), &got)
	if got.Count != 5 {
		t.Errorf("--all JSON should include the system table (count 5), got %d", got.Count)
	}
}
