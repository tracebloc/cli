package submit

import (
	"strings"
	"testing"
)

// realIngestorBanner mirrors what
// tracebloc_ingestor/ingestors/base.py:624+ actually prints,
// ANSI codes and all. The parser strips ANSI before matching so
// the test is bit-exact with the production logs.
//
// The leading "preamble" line + trailing "post-banner" lines
// simulate what real ingestor logs look like — the parser must
// ignore non-banner content + finalize on the closing rule.
var realIngestorBanner = "starting up...\n" +
	"loaded 1234 rows from labels.csv\n" +
	"\n" +
	"\x1b[36m" + strings.Repeat("═", 60) + "\x1b[0m\n" +
	"\x1b[1m\x1b[36m📊 INGESTION SUMMARY 📊\x1b[0m\n" +
	"\x1b[36m" + strings.Repeat("═", 60) + "\x1b[0m\n" +
	"\x1b[1mIngestor ID:\x1b[0m                \x1b[34mrun-abc-123\x1b[0m\n" +
	"\x1b[1m📈 Total Records Found:\x1b[0m     \x1b[34m1,234\x1b[0m\n" +
	"\x1b[1m✅ Successfully Processed:\x1b[0m  \x1b[32m1,200\x1b[0m\n" +
	"\x1b[1m💾 Inserted to Database:\x1b[0m    \x1b[32m1,200\x1b[0m\n" +
	"\x1b[1m🚀 Sent to API:\x1b[0m             \x1b[32m1,150\x1b[0m\n" +
	"\x1b[1m⏭️  Skipped Records:\x1b[0m        \x1b[33m4\x1b[0m\n" +
	"\x1b[1m📁 File Transfer Failures:\x1b[0m  \x1b[32m0\x1b[0m\n" +
	"\x1b[1m❌ Failed DB Insertion:\x1b[0m     \x1b[31m30\x1b[0m\n" +
	"\x1b[36m" + strings.Repeat("═", 60) + "\x1b[0m\n" +
	"ingestor exiting cleanly\n"

// TestSummaryParser_RealBannerEndToEnd pins the parser against
// the actual ingestor's output format. If a regression breaks
// any field's extraction, this test fails with a clear "got X
// want Y" for that specific counter.
func TestSummaryParser_RealBannerEndToEnd(t *testing.T) {
	p := NewSummaryParser()
	p.Feed([]byte(realIngestorBanner))

	got := p.Result()
	if got == nil {
		t.Fatal("parser returned nil Result; expected populated Summary")
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"IngestorID", got.IngestorID, "run-abc-123"},
		{"TotalRecords", got.TotalRecords, int64(1234)},
		{"ProcessedRecords", got.ProcessedRecords, int64(1200)},
		{"InsertedRecords", got.InsertedRecords, int64(1200)},
		{"APISentRecords", got.APISentRecords, int64(1150)},
		{"SkippedRecords", got.SkippedRecords, int64(4)},
		{"FileTransferFailures", got.FileTransferFailures, int64(0)},
		{"FailedRecords", got.FailedRecords, int64(30)},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestSummaryParser_HasFailures pins the failure-detection logic
// that the orchestrator uses to choose between success exit code
// (0) and ingest-failure exit code (9).
func TestSummaryParser_HasFailures(t *testing.T) {
	cases := []struct {
		name string
		s    *Summary
		want bool
	}{
		{"nil", nil, false},
		{"all zero", &Summary{TotalRecords: 100, ProcessedRecords: 100}, false},
		{"file transfer failures", &Summary{FileTransferFailures: 1}, true},
		{"failed records", &Summary{FailedRecords: 1}, true},
		{"both", &Summary{FileTransferFailures: 1, FailedRecords: 1}, true},
		// Skipped records are NOT failures — they're rows that
		// validators rejected. The customer wants to see the
		// count but it doesn't change the exit code.
		{"skipped is not failure", &Summary{SkippedRecords: 100}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.HasFailures(); got != c.want {
				t.Errorf("HasFailures = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSummaryParser_SuccessRate pins the math that feeds the
// rendered panel's "Success rate: XX%" line. Divide-by-zero on
// empty banner is the critical edge case.
func TestSummaryParser_SuccessRate(t *testing.T) {
	cases := []struct {
		name string
		s    *Summary
		want float64
	}{
		{"nil", nil, 0},
		{"empty banner", &Summary{}, 0},
		{"100%", &Summary{TotalRecords: 100, ProcessedRecords: 100}, 100},
		{"50%", &Summary{TotalRecords: 100, ProcessedRecords: 50}, 50},
		{"all failed", &Summary{TotalRecords: 100}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.SuccessRate(); got != c.want {
				t.Errorf("SuccessRate = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSummaryParser_StreamedInChunks: real log streams arrive in
// arbitrary-sized chunks. Feed the banner one byte at a time to
// pin that the parser correctly buffers and finalizes despite
// partial-line input.
func TestSummaryParser_StreamedInChunks(t *testing.T) {
	p := NewSummaryParser()
	for i := 0; i < len(realIngestorBanner); i++ {
		p.Feed([]byte{realIngestorBanner[i]})
	}
	got := p.Result()
	if got == nil {
		t.Fatal("byte-by-byte feed produced nil Result")
	}
	if got.TotalRecords != 1234 || got.FailedRecords != 30 {
		t.Errorf("chunked feed produced different result than monolithic: TotalRecords=%d FailedRecords=%d",
			got.TotalRecords, got.FailedRecords)
	}
}

// TestSummaryParser_NoBanner: if the run died before producing a
// banner (image crashloop, OOM at startup), Result returns nil
// rather than an empty Summary. The orchestrator uses this
// nil-check to decide whether to render the panel at all.
func TestSummaryParser_NoBanner(t *testing.T) {
	p := NewSummaryParser()
	p.Feed([]byte("starting up...\nerror: connection refused\n"))
	if got := p.Result(); got != nil {
		t.Errorf("Result on no-banner log = %+v, want nil", got)
	}
}

// TestSummaryParser_PostBannerLogsIgnored: lines after the closing
// ═-rule are ignored. The ingestor may print shutdown messages
// after the summary; those shouldn't perturb the parsed counts
// (e.g. a regex-misfire interpreting "1234" in an unrelated log
// line as TotalRecords).
func TestSummaryParser_PostBannerLogsIgnored(t *testing.T) {
	p := NewSummaryParser()
	p.Feed([]byte(realIngestorBanner))
	pre := *p.Result()
	p.Feed([]byte("📈 Total Records Found: 999999999\n")) // would alter TotalRecords if not finalized
	post := *p.Result()
	if pre != post {
		t.Errorf("post-banner line altered Summary; pre=%+v post=%+v", pre, post)
	}
}

// TestStripANSI: the parser strips ANSI SGR codes from each line
// before matching. Validate the regex handles common shapes.
func TestStripANSI(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain text", "plain text"},
		{"\x1b[1mbold\x1b[0m", "bold"},
		{"\x1b[1;36mbold-cyan\x1b[0m", "bold-cyan"},
		{"prefix\x1b[31mred\x1b[0msuffix", "prefixredsuffix"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripANSI(c.in); got != c.want {
			t.Errorf("stripANSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRenderPanel_BasicShape: the panel rendering is what the
// customer sees on success; pin a few key lines so a refactor
// breaks the test rather than silently producing weird output.
func TestRenderPanel_BasicShape(t *testing.T) {
	s := &Summary{
		IngestorID:           "run-abc",
		TotalRecords:         1234567,
		InsertedRecords:      1200000,
		APISentRecords:       1150000,
		SkippedRecords:       4000,
		FileTransferFailures: 30,
		FailedRecords:        5,
	}
	got := RenderPanel(s)
	for _, want := range []string{
		"Ingestion summary",
		"run-abc",
		"1,234,567", // commaSep formatting
		"1,200,000", // commaSep formatting
		"30",        // file transfer failures
		"DB-insert failures:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderPanel missing %q in:\n%s", want, got)
		}
	}
}

// TestRenderPanel_Nil: nil summary returns empty string so the
// orchestrator can blind-print without a guard.
func TestRenderPanel_Nil(t *testing.T) {
	if got := RenderPanel(nil); got != "" {
		t.Errorf("RenderPanel(nil) = %q, want empty", got)
	}
}

// TestCommaSep: small helper test. Pin the boundary cases that
// would catch off-by-one in the comma-insertion loop.
func TestCommaSep(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
		{-1234, "-1,234"},
	}
	for _, c := range cases {
		if got := commaSep(c.in); got != c.want {
			t.Errorf("commaSep(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
