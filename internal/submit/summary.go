package submit

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/tracebloc/cli/internal/ui"
)

// Summary is the parsed contents of the ingestor's 📊 INGESTION
// SUMMARY 📊 banner. Fields mirror what
// tracebloc_ingestor/ingestors/base.py:624+ prints. Zero values
// are valid — an early failure may produce a summary with most
// counts at 0 (the ingestor still prints the banner so operators
// can see what got through).
//
// All counts are int64 to fit row counts well past int32's 2.1B
// ceiling — a customer ingesting a few-billion-row table would
// silently truncate with int.
type Summary struct {
	// IngestorID is the run identifier the ingestor logs at the
	// top of the banner. Useful in the customer-facing panel as
	// "you can grep cluster logs for this ID."
	IngestorID string

	// TotalRecords is the row count the ingestor saw in the
	// source data. Includes every row regardless of outcome.
	TotalRecords int64

	// ProcessedRecords is the row count that made it through
	// validation (passed FileTypeValidator, ImageResolutionValidator,
	// etc.). Excludes invalid rows.
	ProcessedRecords int64

	// InsertedRecords is the row count that landed in the
	// cluster-internal MySQL. The "I actually have this data
	// staged" metric — this is what matters for downstream
	// training jobs.
	InsertedRecords int64

	// APISentRecords is the row count that synced metadata to
	// the central tracebloc backend. Only the row count + label
	// is sent, not the raw data; this is the "central catalog
	// knows about this dataset" metric.
	APISentRecords int64

	// SkippedRecords is the row count rejected by validators
	// (wrong dimensions, missing image file, etc.). Non-fatal
	// for the run but worth surfacing — a customer with 50%
	// skipped wants to see that.
	SkippedRecords int64

	// FileTransferFailures is the count of files (NOT rows) that
	// failed to transfer to the requests-proxy. Distinct from
	// FailedRecords because file transfer is a separate stage
	// from DB insertion. Non-zero here is the dominant "your
	// network is flaky" signal.
	FileTransferFailures int64

	// FailedRecords is the row count that errored at the
	// DB-insert stage (constraint violation, type mismatch,
	// connection drop). The catch-all "something went wrong
	// at the storage layer" bucket.
	FailedRecords int64
}

// HasFailures returns true if any failure-class counter is non-zero.
// Used by the orchestrator to decide which exit code to return
// (success: 0, ingest-with-failures: non-zero) and how to color
// the rendered panel.
func (s *Summary) HasFailures() bool {
	if s == nil {
		return false
	}
	return s.FileTransferFailures > 0 || s.FailedRecords > 0
}

// SuccessRate returns a 0-100 percentage for the panel header.
// Defined as ProcessedRecords / TotalRecords; returns 0 when
// TotalRecords is 0 to avoid divide-by-zero in early-failure
// banners.
func (s *Summary) SuccessRate() float64 {
	if s == nil || s.TotalRecords == 0 {
		return 0
	}
	return float64(s.ProcessedRecords) / float64(s.TotalRecords) * 100
}

// ansiCodeRE matches the ANSI SGR (Select Graphic Rendition)
// escape sequences the ingestor uses for its color output —
// `\x1b[1m` (bold), `\x1b[36m` (cyan), `\x1b[0m` (reset), etc.
// The ingestor prints these inline in the summary text; we strip
// them before parsing so a future palette change doesn't break
// the parser.
var ansiCodeRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripANSI removes SGR codes from a single line, returning the
// printable text. Implementation note: this is a hot path for the
// log stream so we avoid an allocation when no codes are present.
func stripANSI(line string) string {
	if !strings.Contains(line, "\x1b[") {
		return line
	}
	return ansiCodeRE.ReplaceAllString(line, "")
}

// bannerStartMarker is the ingestor's literal banner-header line
// (with the BOLD + CYAN ANSI prefix stripped by stripANSI before
// matching). When we see this, we flip to "inside-banner" mode.
const bannerStartMarker = "📊 INGESTION SUMMARY 📊"

// bannerEndMarker is the equals-rule the ingestor prints AFTER
// the last metric line (a second `═`x60 line). We use it as the
// terminator so a parser at end-of-stream emits a complete
// Summary even if the Pod was killed mid-log-flush.
const bannerEndMarker = "════════════════════════════════════════════════════════════"

// fieldPatterns are the regex patterns for each Summary field,
// keyed by the human-readable label the ingestor prints. The
// values are populated by NewSummaryParser via Compile-once-at-
// init. Each pattern matches a `Label:  <number>` shape with
// optional spacing and an optional `,`-separated digit format
// (e.g. "1,234,567").
//
// Maintained as a parallel slice of {label, pointer-target}
// rather than a map so the order of parsing matches the order
// the ingestor prints them — useful if a future ingestor adds a
// new line, the parser doesn't have to re-scan the existing ones.
var fieldPatterns = []struct {
	prefix string
	apply  func(s *Summary, n int64)
}{
	{"📈 Total Records Found:", func(s *Summary, n int64) { s.TotalRecords = n }},
	{"✅ Successfully Processed:", func(s *Summary, n int64) { s.ProcessedRecords = n }},
	{"💾 Inserted to Database:", func(s *Summary, n int64) { s.InsertedRecords = n }},
	{"🚀 Sent to API:", func(s *Summary, n int64) { s.APISentRecords = n }},
	{"⏭️  Skipped Records:", func(s *Summary, n int64) { s.SkippedRecords = n }},
	{"📁 File Transfer Failures:", func(s *Summary, n int64) { s.FileTransferFailures = n }},
	{"❌ Failed DB Insertion:", func(s *Summary, n int64) { s.FailedRecords = n }},
}

// numberRE captures the trailing digit-group on a metric line.
// Allows optional thousands-separator commas the ingestor's
// `f"{count:,}"` formatting emits.
var numberRE = regexp.MustCompile(`([0-9][0-9,]*)\s*$`)

// ingestorIDRE matches the ingestor-ID line specifically; the
// value is a UUID-ish string, not a number, so it gets its own
// pattern.
var ingestorIDRE = regexp.MustCompile(`Ingestor ID:\s*(.+?)\s*$`)

// SummaryParser is a streaming parser for the 📊 banner. Feed it
// log lines as they arrive (any chunk size, any line splitting);
// Result() returns the accumulated Summary at any point. The
// banner-end marker latches the result so post-banner log lines
// don't perturb it.
//
// The parser is stateful but not thread-safe — the watch loop
// uses it from a single goroutine (the log-streaming TeeReader),
// so no synchronization needed.
type SummaryParser struct {
	// buf accumulates partial-line input across Feed calls. The
	// log stream from the API server arrives in TCP-sized chunks
	// that may split lines; we buffer until we see a '\n' to
	// finalize each line.
	buf bytes.Buffer

	// summary is the accumulator. nil until we see the banner
	// header — Result returns nil if the run never produced one.
	summary *Summary

	// finalized latches when we see the banner-end marker. After
	// that, additional Feed calls don't modify summary (the
	// ingestor may keep logging after the banner, e.g. shutdown
	// messages; those shouldn't perturb the result).
	finalized bool

	// insideBanner is true between bannerStartMarker and
	// bannerEndMarker. Outside this window, lines are ignored
	// (so e.g. a stray emoji in earlier log output doesn't
	// trigger spurious parsing).
	insideBanner bool

	// sawAnyField latches once we successfully parse a
	// fieldPatterns line (regardless of whether the count is
	// zero). Used by feedLine to distinguish the opening ═-rule
	// (no fields yet → ignore) from the closing one (fields
	// already parsed → finalize). The earlier "any field
	// non-zero" check failed on banners where every metric was
	// genuinely 0 (early failure case) — Bugbot caught this on
	// PR #10 round 2.
	sawAnyField bool
}

// NewSummaryParser returns an initialized parser. Caller's
// goroutine owns it for the duration of the watch loop.
func NewSummaryParser() *SummaryParser {
	return &SummaryParser{}
}

// Feed accepts arbitrary log bytes; the parser buffers and splits
// internally. Safe to call with partial lines, multiple lines, or
// empty input.
func (p *SummaryParser) Feed(b []byte) {
	if p.finalized {
		return
	}
	_, _ = p.buf.Write(b)
	for {
		idx := bytes.IndexByte(p.buf.Bytes(), '\n')
		if idx < 0 {
			// No complete line yet — wait for more input.
			return
		}
		line := p.buf.Next(idx + 1) // consume up to and including '\n'
		p.feedLine(string(bytes.TrimRight(line, "\n")))
	}
}

// FlushLine forces parsing of any buffered partial-line content.
// Called at end-of-stream by the watch loop in case the Pod
// terminated without a final '\n' (rare but possible if the
// container's stdout was killed mid-write).
func (p *SummaryParser) FlushLine() {
	if p.buf.Len() > 0 && !p.finalized {
		p.feedLine(p.buf.String())
		p.buf.Reset()
	}
}

// feedLine parses a single line, ANSI-stripped. The state machine
// has three regions:
//
//   - Pre-banner: skip until we see the start marker
//   - Inside banner: match each line against fieldPatterns + the
//     Ingestor ID line
//   - End marker: latch and stop processing
func (p *SummaryParser) feedLine(rawLine string) {
	line := stripANSI(rawLine)
	if strings.Contains(line, bannerStartMarker) {
		p.summary = &Summary{}
		p.insideBanner = true
		return
	}
	if !p.insideBanner {
		return
	}
	// Banner-end check: a long row of '═'. Only count when we've
	// already crossed the start marker (the start banner ALSO
	// has a '═' rule before the header, which we want to ignore).
	if strings.Contains(line, bannerEndMarker) {
		// Two ═-rules in the banner: one immediately after the
		// header, one at the very end. We use a simple counter
		// to distinguish — first one we see while insideBanner
		// is the post-header rule (skip), second is the
		// post-metrics rule (finalize).
		//
		// Actually the simpler approach: check whether we've
		// parsed any field yet. If so, this ═ is the closing
		// rule; if not, it's the opening one. The ingestor
		// always prints fields between the two rules.
		if p.summary != nil && p.hasParsedAnyField() {
			p.finalized = true
		}
		return
	}

	// Ingestor ID line is the first content line in the banner.
	if m := ingestorIDRE.FindStringSubmatch(line); m != nil {
		p.summary.IngestorID = strings.TrimSpace(m[1])
		return
	}

	// Otherwise: try each field pattern. The prefix match is
	// linear over a 7-element slice — microscopic overhead per
	// line, and the fixed order matches the ingestor's print
	// order.
	for _, fp := range fieldPatterns {
		if !strings.Contains(line, fp.prefix) {
			continue
		}
		m := numberRE.FindStringSubmatch(line)
		if m == nil {
			return
		}
		// Strip thousands-separator commas.
		raw := strings.ReplaceAll(m[1], ",", "")
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return // malformed; ignore this line
		}
		fp.apply(p.summary, n)
		// Latch regardless of value. An all-zero banner is a
		// real shape (early failure: the ingestor still prints
		// the summary structure with all counts at 0). Bugbot
		// PR #10 r2 flagged the "non-zero" hasParsedAnyField
		// check as a finalization hole for this case.
		p.sawAnyField = true
		return
	}
}

// hasParsedAnyField reports whether the parser has seen and
// applied at least one fieldPatterns line. Used to disambiguate
// the two ═-rules in the banner (see feedLine).
func (p *SummaryParser) hasParsedAnyField() bool {
	return p.sawAnyField
}

// Result returns the accumulated Summary. nil if the parser never
// saw a banner header (early failure, OOM before the ingestor got
// to print its results). Safe to call at any point — the parser
// returns the in-progress accumulator if not yet finalized.
func (p *SummaryParser) Result() *Summary {
	return p.summary
}

// RenderSummary prints the installer-style ingestion summary through
// p: an outcome-colored headline (green when clean, yellow on skips,
// red on failures), the per-stage counts as Section/Field rows, and a
// short "what's next". It replaces the old box-drawing panel —
// Section/Field is plain-ASCII friendly, so no Unicode-box fallback is
// needed (that was the v0.2 TODO).
//
// No-op on a nil summary: an early failure (OOM before the ingestor
// printed its banner) produces no Summary, so the orchestrator can
// call this unconditionally.
func RenderSummary(p *ui.Printer, s *Summary) {
	if s == nil {
		return
	}
	headline := fmt.Sprintf("ingested %s of %s records (%.1f%%)",
		commaSep(s.InsertedRecords), commaSep(s.TotalRecords), s.SuccessRate())
	switch {
	case s.HasFailures():
		p.Errorf("Ingestion completed with failures — %s", headline)
	case s.SkippedRecords > 0:
		p.Warnf("Ingestion completed with skips — %s", headline)
	default:
		p.Successf("Ingestion complete — %s", headline)
	}

	p.Section("Ingestion summary")
	if s.IngestorID != "" {
		p.Field("ingestor ID", s.IngestorID)
	}
	p.Field("total records", commaSep(s.TotalRecords))
	p.Field("inserted", commaSep(s.InsertedRecords))
	p.Field("sent to API", commaSep(s.APISentRecords))
	p.Field("skipped", commaSep(s.SkippedRecords))
	p.Field("file failures", commaSep(s.FileTransferFailures))
	p.Field("DB failures", commaSep(s.FailedRecords))
	p.Field("success rate", fmt.Sprintf("%.1f%%", s.SuccessRate()))

	p.Section("What's next")
	p.Infof("View it in the dashboard: https://ai.tracebloc.io/metadata")
	p.Hintf("The table is staged and ready for training jobs.")
}

// commaSep formats an int64 with thousands-separator commas to
// match the ingestor's own banner format. Pure Go, no x/text.
func commaSep(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	// Insert commas every 3 digits from the right. Handle the
	// optional leading '-' by carving it off first.
	neg := ""
	if s[0] == '-' {
		neg = "-"
		s = s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return neg + string(out)
}

// (compile-time test: bufio + io are referenced via Feed's buffer)
var _ = bufio.ScanLines
var _ io.Writer = parserWriter{} // ensures parserWriter satisfies io.Writer
