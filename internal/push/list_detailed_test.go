package push

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

// seqExecutor is an Executor that returns queued stdout per call and captures
// the SQL each call received on stdin — enough to drive the two-query
// listDatasetsDetailedWith path. Calls whose index is in errCalls return an
// error instead, to exercise the retry-on-vanished-table path.
type seqExecutor struct {
	outs     [][]byte     // stdout for call 0, 1, …
	queries  []string     // stdin (the SQL) captured per call
	errCalls map[int]bool // 0-based call indices that should fail
	call     int
}

func (e *seqExecutor) Exec(_ context.Context, _, _, _ string, _ []string,
	stdin io.Reader, stdout, _ io.Writer) error {
	idx := e.call
	e.call++
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		e.queries = append(e.queries, string(b))
	}
	if e.errCalls[idx] {
		return fmt.Errorf("simulated exec failure on call %d", idx)
	}
	if idx < len(e.outs) && stdout != nil {
		_, _ = stdout.Write(e.outs[idx])
	}
	return nil
}

func TestParseSchemaRows(t *testing.T) {
	raw := "image_train\t1721556000\t40960\tid,label,data_id,extension\n" +
		"empty_cols\t0\t0\t\n" +
		"  \n" // blank line skipped
	got := parseSchemaRows(raw)
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d: %#v", len(got), got)
	}
	d := got[0]
	if d.Name != "image_train" || d.CreatedUnix != 1721556000 || d.DBBytes != 40960 {
		t.Errorf("row0 wrong: %+v", d)
	}
	if len(d.Columns) != 4 || d.Columns[0] != "id" {
		t.Errorf("row0 columns wrong: %#v", d.Columns)
	}
	if got[1].Name != "empty_cols" || got[1].DBBytes != 0 || len(got[1].Columns) != 0 {
		t.Errorf("row1 (no columns) wrong: %+v", got[1])
	}
}

func TestApplyDataRows(t *testing.T) {
	infos := []DatasetInfo{{Name: "a"}, {Name: "b"}}
	applyDataRows(infos, "a\ttrain\t20\t2\tjpg\nb\ttest\t5\t0\t\n")
	if infos[0].Intent != "train" || infos[0].Records != 20 || infos[0].Classes != 2 || infos[0].Extension != "jpg" {
		t.Errorf("a wrong: %+v", infos[0])
	}
	if infos[1].Intent != "test" || infos[1].Records != 5 || infos[1].Classes != 0 || infos[1].Extension != "" {
		t.Errorf("b wrong: %+v", infos[1])
	}
}

func TestParseDuOutput(t *testing.T) {
	// `du -sk` output: "<KiB>\t<path>", keyed by basename → bytes.
	raw := "80\t/data/shared/image_train\n" +
		"8\t/data/shared/clm_test\n" +
		"40\t/data/shared/.tracebloc-staging\n" +
		"junk line\n"
	m := parseDuOutput(raw)
	if m["image_train"] != 80*1024 {
		t.Errorf("image_train = %d, want %d", m["image_train"], 80*1024)
	}
	if m["clm_test"] != 8*1024 {
		t.Errorf("clm_test = %d, want %d", m["clm_test"], 8*1024)
	}
	if _, ok := m["junk"]; ok {
		t.Errorf("non-numeric line should be skipped, got %v", m)
	}
}

// End-to-end over the two-query path: a real dataset (has data_id) is merged
// with its data row; a system table (no data_id) is marked System and excluded
// from the second (data) query, whose SQL selects data_intent — which a system
// table lacks.
func TestListDatasetsDetailedWith(t *testing.T) {
	schema := "image_train\t1721556000\t131072\tid,label,data_intent,data_id,filename,extension\n" +
		"tracebloc_ingest_runs\t1721552400\t16384\tingestor_id,table_name,registered\n"
	data := "image_train\ttrain\t20\t2\t.jpg\n" // leading dot must be stripped
	fe := &seqExecutor{outs: [][]byte{[]byte(schema), []byte(data)}}

	infos, err := listDatasetsDetailedWith(context.Background(), fe, "tracebloc", "mysql-0", "mysql")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("want 2 infos, got %d: %#v", len(infos), infos)
	}

	var img, sys *DatasetInfo
	for i := range infos {
		switch infos[i].Name {
		case "image_train":
			img = &infos[i]
		case "tracebloc_ingest_runs":
			sys = &infos[i]
		}
	}
	if img == nil || img.System || img.Records != 20 || img.Classes != 2 || img.Intent != "train" ||
		img.Extension != "jpg" {
		t.Errorf("image_train merged wrong: %+v", img)
	}
	if sys == nil || !sys.System || sys.Records != 0 {
		t.Errorf("system table should be flagged System with no data: %+v", sys)
	}

	// First query is the schema pass; second is the data pass over real tables only.
	if len(fe.queries) != 2 {
		t.Fatalf("want 2 queries, got %d", len(fe.queries))
	}
	if !strings.Contains(fe.queries[0], "information_schema") {
		t.Errorf("query 0 should hit information_schema: %s", fe.queries[0])
	}
	if !strings.Contains(fe.queries[1], "image_train") {
		t.Errorf("data query should include the real dataset: %s", fe.queries[1])
	}
	if strings.Contains(fe.queries[1], "tracebloc_ingest_runs") {
		t.Errorf("data query must exclude the system table (no data_id): %s", fe.queries[1])
	}
}

// A table dropped between the schema snapshot and the data pass fails the whole
// UNION ALL; listDatasetsDetailedWith retries with a fresh snapshot, which no
// longer lists the vanished table, and succeeds.
func TestListDatasetsDetailedWith_RetriesOnVanishedTable(t *testing.T) {
	// Attempt 1 lists a + b; its data query (call 1) fails as if b was just
	// dropped. Attempt 2's fresh snapshot lists only a, and its data succeeds.
	schema1 := "a\t1721556000\t8192\tdata_id,label,data_intent\n" +
		"b\t1721556000\t8192\tdata_id,label,data_intent\n"
	schema2 := "a\t1721556000\t8192\tdata_id,label,data_intent\n"
	data2 := "a\ttrain\t7\t2\t\n"
	fe := &seqExecutor{
		outs:     [][]byte{[]byte(schema1), nil, []byte(schema2), []byte(data2)},
		errCalls: map[int]bool{1: true}, // the first data query fails
	}

	infos, err := listDatasetsDetailedWith(context.Background(), fe, "ns", "mysql-0", "mysql")
	if err != nil {
		t.Fatalf("expected retry to succeed after a vanished table, got: %v", err)
	}
	if len(infos) != 1 || infos[0].Name != "a" || infos[0].Records != 7 {
		t.Fatalf("want dataset a with 7 records after retry, got: %#v", infos)
	}
	if fe.call != 4 {
		t.Errorf("want 4 execs (schema, failed data, schema, data), got %d", fe.call)
	}
}

func TestApplyDatasetSizes(t *testing.T) {
	// filename/extension are framework columns on EVERY table, so the file-vs-row
	// signal is the per-row Extension: set → file dataset (PVC/du), empty →
	// row-based (DB data_length).
	infos := []DatasetInfo{
		{Name: "img_train", Extension: "jpg", Records: 20, DBBytes: 4096,
			Columns: []string{"data_id", "filename", "extension"}},
		{Name: "tab_train", Extension: "", Records: 20, DBBytes: 24576,
			Columns: []string{"data_id", "filename", "extension", "age", "income"}}, // has filename col yet is row-based
		{Name: "img_nodu", Extension: "jpg", Records: 20, DBBytes: 4096,
			Columns: []string{"data_id", "filename", "extension"}},
		{Name: "empty_ds", Extension: "", Records: 0, DBBytes: 16384,
			Columns: []string{"data_id", "filename", "extension"}}, // 0 rows → sizeless, not the page allocation
	}
	applyDatasetSizes(infos, map[string]int64{"img_train": 1048576})
	if infos[0].SizeBytes != 1048576 {
		t.Errorf("file dataset should take the du size, got %d", infos[0].SizeBytes)
	}
	if infos[1].SizeBytes != 24576 {
		t.Errorf("row-based dataset (empty extension) should take DBBytes despite the framework filename column, got %d", infos[1].SizeBytes)
	}
	if infos[2].SizeBytes != 0 {
		t.Errorf("file dataset without a du entry must stay 0 (—), not the misleading DBBytes, got %d", infos[2].SizeBytes)
	}
	if infos[3].SizeBytes != 0 {
		t.Errorf("empty dataset (0 rows) must stay 0 (—), not the InnoDB page size, got %d", infos[3].SizeBytes)
	}
}

func TestParseTaskRows(t *testing.T) {
	raw := "xray_train\timage_classification\n" +
		"vitals_train\ttime_series_classification\n" +
		"blank_task\t\n" + // blank task skipped (pre-persistence run)
		"short\n" // malformed line skipped
	m := parseTaskRows(raw)
	if m["xray_train"] != "image_classification" ||
		m["vitals_train"] != "time_series_classification" {
		t.Errorf("task map wrong: %#v", m)
	}
	if _, ok := m["blank_task"]; ok {
		t.Errorf("blank task should be skipped: %#v", m)
	}
	if _, ok := m["short"]; ok {
		t.Errorf("malformed line should be skipped: %#v", m)
	}
}

// When the runs journal carries a task column, a third query maps each dataset
// to its real task and tags the DatasetInfo.
func TestListDatasetsDetailedWith_AppliesTask(t *testing.T) {
	schema := "xray_train\t1721556000\t131072\tdata_id,label,data_intent,filename,extension\n" +
		"tracebloc_ingest_runs\t1721552400\t16384\tingestor_id,table_name,registered,task\n"
	data := "xray_train\ttrain\t50\t2\t.jpg\n"
	taskMap := "xray_train\timage_classification\n"
	fe := &seqExecutor{outs: [][]byte{[]byte(schema), []byte(data), []byte(taskMap)}}

	infos, err := listDatasetsDetailedWith(context.Background(), fe, "ns", "mysql-0", "mysql")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var x *DatasetInfo
	for i := range infos {
		if infos[i].Name == "xray_train" {
			x = &infos[i]
		}
	}
	if x == nil || x.Task != "image_classification" {
		t.Fatalf("want xray_train tagged image_classification, got: %#v", x)
	}
	if len(fe.queries) != 3 {
		t.Fatalf("want 3 queries (schema, data, task), got %d", len(fe.queries))
	}
	if !strings.Contains(fe.queries[2], "tracebloc_ingest_runs") ||
		!strings.Contains(fe.queries[2], "task") {
		t.Errorf("third query should read task from the runs journal: %s", fe.queries[2])
	}
}

// A journal predating the task column (no task in its schema) is never queried
// for it — the datasets keep an empty Task and the caller infers modality.
func TestListDatasetsDetailedWith_SkipsTaskWhenColumnAbsent(t *testing.T) {
	schema := "xray_train\t1721556000\t131072\tdata_id,label,data_intent,filename,extension\n" +
		"tracebloc_ingest_runs\t1721552400\t16384\tingestor_id,table_name,registered\n"
	data := "xray_train\ttrain\t50\t2\t.jpg\n"
	fe := &seqExecutor{outs: [][]byte{[]byte(schema), []byte(data)}}

	infos, err := listDatasetsDetailedWith(context.Background(), fe, "ns", "mysql-0", "mysql")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := range infos {
		if infos[i].Task != "" {
			t.Errorf("no task column → Task must stay empty, got %q", infos[i].Task)
		}
	}
	if len(fe.queries) != 2 {
		t.Errorf("task lookup must be skipped when the column is absent; got %d queries", len(fe.queries))
	}
}
