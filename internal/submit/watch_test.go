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

	"github.com/tracebloc/cli/internal/testutil"
	"github.com/tracebloc/cli/internal/ui"
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

	name, phase, err := waitForJobPod(ctx, cs, "tracebloc", "ingestor-abc")
	if err != nil {
		t.Fatalf("waitForJobPod: %v", err)
	}
	if name != "ingestor-abc-xyz" {
		t.Errorf("name = %q, want ingestor-abc-xyz", name)
	}
	if phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running (drives the success-vs-neutral start line)", phase)
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

	name, phase, err := waitForJobPod(ctx, cs, "tracebloc", "ingestor")
	if err != nil {
		t.Fatalf("waitForJobPod: %v", err)
	}
	if name != "ingestor-new-running" {
		t.Errorf("name = %q, want ingestor-new-running "+
			"(most-recent useful-phase Pod, not items[0])", name)
	}
	if phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running (the newer pod's phase, not the old Failed one)", phase)
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

	_, _, err := waitForJobPod(ctx, cs, "tracebloc", "ingestor")
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

	name, phase, err := waitForJobPod(ctx, cs, "tracebloc", "ingestor")
	if err != nil {
		t.Fatalf("waitForJobPod on Succeeded: %v", err)
	}
	if name != "ingestor-fast" {
		t.Errorf("name = %q, want ingestor-fast", name)
	}
	if phase != corev1.PodSucceeded {
		t.Errorf("phase = %q, want Succeeded", phase)
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
	_, _, err := waitForJobPod(ctx, cs, "tracebloc", "j")
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

	_, _, err := waitForJobPod(ctx, cs, "tracebloc", "missing-job")
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
	wr, err := WatchJob(ctx, cs, "tracebloc", "ingestor-stuck", &out, nil)
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

// TestWatchJob_TerminalJobStatusWins is the #28 regression pin: the
// Job — not the log stream — is the source of truth for the outcome.
// With a Running Pod and a Job already reporting Complete, WatchJob
// must return Succeeded. This holds whether the (fake) log stream
// yields data or breaks: if the stream errored, the new
// "streamFailed but Job terminal → report outcome" branch still
// resolves to the Job's verdict instead of bubbling exit-9. Before
// the fix, a broken stream (e.g. a Pod replaced by a retry, or
// deleted mid-follow) returned an error even on a successful Job.
func TestWatchJob_TerminalJobStatusWins(t *testing.T) {
	cs := fake.NewClientset(
		jobPod("ingestor-xyz", "ingestor", corev1.PodRunning),
		jobWithCondition("ingestor", batchv1.JobComplete),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	wr, err := WatchJob(ctx, cs, "tracebloc", "ingestor", &out, nil)
	if err != nil {
		t.Fatalf("WatchJob returned error; want nil + Succeeded: %v", err)
	}
	if wr == nil {
		t.Fatal("WatchJob returned nil result")
	}
	if wr.Outcome != JobOutcomeSucceeded {
		t.Fatalf("Outcome = %v, want Succeeded (Job condition is the source of truth)", wr.Outcome)
	}
}

// TestWatchJob_TerminalFailedJobReported: the mirror of the above for
// a Failed Job — the watch reports Failed (→ exit 9), not a generic
// watch error, even if the stream broke.
func TestWatchJob_TerminalFailedJobReported(t *testing.T) {
	cs := fake.NewClientset(
		jobPod("ingestor-xyz", "ingestor", corev1.PodRunning),
		jobWithCondition("ingestor", batchv1.JobFailed),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	wr, err := WatchJob(ctx, cs, "tracebloc", "ingestor", &out, nil)
	if err != nil {
		t.Fatalf("WatchJob returned error; want nil + Failed: %v", err)
	}
	if wr == nil {
		t.Fatal("WatchJob returned nil result")
	}
	if wr.Outcome != JobOutcomeFailed {
		t.Fatalf("Outcome = %v, want Failed", wr.Outcome)
	}
}

// TestWatchJob_SucceededPodShowsReplayNotLive: a fast ingestion the poll catches
// only after the Pod already Succeeded must not be framed as "live progress" —
// the logs that follow are replayed, not a live stream. Pins the phase-aware
// start line (bugbot #164).
func TestWatchJob_SucceededPodShowsReplayNotLive(t *testing.T) {
	cs := fake.NewClientset(
		jobPod("ingestor-fast", "ingestor", corev1.PodSucceeded),
		jobWithCondition("ingestor", batchv1.JobComplete),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var buf bytes.Buffer
	p := ui.New(&buf) // the start line is rendered via the printer
	wr, err := WatchJob(ctx, cs, "tracebloc", "ingestor", &buf, p)
	if err != nil {
		t.Fatalf("WatchJob: %v", err)
	}
	if wr.Outcome != JobOutcomeSucceeded {
		t.Fatalf("Outcome = %v, want Succeeded", wr.Outcome)
	}
	s := buf.String()
	if !strings.Contains(s, "Ingestion complete — showing its logs:") {
		t.Errorf("succeeded-pod start line missing; got:\n%s", s)
	}
	if strings.Contains(s, "live progress") {
		t.Errorf("must not claim live progress for an already-completed pod:\n%s", s)
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

// TestWaitForJobPod_SameSecondTiePrefersLive: CreationTimestamp is 1s-granular,
// so a backoffLimit retry created in the same second as the failed Pod it
// replaces ties on .After. Without a tie-break the watch can latch onto the old
// Failed Pod (List order is unspecified). The live Pod must win the tie. #219.
func TestWaitForJobPod_SameSecondTiePrefersLive(t *testing.T) {
	ts := metav1.NewTime(time.Now().Truncate(time.Second))
	failed := jobPod("a-failed", "ingestor", corev1.PodFailed)
	failed.CreationTimestamp = ts
	running := jobPod("b-running", "ingestor", corev1.PodRunning)
	running.CreationTimestamp = ts
	cs := fake.NewClientset(failed, running)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	name, phase, err := waitForJobPod(ctx, cs, "tracebloc", "ingestor")
	if err != nil {
		t.Fatalf("waitForJobPod: %v", err)
	}
	if name != "b-running" || phase != corev1.PodRunning {
		t.Fatalf("got %s/%s, want b-running/Running (live Pod wins a same-second tie over Failed)", name, phase)
	}
}

// TestWatchJob_UnknownJobFallsBackToPodPhase: the Job controller hasn't posted a
// terminal condition (slow/contended apiserver) but the Pod already Succeeded.
// WatchJob must fall back to the Pod phase and report Succeeded rather than
// Unknown — which the orchestrator maps to a false exit 9. #219.
func TestWatchJob_UnknownJobFallsBackToPodPhase(t *testing.T) {
	testutil.SwapSeam(t, &finalJobStatusTimeout, 150*time.Millisecond)

	cs := fake.NewClientset(
		jobPod("ingestor-fast", "ingestor", corev1.PodSucceeded),
		// A Job with no terminal condition yet — finalJobStatus times out Unknown.
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ingestor", Namespace: "tracebloc"}},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	wr, err := WatchJob(ctx, cs, "tracebloc", "ingestor", &out, nil)
	if err != nil {
		t.Fatalf("WatchJob: %v", err)
	}
	if wr.Outcome != JobOutcomeSucceeded {
		t.Fatalf("Outcome = %v, want Succeeded (pod-phase fallback when the Job condition lags)", wr.Outcome)
	}
}

// TestMostRecentUsefulPod covers the selection the Unknown-fallback and
// waitForJobPod share: the newest useful-phase Pod wins, a same-second tie goes
// to the live/succeeded Pod over a Failed one, and all-Pending yields nil.
func TestMostRecentUsefulPod(t *testing.T) {
	now := time.Now()
	mk := func(name string, phase corev1.PodPhase, age time.Duration) corev1.Pod {
		p := jobPod(name, "ingestor", phase)
		p.CreationTimestamp = metav1.NewTime(now.Add(-age))
		return *p
	}

	// Newer Succeeded beats older Failed.
	if best := mostRecentUsefulPod([]corev1.Pod{
		mk("old-failed", corev1.PodFailed, 10*time.Minute),
		mk("new-ok", corev1.PodSucceeded, 1*time.Minute),
	}); best == nil || best.Name != "new-ok" {
		t.Fatalf("want new-ok, got %v", best)
	}
	// Same-second tie → live/succeeded over Failed, regardless of slice order.
	ts := metav1.NewTime(now.Truncate(time.Second))
	f := jobPod("f", "ingestor", corev1.PodFailed)
	f.CreationTimestamp = ts
	s := jobPod("s", "ingestor", corev1.PodSucceeded)
	s.CreationTimestamp = ts
	if best := mostRecentUsefulPod([]corev1.Pod{*f, *s}); best == nil || best.Name != "s" {
		t.Fatalf("same-second tie should prefer the succeeded pod, got %v", best)
	}
	// All Pending → nil (no logs/phase to attach to yet).
	if best := mostRecentUsefulPod([]corev1.Pod{mk("p", corev1.PodPending, time.Minute)}); best != nil {
		t.Fatalf("all-Pending must yield nil, got %v", best)
	}
}

// TestWatchJob_UnknownFallbackPrefersSucceededOverFailed: the Unknown-timeout
// fallback must re-list Pods and pick the current most-recent useful one, not
// classify off a stale Failed Pod. With an older Failed Pod and a newer
// Succeeded one and no Job condition, WatchJob must report Succeeded. #224.
func TestWatchJob_UnknownFallbackPrefersSucceededOverFailed(t *testing.T) {
	testutil.SwapSeam(t, &finalJobStatusTimeout, 150*time.Millisecond)

	now := time.Now()
	failed := jobPod("ingestor-failed", "ingestor", corev1.PodFailed)
	failed.CreationTimestamp = metav1.NewTime(now.Add(-5 * time.Minute))
	succeeded := jobPod("ingestor-retry-ok", "ingestor", corev1.PodSucceeded)
	succeeded.CreationTimestamp = metav1.NewTime(now.Add(-1 * time.Minute))
	cs := fake.NewClientset(
		failed, succeeded,
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ingestor", Namespace: "tracebloc"}}, // no terminal condition
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	wr, err := WatchJob(ctx, cs, "tracebloc", "ingestor", &out, nil)
	if err != nil {
		t.Fatalf("WatchJob: %v", err)
	}
	if wr.Outcome != JobOutcomeSucceeded {
		t.Fatalf("Outcome = %v, want Succeeded (fallback must prefer the newer Succeeded pod over the old Failed one)", wr.Outcome)
	}
}
