package push

import (
	"strconv"
	"strings"
	"testing"
)

// These tests pin the pure SQL type-inference / schema helpers at the exact
// boundaries mutation testing (gremlins) found unguarded — the schema is what
// the ingestor keys column dtypes on, so an off-by-one here silently mis-types a
// column (e.g. INT vs BIGINT overflow, or a malformed type token slipping
// through). Each case is chosen to KILL a specific surviving mutant, noted inline.

// TestIntegerType_Int32Boundary pins the INT-vs-BIGINT width boundary
// (integerType: `v < int32Min || v > int32Max`). A value of exactly int32Max /
// int32Min must stay INT; one beyond must widen to BIGINT. Kills the two
// CONDITIONALS_BOUNDARY mutants (`>`→`>=`, `<`→`<=`) that flipped the boundary
// value's width.
func TestIntegerType_Int32Boundary(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"int32 max stays INT", strconv.FormatInt(int32Max, 10), "INT"},
		{"int32 min stays INT", strconv.FormatInt(int32Min, 10), "INT"},
		{"one past int32 max widens", strconv.FormatInt(int32Max+1, 10), "BIGINT"},
		{"one below int32 min widens", strconv.FormatInt(int32Min-1, 10), "BIGINT"},
		{"zero is INT", "0", "INT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := integerType([]string{c.token}); got != c.want {
				t.Errorf("integerType([%q]) = %q, want %q", c.token, got, c.want)
			}
		})
	}
	// Beyond int64 is not storable as an integer → falls back to VARCHAR.
	if got := integerType([]string{"99999999999999999999"}); !strings.HasPrefix(got, "VARCHAR") {
		t.Errorf("integerType(beyond int64) = %q, want VARCHAR(...)", got)
	}
}

// TestSqlBaseType pins the base-type extraction, including the no-leading-
// identifier contract: a token that is ONLY a "(...)" suffix has base "" (kills
// the `i >= 0`→`i > 0` boundary mutant, which would return "(255)" instead).
func TestSqlBaseType(t *testing.T) {
	cases := map[string]string{
		"VARCHAR(255)":     "VARCHAR",
		"  int unsigned  ": "INT",     // upper + first whitespace token
		"DECIMAL(10,2)":    "DECIMAL", // "(...)" suffix dropped
		"TEXT":             "TEXT",
		"(255)":            "", // no leading identifier → empty (boundary killer)
		"":                 "",
		"   ":              "",
	}
	for in, want := range cases {
		if got := sqlBaseType(in); got != want {
			t.Errorf("sqlBaseType(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSplitSchemaEntries pins the entry split, including the unbalanced-paren
// robustness: a stray ')' must not drive paren depth negative (kills the
// `depth > 0`→`depth >= 0` mutant, which would then swallow the following
// separator comma and merge two entries into one).
func TestSplitSchemaEntries(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a:INT,b:VARCHAR(5)", []string{"a:INT", "b:VARCHAR(5)"}},
		{"a:DECIMAL(10,2),b:INT", []string{"a:DECIMAL(10,2)", "b:INT"}}, // nested comma kept
		{"a:INT", []string{"a:INT"}},
		{"a),b", []string{"a)", "b"}}, // stray ')' — must stay 2 entries (depth floor)
	}
	for _, c := range cases {
		got := splitSchemaEntries(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitSchemaEntries(%q) = %v (%d entries), want %v (%d)", c.in, got, len(got), c.want, len(c.want))
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitSchemaEntries(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// TestContainsASCIIDigit pins both ends of the digit range: '0' and '9' must
// count as digits (kills the `>= '0'`→`> '0'` and `<= '9'`→`< '9'` boundary
// mutants), while the chars just outside ('/' = '0'-1, ':' = '9'+1) must not.
func TestContainsASCIIDigit(t *testing.T) {
	cases := map[string]bool{
		"0":       true, // lower boundary
		"9":       true, // upper boundary
		"5":       true,
		"abc7def": true,
		"abc":     false,
		"":        false,
		"/":       false, // ASCII 47, just below '0'
		":":       false, // ASCII 58, just above '9'
	}
	for in, want := range cases {
		if got := containsASCIIDigit(in); got != want {
			t.Errorf("containsASCIIDigit(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestIsIntegerType pins the id-like integer check — kills the two
// CONDITIONALS_NEGATION mutants on `typ == "INT" || typ == "BIGINT"`.
func TestIsIntegerType(t *testing.T) {
	cases := map[string]bool{
		"INT":        true,
		"BIGINT":     true,
		"VARCHAR(5)": false,
		"FLOAT":      false,
		"DATETIME":   false,
		"":           false,
	}
	for typ, want := range cases {
		if got := isIntegerType(typ); got != want {
			t.Errorf("isIntegerType(%q) = %v, want %v", typ, got, want)
		}
	}
}
