package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/submit"
	"github.com/tracebloc/cli/internal/ui"
)

// TestPrintPushPreflight_RendersKeyFacts pins that the pre-flight
// summary surfaces the facts a customer sanity-checks before a push:
// the target release, the shared PVC, and the synthesized spec
// identity. It's the customer's last look before bytes move, so the
// content (not just "it didn't panic") is worth asserting.
func TestPrintPushPreflight_RendersKeyFacts(t *testing.T) {
	layout := &push.LocalLayout{
		Root:       "/tmp/cats_dogs",
		LabelsCSV:  "/tmp/cats_dogs/labels.csv",
		Images:     []string{"a.jpg", "b.jpg", "c.jpg"},
		TotalBytes: 1024,
	}
	release := &cluster.ParentRelease{
		ReleaseName:        "ingdemo",
		ChartVersion:       "1.4.2",
		JobsManagerService: "http://jobs-manager.ingdemo.svc.cluster.local:8080",
	}
	pvc := &cluster.SharedPVC{
		ClaimName:   "client-pvc",
		MountPath:   "/data/shared",
		Phase:       corev1.ClaimBound,
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
	}
	spec := map[string]any{
		"table":    "cats_dogs_train",
		"category": "image_classification",
		"intent":   "train",
		"label":    "label",
	}

	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	printPushPreflight(p, layout, release, pvc, spec, false)
	out := buf.String()

	for _, want := range []string{
		"ingdemo", "1.4.2", "client-pvc",
		"cats_dogs_train", "image_classification", "train",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pre-flight output missing %q:\n%s", want, out)
		}
	}
}

// TestWritePushJSON checks the --output-json result serializes to
// valid JSON with the expected fields.
func TestWritePushJSON(t *testing.T) {
	spec := map[string]any{"table": "reg_train", "category": "tabular_regression", "intent": "train"}
	s := &submit.Summary{IngestorID: "run-1", TotalRecords: 240, InsertedRecords: 240, APISentRecords: 240}

	var buf bytes.Buffer
	writePushJSON(&buf, "succeeded", spec, s, "ns1", "ingest-job-x")

	var got pushJSONResult
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if got.Status != "succeeded" || got.Table != "reg_train" || got.JobName != "ingest-job-x" {
		t.Errorf("unexpected result: %+v", got)
	}
	if got.Summary == nil || got.Summary.InsertedRecords != 240 {
		t.Errorf("summary missing/wrong: %+v", got.Summary)
	}
}

// TestClassifyPushOutcome pins the --output-json status ↔ exit-code
// contract (Bugbot #38): the status must agree with the exit code on
// every path — a partial-failure must NOT report "succeeded", and a
// watch error must still classify (so JSON gets emitted). wantCode 0
// means no exitError (success).
func TestClassifyPushOutcome(t *testing.T) {
	resp := &submit.SubmitResponse{Namespace: "ns1", JobName: "ingest-job-x"}
	cases := []struct {
		name     string
		res      *submit.Result
		err      error
		wantStat string
		wantCode int
	}{
		{"clean", &submit.Result{Submit: resp, Watch: &submit.WatchResult{Outcome: submit.JobOutcomeSucceeded, Summary: &submit.Summary{TotalRecords: 10, InsertedRecords: 10}}}, nil, "succeeded", 0},
		{"partial", &submit.Result{Submit: resp, Watch: &submit.WatchResult{Outcome: submit.JobOutcomeSucceeded, Summary: &submit.Summary{TotalRecords: 10, InsertedRecords: 7, FailedRecords: 3}}}, nil, "completed_with_failures", 9},
		{"failed", &submit.Result{Submit: resp, Watch: &submit.WatchResult{Outcome: submit.JobOutcomeFailed}}, nil, "failed", 9},
		{"detached", &submit.Result{Submit: resp}, nil, "detached", 0},
		{"watch error", &submit.Result{Submit: resp}, &submit.WatchError{Err: errors.New("stream broke")}, "watch_error", 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStat, gotErr := classifyPushOutcome(c.res, c.err)
			if gotStat != c.wantStat {
				t.Errorf("status = %q, want %q", gotStat, c.wantStat)
			}
			code := 0
			if gotErr != nil {
				code = gotErr.Code()
			}
			if code != c.wantCode {
				t.Errorf("exit code = %d, want %d", code, c.wantCode)
			}
		})
	}
}

// TestExpandHome covers the #37 fix: a leading ~ / ~/… resolves under
// $HOME, while relative, absolute, and empty paths pass through
// untouched (the case that bit the interactive prompt — the shell
// never got a chance to expand the typed ~).
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	cases := []struct{ in, want string }{
		{"~", home},
		{"~/x", filepath.Join(home, "x")},
		{"~/tb-fixtures/tab-reg", filepath.Join(home, "tb-fixtures", "tab-reg")},
		{"./x", "./x"},
		{"/abs/path", "/abs/path"},
		{"relative/x", "relative/x"},
		{"", ""},
	}
	for _, c := range cases {
		if got := expandHome(c.in); got != c.want {
			t.Errorf("expandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExitError_Methods pins the exit-code carrier: Error() surfaces
// the wrapped message (or a fallback when nil), and Code() returns the
// process exit code main() propagates.
func TestExitError_Methods(t *testing.T) {
	e := &exitError{code: 7, err: errors.New("staging failed")}
	if e.Error() != "staging failed" {
		t.Errorf("Error() = %q, want %q", e.Error(), "staging failed")
	}
	if e.Code() != 7 {
		t.Errorf("Code() = %d, want 7", e.Code())
	}
	// err==nil: Error() falls back to a generic "exit N" string so the
	// type still satisfies error without panicking.
	nilErr := &exitError{code: 2}
	if !strings.Contains(nilErr.Error(), "2") {
		t.Errorf("Error() on nil-err exitError = %q, want it to mention the code", nilErr.Error())
	}
	if nilErr.Code() != 2 {
		t.Errorf("Code() = %d, want 2", nilErr.Code())
	}
}

// TestRunClusterInfo_BadKubeconfigExitsThree: an unreadable/invalid
// kubeconfig is exit-code-3 territory (the kubeconfig/local-input
// bucket), surfaced before any cluster work. Covers the Load-error
// branch of runClusterInfo without needing a real cluster.
func TestRunClusterInfo_BadKubeconfigExitsThree(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(bad, []byte("}{ this is not valid kubeconfig"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err := runClusterInfo(context.Background(), ui.New(&buf), bad, "", "", "", 600)
	if err == nil {
		t.Fatal("runClusterInfo with a broken kubeconfig returned nil; want an exitError")
	}
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("error is not an *exitError: %v", err)
	}
	if ee.Code() != 3 {
		t.Errorf("exit code = %d, want 3 (kubeconfig/local-input error)", ee.Code())
	}
}
