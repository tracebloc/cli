package submit

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/ui"
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
	// Mirrors the ingestor's IngestionSummary.has_failures exactly.
	cases := []struct {
		name string
		s    *Summary
		want bool
	}{
		{"nil", nil, false},
		// A genuinely clean run: every counter equal, nothing skipped/failed.
		{"clean", &Summary{TotalRecords: 100, ProcessedRecords: 100, InsertedRecords: 100, APISentRecords: 100}, false},
		{"file transfer failures", &Summary{TotalRecords: 1, InsertedRecords: 1, APISentRecords: 1, FileTransferFailures: 1}, true},
		{"failed records", &Summary{TotalRecords: 1, InsertedRecords: 1, APISentRecords: 1, FailedRecords: 1}, true},
		// Skipped rows ARE a failure — a dropped row is silent data loss
		// (#234); the ingestor counts it, so the CLI must too (was the bug).
		{"skipped is a failure", &Summary{TotalRecords: 100, InsertedRecords: 100, APISentRecords: 100, SkippedRecords: 5}, true},
		// Fewer rows in MySQL than the ingestor saw → partial run.
		{"inserted < total", &Summary{TotalRecords: 100, ProcessedRecords: 100, InsertedRecords: 99, APISentRecords: 99}, true},
		// Rows in MySQL but the central catalog got fewer.
		{"api_sent < inserted", &Summary{TotalRecords: 100, InsertedRecords: 100, APISentRecords: 99}, true},
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
	// Rate is INSERTED/total (matches the ingestor banner), not processed/total.
	cases := []struct {
		name string
		s    *Summary
		want float64
	}{
		{"nil", nil, 0},
		{"empty banner", &Summary{}, 0},
		{"100%", &Summary{TotalRecords: 100, ProcessedRecords: 100, InsertedRecords: 100}, 100},
		{"50%", &Summary{TotalRecords: 100, InsertedRecords: 50}, 50},
		// The overstatement the fix closes: all rows validated (processed=100)
		// but only 70 landed in MySQL → 70%, not the old 100%.
		{"processed overstates: inserted<processed", &Summary{TotalRecords: 100, ProcessedRecords: 100, InsertedRecords: 70}, 70},
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

// TestSummaryParser_BufferBoundedOnNewlinelessFlood pins finding D3
// (deferred from the v0.8.0 review): a pathological ingestor can emit
// many MB of tqdm '\r'-redraws with no '\n' for the life of a run. The
// display path drains those past displayLineMax, but the drained bytes
// still flow through the TeeReader into Feed — so without a matching
// bound the parser's buf would grow unbounded. Assert buf stays within
// parserLineMax under the flood, and that a real banner arriving after
// the flood terminates (a '\n' finally lands) still parses.
func TestSummaryParser_BufferBoundedOnNewlinelessFlood(t *testing.T) {
	p := NewSummaryParser()

	// tqdm-style redraw: '\r' + progress text, never a '\n'. Feed well
	// past parserLineMax in bounded chunks so we also exercise the
	// across-Feed-calls accumulation, not just one giant Write.
	chunk := []byte("\r" + strings.Repeat("#", 512*1024-1)) // 512 KiB, no '\n'
	for total := 0; total <= parserLineMax*2; total += len(chunk) {
		p.Feed(chunk)
		if p.buf.Len() > parserLineMax {
			t.Fatalf("buf grew to %d bytes after a newline-less flood, exceeds parserLineMax=%d",
				p.buf.Len(), parserLineMax)
		}
	}

	// The flood is one newline-less line; none of it should have been
	// mistaken for a banner.
	if got := p.Result(); got != nil {
		t.Fatalf("newline-less flood produced a non-nil Summary: %+v", got)
	}

	// The pathological line finally terminates and a real banner
	// follows. The parser must recover: drop the oversized line's tail,
	// then parse the banner that comes after.
	p.Feed([]byte("\n"))
	p.Feed([]byte(realIngestorBanner))

	got := p.Result()
	if got == nil {
		t.Fatal("banner after a newline-less flood did not parse; Result is nil")
	}
	if got.TotalRecords != 1234 || got.FailedRecords != 30 {
		t.Errorf("post-flood banner parsed wrong: TotalRecords=%d FailedRecords=%d, want 1234/30",
			got.TotalRecords, got.FailedRecords)
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

// TestRenderSummary_BasicShape pins the key facts the customer sees;
// a refactor that drops one breaks the test rather than silently
// producing weird output. Color off so we assert plain text.
func TestRenderSummary_BasicShape(t *testing.T) {
	s := &Summary{
		IngestorID:           "run-abc",
		TotalRecords:         1234567,
		InsertedRecords:      1200000,
		APISentRecords:       1150000,
		SkippedRecords:       4000,
		FileTransferFailures: 30,
		FailedRecords:        5,
	}
	var buf bytes.Buffer
	RenderSummary(ui.New(&buf, ui.WithColor(false)), s)
	got := buf.String()
	for _, want := range []string{
		"Ingestion summary",
		"run-abc",
		"1,234,567", // commaSep formatting
		"1,200,000",
		"30", // file failures
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderSummary missing %q in:\n%s", want, got)
		}
	}
}

// TestRenderSummary_Nil: a nil summary writes nothing, so the
// orchestrator can call it unconditionally.
func TestRenderSummary_Nil(t *testing.T) {
	var buf bytes.Buffer
	RenderSummary(ui.New(&buf, ui.WithColor(false)), nil)
	if buf.Len() != 0 {
		t.Errorf("RenderSummary(nil) wrote %q, want nothing", buf.String())
	}
}

// TestRenderSummary_OutcomeHeadline: the headline reflects the outcome
// derived from the counts — clean / skips / failures. Table-driven,
// one sub-test per row via t.Run.
func TestRenderSummary_OutcomeHeadline(t *testing.T) {
	cases := []struct {
		name string
		s    *Summary
		want string
	}{
		{"clean", &Summary{TotalRecords: 10, ProcessedRecords: 10, InsertedRecords: 10, APISentRecords: 10}, "complete —"},
		{"skips", &Summary{TotalRecords: 10, ProcessedRecords: 8, InsertedRecords: 8, APISentRecords: 8, SkippedRecords: 2}, "skips"},
		// Soft shortfall with ZERO skips: inserted < total (and api_sent <
		// inserted) but nothing was skipped. Must NOT be labeled "skips" — it's
		// a partial result, not a validator drop. (Bugbot #193.)
		{"partial, no skips", &Summary{TotalRecords: 10, ProcessedRecords: 10, InsertedRecords: 9, APISentRecords: 8}, "partially"},
		{"failures", &Summary{TotalRecords: 10, ProcessedRecords: 7, InsertedRecords: 7, APISentRecords: 7, FailedRecords: 3}, "failures"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			RenderSummary(ui.New(&buf, ui.WithColor(false)), c.s)
			if !strings.Contains(buf.String(), c.want) {
				t.Errorf("headline missing %q in:\n%s", c.want, buf.String())
			}
		})
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
