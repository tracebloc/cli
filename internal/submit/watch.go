package submit

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/tracebloc/cli/internal/ui"
)

// Watch-loop tunables. Both deliberately conservative — Phase 4's
// watching is the dominant time-spend of a typical push (the actual
// data move was done in Phase 3; what's left is the in-cluster
// ingestion which can take minutes-to-an-hour for large datasets).
const (
	// JobPollInterval is how often the watch loop re-Gets the Job.
	// 2s is a sweet spot: human-perceptible enough that a
	// 30-second ingestion has clean lifecycle output, light
	// enough that an hour-long ingestion adds <2000 API calls
	// (negligible at the apiserver's ~10k req/s ceiling).
	JobPollInterval = 2 * time.Second

	// JobWatchTimeout is the absolute cap on a single
	// dataset-push's watch phase. 1 hour is generous — typical
	// image_classification ingestions finish in <10 min; cap
	// exists to avoid infinite hangs if the cluster goes weird
	// (kubelet stops reporting status, etc.). Customers running
	// hours-long ingestions should use --detach.
	JobWatchTimeout = 1 * time.Hour

	// PodPollInterval is how often we look for the ingestor Job's
	// Pod once we have the Job name. Same 2s as the Job-level
	// poll; same rationale.
	PodPollInterval = 2 * time.Second

	// PodReadyTimeout caps how long the Job's Pod has to be
	// schedulable + Running before we give up looking for it.
	// 5 min covers image pull on a slow registry; beyond that
	// the ingestion isn't going to start at all and the customer
	// wants the diagnostic.
	PodReadyTimeout = 5 * time.Minute
)

// JobOutcome enumerates the terminal states the watch loop reports.
// The orchestrator (submit.go) maps these to exit codes.
type JobOutcome int

const (
	// JobOutcomeUnknown is the zero value — never returned, used
	// only as a switch-default sentinel.
	JobOutcomeUnknown JobOutcome = iota

	// JobOutcomeSucceeded means the ingestor Job's Pod exited 0.
	// Maps to exit code 0 in the orchestrator.
	JobOutcomeSucceeded

	// JobOutcomeFailed means the ingestor Job's Pod exited
	// non-zero (any cause: ingestion runtime error, OOM, image
	// crashloop). Maps to the "ingest" exit code (9) in the
	// orchestrator. The Pod-side summary banner may or may not
	// have been printed depending on how far the run got — the
	// orchestrator parses what it can.
	JobOutcomeFailed

	// JobOutcomeDetached means the customer Ctrl-C'd mid-watch.
	// jobs-manager already accepted the run; the cluster will
	// continue without us. Maps to exit code 0 with a "reconnect
	// with kubectl logs" hint in the orchestrator output.
	JobOutcomeDetached
)

func (o JobOutcome) String() string {
	switch o {
	case JobOutcomeSucceeded:
		return "Succeeded"
	case JobOutcomeFailed:
		return "Failed"
	case JobOutcomeDetached:
		return "Detached"
	default:
		return "Unknown"
	}
}

// WatchResult bundles everything the orchestrator wants from the
// watch loop. Outcome drives the exit code; PodName is what the
// detach-hint prints; Summary is the structured INGESTION SUMMARY
// (nil if the run didn't produce one — early failure, OOM at
// startup, etc.).
type WatchResult struct {
	Outcome JobOutcome
	PodName string

	// Summary is the parsed 📊 banner, nil on early failure.
	// The orchestrator decides whether to render the panel
	// (success path) or include it in the failure framing
	// (failed-after-summary path).
	Summary *Summary

	// DetachReason qualifies the Detached outcome — set only
	// when Outcome == JobOutcomeDetached. Lets the orchestrator
	// print an accurate per-reason diagnostic (signal → stopped
	// watching, pod-wait timeout → not started yet, watch cap →
	// stopped following after 1 hour) instead of one one-size-fits-all
	// message. Bugbot PR #10 r7 flagged the misleading "signal"
	// framing for the timeout-detach paths.
	DetachReason DetachReason
}

// DetachReason enumerates the conditions that produce a Detached
// outcome. Used by the orchestrator's diagnostic output only —
// the exit-code mapping treats all detach reasons as success (0)
// because the cluster keeps running regardless of why we stopped
// watching.
type DetachReason int

const (
	// DetachReasonNone is the zero value, used when Outcome is
	// not Detached.
	DetachReasonNone DetachReason = iota

	// DetachReasonSignal: customer pressed Ctrl-C (or a parent
	// process sent SIGTERM). The original Detach semantic.
	DetachReasonSignal

	// DetachReasonPodWaitTimeout: PodReadyTimeout (5 min)
	// exhausted before the ingestor Pod reached a useful
	// phase. Slow image pull, scheduling backlog, PSA rejection.
	DetachReasonPodWaitTimeout

	// DetachReasonWatchCap: JobWatchTimeout (1 hour) exceeded
	// during log streaming. Long-running ingestion that
	// outlasted the observation window.
	DetachReasonWatchCap
)

// WatchJob is the top-level watch loop: poll the Job until it
// reaches a terminal phase, stream the Pod's logs while it's
// running, and return a WatchResult.
//
// SIGINT contract (Bugbot-r9 echo for the previous package):
// the caller (cli/main.go via signal.NotifyContext) is expected
// to cancel ctx on Ctrl-C. WatchJob detects ctx.Err() == Canceled
// and returns Outcome=Detached rather than treating it as a poll
// failure. The customer who Ctrl-C'd during the watch sees the
// "your ingestion is still running in the cluster; reconnect with
// kubectl logs <pod>" hint.
//
// out is the customer-facing log stream (typically os.Stdout).
// Logs are written verbatim — no prefix, no munging — so the
// stream looks identical to `kubectl logs -f <pod>`.
//
// p (may be nil) renders a live spinner during the otherwise-silent
// wait for the ingestor Pod to schedule + pull its image — without it
// the CLI printed one line and then went quiet for up to PodReadyTimeout
// (5 min), looking hung. When p is nil (tests, --output-json-to-a-pipe)
// the wait is silent as before.
func WatchJob(
	ctx context.Context,
	cs kubernetes.Interface,
	namespace, jobName string,
	out io.Writer,
	p *ui.Printer,
) (*WatchResult, error) {
	// Keep the customer's original ctx separately so finalJobStatus
	// can derive a FRESH 30s context from it (rather than inheriting
	// a possibly-depleted JobWatchTimeout). Bugbot PR #10 r2: the
	// previous "wrap everything in JobWatchTimeout" approach starved
	// finalJobStatus's budget when streaming used most of the hour,
	// so a successful slow ingestion misreported as Unknown → exit 9.
	customerCtx := ctx

	// JobWatchTimeout caps the pod-wait + log-stream phases (the
	// time-spend dominant parts of the watch). finalJobStatus gets
	// its own ctx below.
	watchCtx, cancel := context.WithTimeout(customerCtx, JobWatchTimeout)
	defer cancel()

	// 1. Wait for the ingestor Job's Pod to exist + reach Running.
	//    jobs-manager creates the Job and Kubernetes spawns the
	//    Pod asynchronously, so the Pod usually isn't there the
	//    moment after the 201 comes back — and scheduling + image
	//    pull can take minutes. A spinner keeps that wait honest
	//    instead of looking hung (the pre-spinner behaviour).
	var startSpin *ui.Spinner
	if p != nil {
		startSpin = p.Spinner(
			"Waiting for the ingestion to start (scheduling + pulling the image)",
			"Ctrl-C to stop watching — the run keeps going on the cluster")
	}
	podName, podPhase, err := waitForJobPod(watchCtx, cs, namespace, jobName)
	if startSpin != nil {
		startSpin.Stop()
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// SIGINT before the Pod even appeared. jobs-manager
			// has accepted the run, the cluster will run it,
			// the CLI is just not watching anymore.
			return &WatchResult{
				Outcome:      JobOutcomeDetached,
				DetachReason: DetachReasonSignal,
			}, nil
		}
		// PodReadyTimeout (5min) exhausted = slow image pull /
		// scheduling backlog / PSA still rejecting. The submit
		// was accepted, the run will (eventually) execute in the
		// cluster — the CLI just gave up observing within the
		// timeout. Treat as Detached, not ingest-failed: bumping
		// to exit 9 would falsely claim the ingestion failed.
		// Bugbot PR #10 r5 flagged the false-positive exit code.
		if errors.Is(err, context.DeadlineExceeded) {
			return &WatchResult{
				Outcome:      JobOutcomeDetached,
				DetachReason: DetachReasonPodWaitTimeout,
			}, nil
		}
		return nil, fmt.Errorf("waiting for ingestor Pod: %w", err)
	}

	// 2. Stream Pod logs. This blocks until the Pod terminates
	//    or ctx is cancelled. We don't need a separate Job-status
	//    poll here because the Pod terminating drains the log
	//    stream — when GetLogs(Follow=true) returns EOF, the
	//    Pod has completed (success or failure).
	//
	//    Any text the ingestor prints (including the 📊 banner
	//    at the end) flows verbatim through `out`. We also feed
	//    a side-channel to the summary parser so we end up with
	//    a structured representation of the banner without
	//    requiring a second log fetch post-completion.
	if p != nil {
		switch podPhase {
		case corev1.PodFailed:
			// Don't success-frame a pod that's already Failed (immediate crash, or a
			// Failed pod left by a prior backoffLimit retry that bestPod selected) —
			// its crash logs are about to stream and finalJobStatus will report the
			// failure. Neutral line, no green ✔.
			p.Infof("Ingestion started — streaming logs:")
		case corev1.PodSucceeded:
			// A fast ingestion the poll caught only after it already finished — the
			// logs below are REPLAYED, not a live stream, so don't imply "live
			// progress". The final summary still confirms the outcome.
			p.Successf("Ingestion complete — showing its logs:")
		default:
			// Running (the common case): the stream below is live.
			p.Successf("Ingestion started — live progress:")
		}
	}
	summary, logErr := streamPodLogsAndParse(watchCtx, cs, namespace, podName, out)

	// 3. Detach branches — checked FIRST, since the customer's SIGINT
	//    or the watch-cap expiry is the operative intent and takes
	//    precedence over any stream error:
	//    - customerCtx canceled = SIGINT
	//    - watchCtx expired (DeadlineExceeded) = JobWatchTimeout cap
	//      hit during streaming (1-hour observation window exceeded)
	//
	//    Both are "the cluster keeps running; the CLI just gave up
	//    observing." Same UX as the PodReadyTimeout case from r5
	//    above — exit 0 with the kubectl-logs reconnect hint.
	//    Bugbot PR #10 r6 flagged the inconsistency: r5 detached
	//    on PodReady timeout but the watch-cap exit still mapped
	//    to exit 9.
	if errors.Is(customerCtx.Err(), context.Canceled) || errors.Is(watchCtx.Err(), context.DeadlineExceeded) {
		reason := DetachReasonSignal
		if errors.Is(watchCtx.Err(), context.DeadlineExceeded) &&
			!errors.Is(customerCtx.Err(), context.Canceled) {
			// Pure watchCtx-only expiry = JobWatchTimeout. The
			// customerCtx-canceled case takes precedence (if both
			// fired, the customer's intent was SIGINT).
			reason = DetachReasonWatchCap
		}
		return &WatchResult{
			Outcome:      JobOutcomeDetached,
			PodName:      podName,
			Summary:      summary, // may be partial
			DetachReason: reason,
		}, nil
	}

	// 4. Final status check with a FRESH 30s budget derived from
	//    the customer's ctx (not watchCtx, which may be near-
	//    expired after a long log stream). Bugbot PR #10 r2:
	//    inheriting watchCtx's depleted budget caused successful
	//    slow ingestions to misreport as Unknown.
	//
	//    The Job — not the log stream — is the source of truth for
	//    success/failure, so we ALWAYS consult it here, INCLUDING when
	//    the log stream broke for a non-ctx reason (#28: the watched
	//    Pod was replaced / restarted / deleted mid-follow, e.g. a
	//    backoffLimit retry). A broken stream is only fatal if we also
	//    can't determine the Job's outcome.
	//
	//    The fresh ctx still propagates SIGINT (parent is customerCtx,
	//    which carries signal.NotifyContext's cancel); a Ctrl-C in this
	//    window falls into the detach branches below.
	finalCtx, finalCancel := context.WithTimeout(customerCtx, 30*time.Second)
	defer finalCancel()
	outcome, statusErr := finalJobStatus(finalCtx, cs, namespace, jobName)

	// A non-ctx log-stream error is incidental if the Job still
	// reached a terminal state. Previously ANY such error (e.g.
	// "container is terminated" once the Pod was replaced by a retry)
	// returned exit 9 even when the Job ultimately succeeded. #28.
	streamFailed := logErr != nil &&
		!errors.Is(logErr, context.Canceled) &&
		!errors.Is(logErr, context.DeadlineExceeded)
	if streamFailed {
		// SIGINT during the final-status poll → graceful detach.
		if errors.Is(customerCtx.Err(), context.Canceled) {
			return &WatchResult{
				Outcome:      JobOutcomeDetached,
				PodName:      podName,
				Summary:      summary,
				DetachReason: DetachReasonSignal,
			}, nil
		}
		// Job reached a terminal state → the stream error was
		// incidental (Pod replaced/restarted). Report the real
		// outcome the customer cares about.
		if statusErr == nil && (outcome == JobOutcomeSucceeded || outcome == JobOutcomeFailed) {
			return &WatchResult{Outcome: outcome, PodName: podName, Summary: summary}, nil
		}
		// Couldn't confirm a terminal Job state → the stream failure
		// is the actionable signal; surface it.
		return nil, fmt.Errorf("streaming logs from Pod %s/%s: %w", namespace, podName, logErr)
	}

	// 5. Clean-stream path: classify on the Job status alone.
	if statusErr != nil {
		// Treat SIGINT during finalJobStatus as graceful detach
		// (jobs-manager already accepted the run; the customer is
		// just stopping the observation). Bugbot PR #10 r2 flagged
		// the "exit 9 on post-stream SIGINT" inconsistency.
		if errors.Is(customerCtx.Err(), context.Canceled) {
			return &WatchResult{
				Outcome:      JobOutcomeDetached,
				PodName:      podName,
				Summary:      summary,
				DetachReason: DetachReasonSignal,
			}, nil
		}
		return nil, fmt.Errorf("reading final Job status for %s/%s: %w", namespace, jobName, statusErr)
	}
	return &WatchResult{
		Outcome: outcome,
		PodName: podName,
		Summary: summary,
	}, nil
}

// waitForJobPod polls until the Job has spawned its Pod and that
// Pod has reached Phase=Running. The selection key is the
// `job-name=<jobName>` label that batch/v1 controllers attach to
// every Pod they create.
func waitForJobPod(ctx context.Context, cs kubernetes.Interface, namespace, jobName string) (string, corev1.PodPhase, error) {
	var podName string
	var podPhase corev1.PodPhase
	err := wait.PollUntilContextTimeout(ctx, PodPollInterval, PodReadyTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "job-name=" + jobName,
			})
			if err != nil {
				// Terminal errors short-circuit (echoes the
				// Phase-3 r3 fix in push/pod.go).
				if apierrors.IsForbidden(err) {
					return false, err
				}
				return false, nil // transient
			}
			if len(pods.Items) == 0 {
				return false, nil // Pod hasn't been created yet
			}
			// Pick the MOST RECENT useful-phase Pod, not just
			// items[0]. A Job with backoffLimit > 0 (or a Job
			// where jobs-manager re-spawned the Pod for any
			// reason) can have multiple Pods bearing the same
			// `job-name=<jobName>` label. The List API doesn't
			// guarantee order, so items[0] could be the old
			// Failed Pod from a prior retry instead of the
			// current Running one. Bugbot PR #10 r4 caught this.
			//
			// "Useful phase" = Running (happy path) | Succeeded
			// (fast-completing ingestion we missed) | Failed
			// (terminated; we still want its logs). Pending Pods
			// don't count — they have no logs to stream yet, so
			// we keep polling until they either transition or
			// become irrelevant.
			var bestPod *corev1.Pod
			for i := range pods.Items {
				p := &pods.Items[i]
				switch p.Status.Phase {
				case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
					if bestPod == nil ||
						p.CreationTimestamp.After(bestPod.CreationTimestamp.Time) {
						bestPod = p
					}
				}
			}
			if bestPod == nil {
				return false, nil // all Pods still Pending
			}
			podName = bestPod.Name
			podPhase = bestPod.Status.Phase
			return true, nil
		})
	if err != nil {
		return "", "", err
	}
	return podName, podPhase, nil
}

// streamPodLogsAndParse opens a streaming log read on the Pod and
// pipes it through (a) a TeeReader to `out` for verbatim display
// and (b) a Summary parser for the 📊 banner. Returns the parsed
// Summary (or nil if the Pod never produced one) + the underlying
// stream error.
//
// Streaming model: Follow=true means the API server keeps the
// connection open until the Pod terminates. When the Pod's
// container exits, the stream returns EOF and we drop out. This
// avoids the "poll twice" anti-pattern where Phase 4 would have to
// re-fetch logs after the Job is done to see the summary.
func streamPodLogsAndParse(
	ctx context.Context,
	cs kubernetes.Interface,
	namespace, podName string,
	out io.Writer,
) (*Summary, error) {
	req := cs.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Follow: true,
		// Container omitted — Job Pods have exactly one container
		// (the ingestor). If a future ingestor adds a sidecar
		// (e.g. for metrics scrape), this needs to specify
		// `Container: "ingestor"`.
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	// Wrap the stream in a TeeReader so each line flows through
	// both customer-facing output AND the summary parser. The
	// parser keeps a small ring buffer internally; it doesn't
	// need to see the full stream in memory.
	parser := NewSummaryParser()
	tee := io.TeeReader(stream, parserWriter{parser: parser})

	// Line-by-line copy so the customer sees output progressively.
	// io.Copy would also work but would buffer chunks at the
	// transport layer, making the output feel laggy on a fast
	// ingestion.
	scanner := bufio.NewScanner(tee)
	// The scanner splits the DISPLAY stream on '\n' (the parser is fed
	// separately via the TeeReader, so this cap never affects the verdict).
	// tqdm — a data-ingestors dep — redraws its progress bar with '\r' and NO
	// '\n', so a whole ingestion phase's worth of redraws is a single
	// newline-delimited "line" that grows for the life of the run. At 1 MB that
	// overflowed on a big ingestion (ErrTooLong cut the stream mid-run, and a
	// still-running Job then couldn't be confirmed terminal in the 30s
	// finalJobStatus poll → a false exit 9). The watch is capped at 1h
	// (JobWatchTimeout), which bounds the accumulation; 16 MB clears a fast
	// (~10/s) hour of redraws with headroom. The buffer grows on demand, so
	// ordinary log lines still cost 64 KB.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		// Print the line + a newline (scanner strips the trailing
		// '\n'). errcheck-friendly: we discard the writer error
		// because the exit code is the customer-facing contract.
		_, _ = out.Write(line)
		_, _ = out.Write([]byte("\n"))
	}
	if err := scanner.Err(); err != nil {
		// EOF is normal end-of-stream; other errors (network drop
		// mid-stream, ctx cancel) get propagated.
		if !errors.Is(err, io.EOF) {
			// Flush any buffered partial line before returning so
			// the parser sees content even on mid-line failure.
			parser.FlushLine()
			return parser.Result(), err
		}
	}
	// Flush at the end of stream too. A Pod that exited without
	// a trailing newline on its final stdout write would otherwise
	// lose the final line — including potentially the closing
	// ═-rule that finalizes the banner. Bugbot flagged on PR #10.
	parser.FlushLine()
	return parser.Result(), nil
}

// parserWriter adapts a SummaryParser into an io.Writer for use
// with io.TeeReader. The TeeReader writes everything to its
// secondary sink as bytes flow through; the parser's Feed method
// accepts them line-by-line internally.
type parserWriter struct {
	parser *SummaryParser
}

func (pw parserWriter) Write(b []byte) (int, error) {
	pw.parser.Feed(b)
	return len(b), nil
}

// finalJobStatus does a bounded poll on the Job's status to
// determine Succeeded vs Failed after log streaming ends. This is
// a separate step because the log-stream-end doesn't always race
// the Job-status-update; we need to wait briefly for the
// apiserver to post the terminal phase.
func finalJobStatus(ctx context.Context, cs kubernetes.Interface, namespace, jobName string) (JobOutcome, error) {
	var outcome JobOutcome
	err := wait.PollUntilContextTimeout(ctx, JobPollInterval, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			job, err := cs.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) {
					return false, err
				}
				return false, nil
			}
			// batch/v1 Job conditions: Complete (success) or
			// Failed. Poll until one is set; in practice this
			// resolves within ~1s of log stream EOF.
			for _, c := range job.Status.Conditions {
				if c.Status != corev1.ConditionTrue {
					continue
				}
				switch c.Type {
				case batchv1.JobComplete:
					outcome = JobOutcomeSucceeded
					return true, nil
				case batchv1.JobFailed:
					outcome = JobOutcomeFailed
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		// If the poll timed out without seeing a terminal
		// condition, the apiserver is being slow. Treat as
		// Unknown rather than failing the whole push — the
		// orchestrator can render a useful diagnostic from the
		// streamed logs.
		if errors.Is(err, context.DeadlineExceeded) {
			return JobOutcomeUnknown, nil
		}
		return JobOutcomeUnknown, err
	}
	return outcome, nil
}
