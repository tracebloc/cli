package push

import (
	"context"
	"io"
	"strings"
	"testing"
)

// seqExecutor is an Executor that returns queued stdout per call and captures
// the SQL each call received on stdin — enough to drive the two-query
// listDatasetsDetailedWith path.
type seqExecutor struct {
	outs    [][]byte // stdout for call 0, 1, …
	queries []string // stdin (the SQL) captured per call
	call    int
	err     error
}

func (e *seqExecutor) Exec(_ context.Context, _, _, _ string, _ []string,
	stdin io.Reader, stdout, _ io.Writer) error {
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		e.queries = append(e.queries, string(b))
	}
	if e.err != nil {
		return e.err
	}
	if e.call < len(e.outs) && stdout != nil {
		_, _ = stdout.Write(e.outs[e.call])
	}
	e.call++
	return nil
}

func TestParseSchemaRows(t *testing.T) {
	raw := "image_train\t2026-07-21T10:00:00\t1721556000\tid,label,data_id,extension\n" +
		"empty_cols\t\t0\t\n" +
		"  \n" // blank line skipped
	got := parseSchemaRows(raw)
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d: %#v", len(got), got)
	}
	d := got[0]
	if d.Name != "image_train" || d.CreatedAt != "2026-07-21T10:00:00" || d.CreatedUnix != 1721556000 {
		t.Errorf("row0 wrong: %+v", d)
	}
	if len(d.Columns) != 4 || d.Columns[0] != "id" {
		t.Errorf("row0 columns wrong: %#v", d.Columns)
	}
	if got[1].Name != "empty_cols" || len(got[1].Columns) != 0 {
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
	schema := "image_train\t2026-07-21T10:00:00\t1721556000\tid,label,data_intent,data_id,filename,extension\n" +
		"tracebloc_ingest_runs\t2026-07-21T09:00:00\t1721552400\tingestor_id,table_name,registered\n"
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
