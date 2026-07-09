package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
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

	// Verbose so the cluster block renders too — it's --verbose-only now (see
	// TestPrintClusterSummary_VerboseOnly). The local summary shows regardless.
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false), ui.WithVerbose(true))
	printLocalSummary(p, layout, spec)
	printClusterSummary(p, release, pvc)
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

// TestPrintClusterSummary_VerboseOnly pins that the Kubernetes cluster detail
// (release / jobs-manager / shared PVC) is hidden on the default happy path and
// surfaces only under --verbose — the RFC-0002 §6 ceremony-hiding contract.
func TestPrintClusterSummary_VerboseOnly(t *testing.T) {
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

	// Default (non-verbose): none of the cluster plumbing leaks.
	var quiet bytes.Buffer
	printClusterSummary(ui.New(&quiet, ui.WithColor(false)), release, pvc)
	for _, hidden := range []string{"ingdemo", "1.4.2", "client-pvc", "jobs-manager", "Target cluster"} {
		if strings.Contains(quiet.String(), hidden) {
			t.Errorf("non-verbose output leaked cluster detail %q:\n%s", hidden, quiet.String())
		}
	}

	// --verbose: the same facts are shown.
	var loud bytes.Buffer
	printClusterSummary(ui.New(&loud, ui.WithColor(false), ui.WithVerbose(true)), release, pvc)
	for _, want := range []string{"ingdemo", "1.4.2", "client-pvc"} {
		if !strings.Contains(loud.String(), want) {
			t.Errorf("verbose output missing cluster detail %q:\n%s", want, loud.String())
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
		{"clean", &submit.Result{Submit: resp, Watch: &submit.WatchResult{Outcome: submit.JobOutcomeSucceeded, Summary: &submit.Summary{TotalRecords: 10, InsertedRecords: 10, APISentRecords: 10}}}, nil, "succeeded", 0},
		{"partial", &submit.Result{Submit: resp, Watch: &submit.WatchResult{Outcome: submit.JobOutcomeSucceeded, Summary: &submit.Summary{TotalRecords: 10, InsertedRecords: 7, FailedRecords: 3}}}, nil, "completed_with_failures", 9},
		{"failed", &submit.Result{Submit: resp, Watch: &submit.WatchResult{Outcome: submit.JobOutcomeFailed}}, nil, "failed", 9},
		{"unknown", &submit.Result{Submit: resp, Watch: &submit.WatchResult{Outcome: submit.JobOutcomeUnknown}}, nil, "unknown", 9},
		{"detached", &submit.Result{Submit: resp}, nil, "detached", 0},
		{"nil result", nil, nil, "detached", 0},
		{"watch error", &submit.Result{Submit: resp}, &submit.WatchError{Err: errors.New("stream broke")}, "watch_error", 9},
		// The submit-side error buckets (exit 5 vs 8) the original matrix missed.
		{"auth 401", nil, &submit.SubmitError{StatusCode: 401}, "auth_error", 5},
		{"auth 403", nil, &submit.SubmitError{StatusCode: 403}, "auth_error", 5},
		{"submit 500", nil, &submit.SubmitError{StatusCode: 500}, "submit_error", 8},
		{"submit 422", nil, &submit.SubmitError{StatusCode: 422}, "submit_error", 8},
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

// TestRunDatasetPush_OutputJSONEarlyFailureEmitsJSON: with --output-json,
// a failure before the dry-run/submit emit points (here an invalid table,
// exit 2) still writes a JSON error object to stdout — the stdout-always-
// JSON contract. (Bugbot #49)
func TestRunDatasetPush_OutputJSONEarlyFailureEmitsJSON(t *testing.T) {
	var jsonBuf, human bytes.Buffer
	a := runDataIngestArgs{
		// A real path so the failure is the invalid table name (exit 2), not
		// the earlier path-existence check (exit 3, #181) — this test pins the
		// stdout-always-JSON contract on the table-validation failure.
		LocalPath:  t.TempDir(),
		Spec:       push.SpecArgs{Table: "../bad", Category: "image_classification", Intent: "train"},
		Printer:    ui.New(&human, ui.WithColor(false)),
		OutputJSON: true,
		JSONOut:    &jsonBuf,
	}
	err := runDataIngest(context.Background(), &human, &human, a)

	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("err = %v, want *exitError code 2", err)
	}
	var got pushJSONResult
	if e := json.Unmarshal(jsonBuf.Bytes(), &got); e != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", e, jsonBuf.String())
	}
	if got.Status != "error" || got.ExitCode != 2 || got.Table != "../bad" {
		t.Errorf("got %+v, want status=error exit_code=2 table=../bad", got)
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

// TestExpandHome_NamedUser covers the #181 ~user form: "~user" and
// "~user/…" resolve under that user's home. We look up the CURRENT user by
// name so the test doesn't depend on a fixed account existing, and compare
// against os.UserHomeDir. An unknown ~user is left literal (the path-
// existence check surfaces it), which we also pin.
func TestExpandHome_NamedUser(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		t.Skipf("no current user: %v", err)
	}
	// user.Lookup must resolve the same account (it can differ from
	// UserHomeDir on some CI images); skip if it doesn't rather than assert
	// on an environment quirk.
	looked, err := user.Lookup(u.Username)
	if err != nil {
		t.Skipf("user.Lookup(%q) unsupported here: %v", u.Username, err)
	}
	home := looked.HomeDir

	cases := []struct{ in, want string }{
		{"~" + u.Username, home},
		{"~" + u.Username + "/data", filepath.Join(home, "data")},
		{"~" + u.Username + "/a/b", filepath.Join(home, "a", "b")},
	}
	for _, c := range cases {
		if got := expandHome(c.in); got != c.want {
			t.Errorf("expandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// An unknown user can't be resolved: the literal is returned unchanged so
	// the downstream path-existence check reports it plainly.
	const unknown = "~nsuchuser-tracebloc-181/x"
	if got := expandHome(unknown); got != unknown {
		t.Errorf("expandHome(%q) = %q, want it left literal", unknown, got)
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

// TestRunDatasetRm_InvalidTableExitsTwo: rm validates the table name
// before any cluster work, so an unsafe name is exit-code-2 territory
// and never reaches kubeconfig/cluster resolution.
func TestRunDatasetRm_InvalidTableExitsTwo(t *testing.T) {
	var buf bytes.Buffer
	err := runDataDelete(context.Background(), runDataDeleteArgs{
		Table:   "../bad",
		Printer: ui.New(&buf, ui.WithColor(false)),
	})
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("error is not an *exitError: %v", err)
	}
	if ee.Code() != 2 {
		t.Errorf("exit code = %d, want 2 (invalid table name)", ee.Code())
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
	err := runClusterInfo(context.Background(), ui.New(&buf), bad, "", "", 600)
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
