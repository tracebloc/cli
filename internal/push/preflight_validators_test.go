package push

import (
	"fmt"
	"strings"
	"testing"
)

// These tests pin preflight validators at the exact boundaries mutation testing
// (gremlins) found unguarded. All are pure (in-memory slices/maps), no fixtures.
// Each case is chosen to KILL a specific surviving mutant, noted inline.

func tsc(seq, label, tm string, row int) tscRow {
	return tscRow{seq: seq, label: label, time: tm, row: row}
}

// TestCheckLabelColumn_FirstColumnBoundary kills the `matchColumnIndex(...) >= 0`
// → `> 0` mutant (CheckLabelColumn:154): a label column that is the FIRST header
// column (index 0) must still be accepted — `> 0` would reject it.
func TestCheckLabelColumn_FirstColumnBoundary(t *testing.T) {
	// label at index 0 — the boundary the mutant breaks.
	if err := CheckLabelColumn([]string{"label", "feat1", "feat2"}, "label", "d.csv"); err != nil {
		t.Errorf("label in the first column must be accepted, got: %v", err)
	}
	// case-insensitive / trimmed match (also index 0) still accepted.
	if err := CheckLabelColumn([]string{" LABEL ", "feat"}, "label", "d.csv"); err != nil {
		t.Errorf("case-insensitive first-column match must be accepted, got: %v", err)
	}
	// genuinely absent → a clear error naming the columns + the flag.
	err := CheckLabelColumn([]string{"feat1", "feat2"}, "target", "d.csv")
	if err == nil {
		t.Fatal("absent label column must error")
	}
	for _, want := range []string{"target", "d.csv", "--label-column"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestCheckAnnotationPairing_AsymmetricMismatch kills the `len(noAnn) > 0` /
// `len(noImg) > 0` → `>= 0` mutants (CheckAnnotationPairing:425/429): when only
// ONE side is orphaned, the message must mention only that side — `>= 0` would
// emit a bogus "0 X without a Y ()" clause for the empty side.
func TestCheckAnnotationPairing_AsymmetricMismatch(t *testing.T) {
	// Every image is paired; one annotation is orphaned → noAnn empty, noImg = {c}.
	err := CheckAnnotationPairing(
		[]string{"a.jpg", "b.jpg"},
		[]string{"a.xml", "b.xml", "c.xml"},
	)
	if err == nil {
		t.Fatal("an orphaned annotation must be reported")
	}
	if !strings.Contains(err.Error(), "annotation(s) without an image") {
		t.Errorf("should report the orphaned annotation, got: %v", err)
	}
	if strings.Contains(err.Error(), "image(s) without an annotation") {
		t.Errorf("must NOT emit the empty image-side clause (>=0 mutant): %v", err)
	}

	// Mirror: an orphaned image, all annotations paired.
	err = CheckAnnotationPairing([]string{"a.jpg", "d.jpg"}, []string{"a.xml"})
	if err == nil || !strings.Contains(err.Error(), "image(s) without an annotation") {
		t.Fatalf("orphaned image must be reported, got: %v", err)
	}
	if strings.Contains(err.Error(), "annotation(s) without an image") {
		t.Errorf("must NOT emit the empty annotation-side clause: %v", err)
	}

	// Fully paired → nil.
	if err := CheckAnnotationPairing([]string{"a.jpg"}, []string{"a.xml"}); err != nil {
		t.Errorf("paired sets must pass, got: %v", err)
	}
}

// TestLabelConstantViolation_Direct pins the constant-label-per-sequence rule and
// the "+N more" truncation boundary (labelConstantViolation:1062, `len > 5`).
func TestLabelConstantViolation_Direct(t *testing.T) {
	const g, l = "seq", "label"

	// A sequence whose label stays constant → nil.
	ok := []tscRow{tsc("s1", "A", "", 1), tsc("s1", "A", "", 2), tsc("s2", "B", "", 3)}
	if err := labelConstantViolation(ok, true, true, g, l); err != nil {
		t.Errorf("constant labels per sequence must pass, got: %v", err)
	}

	// A sequence whose label changes mid-sequence → error naming the row.
	bad := []tscRow{tsc("s1", "A", "", 1), tsc("s1", "B", "", 2)}
	if err := labelConstantViolation(bad, true, true, g, l); err == nil {
		t.Fatal("a label change mid-sequence must be rejected")
	}

	// Benign skip when either column is absent.
	if err := labelConstantViolation(bad, false, true, g, l); err != nil {
		t.Errorf("absent group column must benign-skip, got: %v", err)
	}

	// Truncation boundary: build N offending sequences (each 2 rows, A then B).
	offenders := func(n int) []tscRow {
		var rows []tscRow
		r := 1
		for i := 0; i < n; i++ {
			s := fmt.Sprintf("s%d", i)
			rows = append(rows, tsc(s, "A", "", r), tsc(s, "B", "", r+1))
			r += 2
		}
		return rows
	}
	// Exactly 5 offenders → all shown, NO "(+N more)".
	err5 := labelConstantViolation(offenders(5), true, true, g, l)
	if err5 == nil || strings.Contains(err5.Error(), "more") {
		t.Errorf("exactly 5 offenders must list all with no '(+N more)', got: %v", err5)
	}
	// 6 offenders → truncated with "(+1 more)".
	err6 := labelConstantViolation(offenders(6), true, true, g, l)
	if err6 == nil || !strings.Contains(err6.Error(), "(+1 more)") {
		t.Errorf("6 offenders must show '(+1 more)', got: %v", err6)
	}
}

// TestPerGroupTimeViolation_Direct pins the numeric-time ordering rule plus both
// "+N more" truncation boundaries (perGroupTimeViolation:1115 invalid-values,
// :1165 out-of-order).
func TestPerGroupTimeViolation_Direct(t *testing.T) {
	const g, tcol = "seq", "t"
	numeric := map[string]string{tcol: "INT"}

	// Monotonic non-decreasing within each sequence → nil.
	ok := []tscRow{tsc("s1", "", "1", 1), tsc("s1", "", "1", 2), tsc("s1", "", "2", 3)}
	if err := perGroupTimeViolation(ok, true, true, g, tcol, numeric); err != nil {
		t.Errorf("monotonic per-sequence time must pass, got: %v", err)
	}

	// A strict decrease within a sequence → out-of-order error.
	dec := []tscRow{tsc("s1", "", "2", 1), tsc("s1", "", "1", 2)}
	if err := perGroupTimeViolation(dec, true, true, g, tcol, numeric); err == nil || !strings.Contains(err.Error(), "out-of-order") {
		t.Fatalf("a decreasing time must be flagged out-of-order, got: %v", err)
	}

	// Non-numeric declared time column → documented under-preview skip (nil).
	ts := map[string]string{tcol: "TIMESTAMP"}
	if err := perGroupTimeViolation(dec, true, true, g, tcol, ts); err != nil {
		t.Errorf("a TIMESTAMP-typed time column must benign-skip, got: %v", err)
	}

	// Invalid-value truncation boundary: exactly 5 invalid cells → no "(+N more)".
	invalid := func(n int) []tscRow {
		var rows []tscRow
		for i := 0; i < n; i++ {
			rows = append(rows, tsc("s1", "", "notnum", i+1))
		}
		return rows
	}
	e5 := perGroupTimeViolation(invalid(5), true, true, g, tcol, numeric)
	if e5 == nil || !strings.Contains(e5.Error(), "invalid") || strings.Contains(e5.Error(), "more") {
		t.Errorf("5 invalid time values must list all with no '(+N more)', got: %v", e5)
	}
	e6 := perGroupTimeViolation(invalid(6), true, true, g, tcol, numeric)
	if e6 == nil || !strings.Contains(e6.Error(), "(+1 more)") {
		t.Errorf("6 invalid time values must show '(+1 more)', got: %v", e6)
	}

	// Out-of-order truncation boundary: 5 sequences each with a decrease.
	decSeqs := func(n int) []tscRow {
		var rows []tscRow
		r := 1
		for i := 0; i < n; i++ {
			s := fmt.Sprintf("s%d", i)
			rows = append(rows, tsc(s, "", "2", r), tsc(s, "", "1", r+1))
			r += 2
		}
		return rows
	}
	o5 := perGroupTimeViolation(decSeqs(5), true, true, g, tcol, numeric)
	if o5 == nil || strings.Contains(o5.Error(), "more") {
		t.Errorf("5 out-of-order sequences must list all with no '(+N more)', got: %v", o5)
	}
	o6 := perGroupTimeViolation(decSeqs(6), true, true, g, tcol, numeric)
	if o6 == nil || !strings.Contains(o6.Error(), "(+1 more)") {
		t.Errorf("6 out-of-order sequences must show '(+1 more)', got: %v", o6)
	}
}

// TestIsNumericTimeColumn_Direct pins the numeric-vs-temporal branch selector:
// integer/float/decimal families are numeric (parsed as a step index), while
// VARCHAR / TIMESTAMP / an absent column are not. Also covers the length-suffix
// and " UNSIGNED"-modifier normalization.
func TestIsNumericTimeColumn_Direct(t *testing.T) {
	cases := []struct {
		typ  string
		want bool
	}{
		{"INT", true},
		{"BIGINT", true},
		{"FLOAT", true},
		{"DECIMAL(10,2)", true}, // length suffix dropped
		{"int unsigned", true},  // lower-case + " UNSIGNED" modifier → INT
		{"VARCHAR(255)", false}, // text
		{"TIMESTAMP", false},    // temporal branch (under-preview)
		{"DATETIME", false},
	}
	for _, c := range cases {
		got := isNumericTimeColumn(map[string]string{"t": c.typ}, "t")
		if got != c.want {
			t.Errorf("isNumericTimeColumn(%q) = %v, want %v", c.typ, got, c.want)
		}
	}
	// An absent time column resolves to the non-numeric (temporal) branch.
	if isNumericTimeColumn(map[string]string{"other": "INT"}, "t") {
		t.Error("an absent time column must not be numeric")
	}
}
