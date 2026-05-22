package submit

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// jobPod constructs a Pod owned by `jobName` via the standard
// batch/v1 job-name label. Used to seed the fake clientset for
// waitForJobPod tests.
func jobPod(name, jobName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "tracebloc",
			Labels:    map[string]string{"job-name": jobName},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// TestWaitForJobPod_RunningPodSurfaces: a Pod with job-name label
// in Phase=Running is returned. Pin the label-selector contract +
// the happy-path return.
func TestWaitForJobPod_RunningPodSurfaces(t *testing.T) {
	cs := fake.NewClientset(jobPod("ingestor-abc-xyz", "ingestor-abc", corev1.PodRunning))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	name, err := waitForJobPod(ctx, cs, "tracebloc", "ingestor-abc")
	if err != nil {
		t.Fatalf("waitForJobPod: %v", err)
	}
	if name != "ingestor-abc-xyz" {
		t.Errorf("name = %q, want ingestor-abc-xyz", name)
	}
}

// TestWaitForJobPod_PicksMostRecentNotFirst: Jobs with retries
// (or any multi-Pod scenario) produce multiple Pods with the
// same `job-name=<j>` label. The List API doesn't guarantee
// order — picking items[0] could grab the old Failed Pod from a
// prior retry while the current Running one waits. Bugbot
// PR #10 r4 caught the missing tie-break.
func TestWaitForJobPod_PicksMostRecentNotFirst(t *testing.T) {
	now := time.Now()
	older := jobPod("ingestor-old-failed", "ingestor", corev1.PodFailed)
	older.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))

	newer := jobPod("ingestor-new-running", "ingestor", corev1.PodRunning)
	newer.CreationTimestamp = metav1.NewTime(now.Add(-1 * time.Minute))

	cs := fake.NewClientset(older, newer)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	name, err := waitForJobPod(ctx, cs, "tracebloc", "ingestor")
	if err != nil {
		t.Fatalf("waitForJobPod: %v", err)
	}
	if name != "ingestor-new-running" {
		t.Errorf("name = %q, want ingestor-new-running "+
			"(most-recent useful-phase Pod, not items[0])", name)
	}
}

// TestWaitForJobPod_AllPendingKeepsPolling: if every Pod is still
// Pending (image pulling, scheduling), the function keeps polling
// rather than returning a Pending Pod's name (Pods with no log
// stream yet aren't useful to attach to).
func TestWaitForJobPod_AllPendingKeepsPolling(t *testing.T) {
	cs := fake.NewClientset(jobPod("pending-1", "ingestor", corev1.PodPending))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := waitForJobPod(ctx, cs, "tracebloc", "ingestor")
	if err == nil {
		t.Fatal("waitForJobPod returned nil on all-Pending; expected DeadlineExceeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error doesn't wrap DeadlineExceeded: %v", err)
	}
}

// TestWaitForJobPod_FastCompletionPath: ingestions that finish
// faster than the poll interval might be in Phase=Succeeded by
// the time the watch loop checks. We still want the Pod's name so
// we can fetch its (post-mortem) logs.
func TestWaitForJobPod_FastCompletionPath(t *testing.T) {
	cs := fake.NewClientset(jobPod("ingestor-fast", "ingestor", corev1.PodSucceeded))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	name, err := waitForJobPod(ctx, cs, "tracebloc", "ingestor")
	if err != nil {
		t.Fatalf("waitForJobPod on Succeeded: %v", err)
	}
	if name != "ingestor-fast" {
		t.Errorf("name = %q, want ingestor-fast", name)
	}
}

// TestWaitForJobPod_ForbiddenIsTerminal: an RBAC denial on
// `list pods` must short-circuit the wait — otherwise the
// customer sits through PodReadyTimeout for an error that
// doesn't change.
func TestWaitForJobPod_ForbiddenIsTerminal(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("list", "pods",
		func(_ k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				corev1.Resource("pods"), "",
				errors.New("user cannot list pods"))
		})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := waitForJobPod(ctx, cs, "tracebloc", "j")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("waitForJobPod returned nil on Forbidden")
	}
	if elapsed > 3*time.Second {
		t.Errorf("waitForJobPod waited %s on Forbidden; expected immediate return", elapsed)
	}
}

// TestWaitForJobPod_NoPodEverShows: ingestor Job that doesn't
// spawn its Pod (image pull stuck, scheduling impossible) hits
// the PodReadyTimeout. Bound the test's ctx so it doesn't wait
// the full 5min.
func TestWaitForJobPod_NoPodEverShows(t *testing.T) {
	cs := fake.NewClientset() // empty

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := waitForJobPod(ctx, cs, "tracebloc", "missing-job")
	if err == nil {
		t.Fatal("waitForJobPod returned nil when no Pod ever appeared")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error doesn't wrap DeadlineExceeded: %v", err)
	}
}

// jobWithCondition constructs a batch/v1 Job whose status reports
// the given terminal condition. Used to seed finalJobStatus tests.
func jobWithCondition(name string, cond batchv1.JobConditionType) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tracebloc"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type:   cond,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

// TestFinalJobStatus_Complete: Job with Condition=Complete maps
// to JobOutcomeSucceeded.
func TestFinalJobStatus_Complete(t *testing.T) {
	cs := fake.NewClientset(jobWithCondition("done", batchv1.JobComplete))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := finalJobStatus(ctx, cs, "tracebloc", "done")
	if err != nil {
		t.Fatalf("finalJobStatus: %v", err)
	}
	if out != JobOutcomeSucceeded {
		t.Errorf("Outcome = %v, want Succeeded", out)
	}
}

// TestFinalJobStatus_Failed: Job with Condition=Failed maps to
// JobOutcomeFailed (drives the exit-9 "ingest failed" path).
func TestFinalJobStatus_Failed(t *testing.T) {
	cs := fake.NewClientset(jobWithCondition("crashed", batchv1.JobFailed))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := finalJobStatus(ctx, cs, "tracebloc", "crashed")
	if err != nil {
		t.Fatalf("finalJobStatus: %v", err)
	}
	if out != JobOutcomeFailed {
		t.Errorf("Outcome = %v, want Failed", out)
	}
}

// TestFinalJobStatus_TimeoutIsUnknown: if no terminal condition
// posts within 30s of the log stream ending, we return Unknown
// rather than blocking the customer forever. The orchestrator
// renders a useful diagnostic from the streamed logs.
func TestFinalJobStatus_TimeoutIsUnknown(t *testing.T) {
	cs := fake.NewClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "stuck", Namespace: "tracebloc"},
		// No conditions — Job in a weird mid-state.
	})

	// Tight ctx so the test doesn't take 30s.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := finalJobStatus(ctx, cs, "tracebloc", "stuck")
	if err != nil {
		t.Fatalf("finalJobStatus: %v", err)
	}
	if out != JobOutcomeUnknown {
		t.Errorf("Outcome = %v, want Unknown", out)
	}
}

// TestWatchJob_PodWaitTimeoutMapsToDetach: when waitForJobPod
// exhausts its 5-min budget (slow image pull, PSA backlog), the
// submit has already been accepted by jobs-manager — the
// ingestion will run, the CLI just gave up watching within the
// timeout. WatchJob must return Outcome=Detached, not an error
// that bubbles up as exit 9 ("ingestion failed"). Bugbot PR #10
// r5 caught the false-positive exit code.
func TestWatchJob_PodWaitTimeoutMapsToDetach(t *testing.T) {
	cs := fake.NewClientset() // no Pod backing the job-name

	// Tight ctx so the test doesn't actually wait PodReadyTimeout (5m).
	// The DeadlineExceeded from the parent ctx propagates the same
	// way the inner PodReadyTimeout would, exercising the same
	// errors.Is(DeadlineExceeded) branch in WatchJob.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	wr, err := WatchJob(ctx, cs, "tracebloc", "ingestor-stuck", &out)
	if err != nil {
		t.Fatalf("WatchJob returned error on Pod-wait timeout; want nil + Detached: %v", err)
	}
	if wr == nil {
		t.Fatal("WatchJob returned nil result on Pod-wait timeout")
	}
	if wr.Outcome != JobOutcomeDetached {
		t.Errorf("Outcome = %v, want Detached (the cluster keeps running; the CLI just gave up observing)", wr.Outcome)
	}
}

// TestJobOutcome_String: stringer pin so diagnostic output stays
// stable.
func TestJobOutcome_String(t *testing.T) {
	cases := map[JobOutcome]string{
		JobOutcomeSucceeded: "Succeeded",
		JobOutcomeFailed:    "Failed",
		JobOutcomeDetached:  "Detached",
		JobOutcomeUnknown:   "Unknown",
	}
	for o, want := range cases {
		if got := o.String(); got != want {
			t.Errorf("%v.String() = %q, want %q", o, got, want)
		}
	}
}

// TestParserWriter_FeedsParser: the io.Writer adapter that hooks
// the log-stream TeeReader to the SummaryParser. Pin that writes
// flow through to Feed correctly.
func TestParserWriter_FeedsParser(t *testing.T) {
	p := NewSummaryParser()
	pw := parserWriter{parser: p}
	chunks := strings.Split(realIngestorBanner, "\n")
	for _, line := range chunks {
		_, _ = pw.Write([]byte(line + "\n"))
	}
	got := p.Result()
	if got == nil {
		t.Fatal("parserWriter didn't feed parser; Result is nil")
	}
	if got.TotalRecords != 1234 {
		t.Errorf("TotalRecords = %d, want 1234 (via parserWriter)", got.TotalRecords)
	}
}
