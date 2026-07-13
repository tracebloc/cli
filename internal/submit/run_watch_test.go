package submit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/tracebloc/cli/internal/ui"
)

// withWatchJob seams watchJobFn so Run's NON-detach path can be exercised with a
// canned WatchResult/error, without a live Job to watch. All six pre-existing Run
// tests set Detach:true and returned before WatchJob, so the whole post-watch
// surface — the WatchError wrap, the per-DetachReason switch, the summary-render
// branch — was unexecuted (coverage root-cause #4). The watch mechanics
// themselves stay covered by watch_test.go.
func withWatchJob(t *testing.T, wr *WatchResult, watchErr error) {
	t.Helper()
	orig := watchJobFn
	t.Cleanup(func() { watchJobFn = orig })
	watchJobFn = func(context.Context, kubernetes.Interface, string, string, io.Writer, *ui.Printer) (*WatchResult, error) {
		return wr, watchErr
	}
}

// runNonDetach drives Run down the watch path (Detach:false) with the submit POST
// faked and WatchJob seamed to return (wr, watchErr). Returns the rendered output,
// the Result, and the error.
func runNonDetach(t *testing.T, wr *WatchResult, watchErr error) (string, *Result, error) {
	t.Helper()
	withWatchJob(t, wr, watchErr)
	var out bytes.Buffer
	res, err := Run(context.Background(), Options{
		Submitter:        &fakeSubmitter{resp: &SubmitResponse{JobName: "ingestor-abc", Namespace: "tracebloc"}},
		Client:           fake.NewClientset(),
		IngestConfigYAML: "apiVersion: tracebloc.io/v1\n",
		Detach:           false,
		Out:              &out,
	})
	return out.String(), res, err
}

// A watch-phase failure is tagged WatchError so the orchestrator maps it to the
// ingest-flavored exit code (9), not the submit one (8) — the run was already
// accepted server-side, so Result.Submit must still survive.
func TestRun_NonDetach_WatchErrorWrapped(t *testing.T) {
	_, res, err := runNonDetach(t, nil, errors.New("pod vanished"))
	if !IsWatchError(err) {
		t.Fatalf("a watch failure must be tagged WatchError (exit-9 mapping), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "watching ingestor Job") || !strings.Contains(err.Error(), "pod vanished") {
		t.Errorf("want the wrapped watch error, got %v", err)
	}
	if res == nil || res.Submit == nil || res.Submit.JobName != "ingestor-abc" {
		t.Errorf("Result.Submit must survive a watch failure (run was accepted), got %+v", res)
	}
}

// The per-DetachReason switch: each timeout reason prints its own diagnostic
// (Bugbot PR #10 r7 — the timeout paths must NOT read as "you pressed Ctrl-C"),
// and every arm appends the same kubectl-logs reconnect hint.
func TestRun_NonDetach_DetachReasonMessages(t *testing.T) {
	cases := []struct {
		name   string
		reason DetachReason
		want   string
	}{
		{"signal", DetachReasonSignal, "the ingestion keeps running"},
		{"pod-wait-timeout", DetachReasonPodWaitTimeout, "hasn't started yet"},
		{"watch-cap", DetachReasonWatchCap, "Stopped following after 1 hour"},
		{"none-default", DetachReasonNone, "the ingestion keeps running"}, // default arm
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, res, err := runNonDetach(t, &WatchResult{Outcome: JobOutcomeDetached, DetachReason: c.reason}, nil)
			if err != nil {
				t.Fatalf("a detach outcome is not an error (run keeps going in-cluster), got %v", err)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("reason %v should render %q:\n%s", c.reason, c.want, out)
			}
			if !strings.Contains(out, "kubectl logs -f -n tracebloc job/ingestor-abc") {
				t.Errorf("every detach arm must print the reconnect hint:\n%s", out)
			}
			if res == nil || res.Watch == nil {
				t.Errorf("Result.Watch must carry the detach result, got %+v", res)
			}
		})
	}
	// Mutation guard: the two timeout arms must NOT collapse to the signal text —
	// pin that PodWaitTimeout does not claim the customer stopped watching.
	out, _, _ := runNonDetach(t, &WatchResult{Outcome: JobOutcomeDetached, DetachReason: DetachReasonPodWaitTimeout}, nil)
	if strings.Contains(out, "Stopped watching") {
		t.Errorf("pod-wait-timeout must not read as a Ctrl-C stop:\n%s", out)
	}
}

// A completed run WITH a parsed summary renders the summary panel.
func TestRun_NonDetach_RendersSummary(t *testing.T) {
	wr := &WatchResult{
		Outcome: JobOutcomeSucceeded,
		Summary: &Summary{IngestorID: "ing-42", TotalRecords: 100, ProcessedRecords: 100, InsertedRecords: 100, APISentRecords: 100},
	}
	out, res, err := runNonDetach(t, wr, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"Ingestion summary", "ing-42"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary panel missing %q:\n%s", want, out)
		}
	}
	if res == nil || res.Watch != wr {
		t.Errorf("Result.Watch should be the watch result, got %+v", res)
	}
}

// A completed run with NO summary (nil) must skip the panel entirely (the
// wr.Summary != nil false branch) — but still return the watch result cleanly.
func TestRun_NonDetach_NoSummaryNoPanel(t *testing.T) {
	out, res, err := runNonDetach(t, &WatchResult{Outcome: JobOutcomeSucceeded, Summary: nil}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out, "Ingestion summary") {
		t.Errorf("no summary → no panel, got:\n%s", out)
	}
	if res == nil || res.Watch == nil || res.Watch.Outcome != JobOutcomeSucceeded {
		t.Errorf("Result.Watch must carry the succeeded outcome, got %+v", res)
	}
}
