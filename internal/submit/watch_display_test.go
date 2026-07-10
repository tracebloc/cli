package submit

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// errAfterReader yields its data, then returns err on the next Read — a genuine
// mid-stream read failure (network drop / ctx cancel), distinct from io.EOF.
type errAfterReader struct {
	data []byte
	err  error
	pos  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// TestStreamDisplayAndParse_DrainsPastOversizedLineSoParserSeesBanner pins the
// #208-review fix: an over-long tqdm-style progress "line" (many '\r' redraws,
// no '\n') that outgrows the display buffer, immediately followed by the real
// closing banner. Because the tee is pulled ONLY by the display scanner, a
// naive ErrTooLong bail would stop the parser from ever seeing the banner →
// watch returns a false exit 9 on a healthy run. The drain-past-ErrTooLong must
// keep pulling so the parser still resolves the summary.
func TestStreamDisplayAndParse_DrainsPastOversizedLineSoParserSeesBanner(t *testing.T) {
	oversized := strings.Repeat("\rprocessing... ", 500) // ~7 KB, no '\n'
	stream := strings.NewReader(oversized + realIngestorBanner)
	parser := NewSummaryParser()
	tee := io.TeeReader(stream, parserWriter{parser: parser})

	var out bytes.Buffer
	// Tiny cap so the oversized line trips ErrTooLong (production uses 16 MB).
	if err := streamDisplayAndParse(tee, &out, 1024); err != nil {
		t.Fatalf("an over-long DISPLAY line must not be fatal; got: %v", err)
	}
	parser.FlushLine()
	if got := parser.Result().InsertedRecords; got != 1200 {
		t.Fatalf(
			"parser missed the banner after the oversized line (false exit 9): "+
				"InsertedRecords=%d, want 1200",
			got,
		)
	}
}

// TestStreamDisplayAndParse_GenuineReadErrorPropagates: a real mid-stream read
// failure (not ErrTooLong, not EOF) stays fatal — the drain path must not
// swallow genuine stream errors.
func TestStreamDisplayAndParse_GenuineReadErrorPropagates(t *testing.T) {
	want := errors.New("connection reset by peer")
	r := &errAfterReader{data: []byte("some log line\n"), err: want}

	var out bytes.Buffer
	if err := streamDisplayAndParse(r, &out, 1024); !errors.Is(err, want) {
		t.Fatalf("a genuine read error should propagate; got: %v", err)
	}
}
