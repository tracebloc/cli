package push

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckColumnValueTypes covers the DataValidator numeric per-value preview
// (cli#352). The through-line is the parity contract's cardinal rule: reject a
// value the cluster would reject (non-numeric / fractional in a numeric column),
// but NEVER over-reject — valid numbers, NA/missing cells, numeric-looking
// strings, and the deliberately-unmirrored types (string/date/boolean) all pass.
func TestCheckColumnValueTypes(t *testing.T) {
	cases := []struct {
		name     string
		csv      string
		schema   map[string]string
		category string
		wantErr  bool
		wantSub  string
	}{
		// --- rejects: values that don't match a declared numeric type ---
		{
			name:     "int non-numeric rejected",
			csv:      "age,label\nabc,x\n40,y\n",
			schema:   map[string]string{"age": "INT", "label": "VARCHAR(255)"},
			category: "tabular_classification",
			wantErr:  true, wantSub: `"age"`,
		},
		{
			name:     "int fractional rejected",
			csv:      "age\n30\n40.5\n",
			schema:   map[string]string{"age": "INT"},
			category: "tabular_classification",
			wantErr:  true, wantSub: "whole numbers",
		},
		{
			name:     "bigint non-numeric rejected",
			csv:      "n\n5\nNaNaN\n",
			schema:   map[string]string{"n": "BIGINT"},
			category: "tabular_classification",
			wantErr:  true, wantSub: `"n"`,
		},
		{
			name:     "float non-numeric rejected",
			csv:      "price\n9.99\nxyz\n",
			schema:   map[string]string{"price": "FLOAT"},
			category: "tabular_regression",
			wantErr:  true, wantSub: `"price"`,
		},

		// --- accepts: never over-reject ---
		{
			name:     "int 1.0 accepted (Excel-style whole float)",
			csv:      "age\n1\n1.0\n2\n",
			schema:   map[string]string{"age": "INT"},
			category: "tabular_classification",
			wantErr:  false,
		},
		{
			name:     "int NA and empty accepted (missing, stored NULL)",
			csv:      "age,x\n30,a\n,b\nNA,c\nnull,d\n",
			schema:   map[string]string{"age": "INT", "x": "VARCHAR(255)"},
			category: "tabular_classification",
			wantErr:  false,
		},
		{
			name:     "float scientific + padded + signed accepted",
			csv:      "price\n1e3\n 2.5 \n-0.1\n",
			schema:   map[string]string{"price": "FLOAT"},
			category: "tabular_regression",
			wantErr:  false,
		},
		{
			name:     "float allows fractional (not integer-checked)",
			csv:      "price\n1.5\n2.75\n",
			schema:   map[string]string{"price": "DOUBLE"},
			category: "tabular_regression",
			wantErr:  false,
		},
		{
			name:     "numeric-looking value in numeric column accepted (#188 direction)",
			csv:      "zip\n01000\n90210\n",
			schema:   map[string]string{"zip": "INT"},
			category: "tabular_classification",
			wantErr:  false,
		},

		// --- deliberately unmirrored types (documented under-preview): accept ---
		{
			name:     "varchar not per-value checked",
			csv:      "code\nabc\n123xyz!!\n",
			schema:   map[string]string{"code": "VARCHAR(3)"},
			category: "tabular_classification",
			wantErr:  false,
		},
		{
			name:     "date not per-value checked",
			csv:      "d\nnot-a-date\n2024-13-99\n",
			schema:   map[string]string{"d": "DATE"},
			category: "tabular_classification",
			wantErr:  false,
		},
		{
			name:     "boolean not per-value checked",
			csv:      "b\nmaybe\nsometimes\n",
			schema:   map[string]string{"b": "BOOLEAN"},
			category: "tabular_classification",
			wantErr:  false,
		},

		// --- scope / resolution edges ---
		{
			name:     "empty schema is a no-op",
			csv:      "age\nabc\n",
			schema:   map[string]string{},
			category: "tabular_classification",
			wantErr:  false,
		},
		{
			name:     "schema column absent from CSV is not this check's diagnostic",
			csv:      "other\n1\n",
			schema:   map[string]string{"age": "INT"},
			category: "tabular_classification",
			wantErr:  false,
		},
		{
			name:     "case-sensitive column match (Age != age)",
			csv:      "Age\nabc\n",
			schema:   map[string]string{"age": "INT"},
			category: "tabular_classification",
			wantErr:  false, // schema 'age' doesn't match header 'Age' — CheckSchemaColumns owns that
		},
		{
			name:     "whitespace-stripped header match",
			csv:      " age \nabc\n",
			schema:   map[string]string{"age": "INT"},
			category: "tabular_classification",
			wantErr:  true, wantSub: `"age"`,
		},
		{
			name:     "TSC grouping time column excluded (fractional step accepted)",
			csv:      "sequence_id,timestamp,hr\np1,1.5,80\np1,2.5,84\n",
			schema:   map[string]string{"sequence_id": "VARCHAR(64)", "timestamp": "INT", "hr": "FLOAT"},
			category: "time_series_classification",
			wantErr:  false,
		},
		{
			name:     "TSC feature column still checked",
			csv:      "sequence_id,timestamp,hr\np1,1,notnum\np1,2,84\n",
			schema:   map[string]string{"sequence_id": "VARCHAR(64)", "timestamp": "INT", "hr": "FLOAT"},
			category: "time_series_classification",
			wantErr:  true, wantSub: `"hr"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "data.csv")
			if err := os.WriteFile(p, []byte(tc.csv), 0o644); err != nil {
				t.Fatal(err)
			}
			err := CheckColumnValueTypes(p, tc.schema, tc.category)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("expected a rejection, got nil")
			case !tc.wantErr && err != nil:
				t.Fatalf("expected accept, got: %v", err)
			case tc.wantErr && tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub):
				t.Errorf("error missing %q: %v", tc.wantSub, err)
			}
		})
	}
}

// TestNumericCellBad pins the cell-level contract directly, including the safe
// (under-reject) handling of inf/nan and NA sentinels.
func TestNumericCellBad(t *testing.T) {
	tests := []struct {
		raw     string
		integer bool
		bad     bool
	}{
		{"42", true, false},
		{"42", false, false},
		{"1.0", true, false},   // whole float in INT — Excel writes these
		{"1.5", true, true},    // fractional in INT
		{"1.5", false, false},  // fractional in FLOAT is fine
		{"abc", true, true},    // non-numeric
		{"abc", false, true},   // non-numeric
		{"", true, false},      // empty = missing
		{"NA", true, false},    // NA sentinel = missing
		{"null", false, false}, // NA sentinel = missing
		{"  ", true, false},    // whitespace-only = missing (under-reject, safe)
		{" 7 ", true, false},   // padded number parses after trim
		{"1e3", false, false},  // scientific notation
		{"inf", true, false},   // left to the ingestor's non-finite handling
		{"inf", false, false},
		{"-5", true, false},
	}
	for _, tc := range tests {
		if got := numericCellBad(tc.raw, tc.integer); got != tc.bad {
			t.Errorf("numericCellBad(%q, integer=%v) = %v, want %v", tc.raw, tc.integer, got, tc.bad)
		}
	}
}
