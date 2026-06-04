package push

import (
	"reflect"
	"testing"
)

// TestParseDatasetList pins the raw `mysql -N` output → []string
// parsing: real names kept in order, blank/whitespace lines and a
// trailing newline dropped, and empty input → nil (no datasets).
func TestParseDatasetList(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "\n  \n\t\n", nil},
		{"one", "cats_dogs_train\n", []string{"cats_dogs_train"}},
		{"several with trailing newline", "a\nb\nc\n", []string{"a", "b", "c"}},
		{"surrounding whitespace", "  reg_train \n\tchurn_test\t\n", []string{"reg_train", "churn_test"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseDatasetList(c.raw); !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseDatasetList(%q) = %#v, want %#v", c.raw, got, c.want)
			}
		})
	}
}
