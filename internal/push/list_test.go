package push

import (
	"context"
	"errors"
	"reflect"
	"strings"
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

// TestListDatasetsWith covers the exec + error-wrap path of ListDatasets, which
// was 0% because the production ListDatasets builds a real SPDYExecutor. The
// split-out core takes an Executor, so a fake drives it: a happy query result is
// parsed into table names, an empty database yields no datasets, and a failing
// exec surfaces "querying datasets" plus the remote stderr (via stderrSuffix).
func TestListDatasetsWith(t *testing.T) {
	t.Run("query result parsed into table names", func(t *testing.T) {
		fe := &fakeExecutor{stdoutToReturn: []byte("cats_dogs\nchurn\n")}
		got, err := listDatasetsWith(context.Background(), fe, "tracebloc", "mysql-0", "mysql")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"cats_dogs", "churn"}) {
			t.Fatalf("got %#v, want [cats_dogs churn]", got)
		}
		if len(fe.gotCmd) != 3 || fe.gotCmd[0] != "sh" || !strings.Contains(fe.gotCmd[2], "information_schema") {
			t.Errorf("unexpected exec cmd: %#v", fe.gotCmd)
		}
	})
	t.Run("empty database yields no datasets", func(t *testing.T) {
		fe := &fakeExecutor{stdoutToReturn: []byte("")}
		got, err := listDatasetsWith(context.Background(), fe, "tracebloc", "mysql-0", "mysql")
		if err != nil || got != nil {
			t.Fatalf("got (%#v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("exec failure surfaces the query + remote stderr", func(t *testing.T) {
		fe := &fakeExecutor{
			errToReturn:    errors.New("command terminated with exit code 1"),
			stderrToReturn: []byte("ERROR 1045: Access denied"),
		}
		_, err := listDatasetsWith(context.Background(), fe, "tracebloc", "mysql-0", "mysql")
		if err == nil {
			t.Fatal("want an error when the exec fails")
		}
		for _, want := range []string{"querying datasets", "Access denied"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q must contain %q", err.Error(), want)
			}
		}
	})
}
