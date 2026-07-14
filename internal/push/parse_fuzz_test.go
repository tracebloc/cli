package push

import "testing"

// The CLI had zero fuzz targets (coverage audit). These fuzz the user-facing
// dimension parsers, which turn arbitrary --target-size / --min-size flag
// values into a (width, height) pair. Two invariants must hold for ANY input —
// and the fuzzer also proves neither parser panics:
//   - success  => both dimensions are strictly positive;
//   - error    => both dimensions are zero (no partial/garbage pair leaks out).
// Under plain `go test` the seed corpus runs as ordinary cases; `go test
// -fuzz=FuzzParseTargetSize` explores beyond it.

var wxhSeeds = []string{
	"512x512", "640x480", "1x1", "512,512", " 512 x 512 ",
	"", "512", "512x", "x512", "0x512", "-4x512", "512x0",
	"512x512x512", "axb", "  ", ",", "x", "99999999999999999999x1",
	"1x-1", "0,0", "12.5x12.5", "\t8x8\n",
}

func fuzzWxH(f *testing.F, parse func(string) (int, int, error)) {
	for _, s := range wxhSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		w, h, err := parse(s)
		if err == nil {
			if w <= 0 || h <= 0 {
				t.Errorf("parse(%q) succeeded with non-positive dims (%d, %d)", s, w, h)
			}
			return
		}
		if w != 0 || h != 0 {
			t.Errorf("parse(%q) errored but leaked dims (%d, %d) instead of (0, 0)", s, w, h)
		}
	})
}

func FuzzParseTargetSize(f *testing.F) { fuzzWxH(f, ParseTargetSize) }

func FuzzParseMinSize(f *testing.F) { fuzzWxH(f, ParseMinSize) }
