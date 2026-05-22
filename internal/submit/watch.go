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
}

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
func WatchJob(
	ctx context.Context,
	cs kubernetes.Interface,
	namespace, jobName string,
	out io.Writer,
) (*WatchResult, error) {
	// 1. Wait for the ingestor Job's Pod to exist + reach Running.
	//    jobs-manager creates the Job and Kubernetes spawns the
	//    Pod asynchronously, so the Pod usually isn't there the
	//    moment after the 201 comes back.
	podName, err := waitForJobPod(ctx, cs, namespace, jobName)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// SIGINT before the Pod even appeared. jobs-manager
			// has accepted the run, the cluster will run it,
			// the CLI is just not watching anymore.
			return &WatchResult{Outcome: JobOutcomeDetached}, nil
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
	summary, logErr := streamPodLogsAndParse(ctx, cs, namespace, podName, out)
	if logErr != nil && !errors.Is(logErr, context.Canceled) {
		return nil, fmt.Errorf("streaming logs from Pod %s/%s: %w", namespace, podName, logErr)
	}

	// 3. ctx cancellation = detach. Pod might still be running
	//    server-side, but as far as the CLI is concerned we've
	//    handed off.
	if errors.Is(ctx.Err(), context.Canceled) {
		return &WatchResult{
			Outcome: JobOutcomeDetached,
			PodName: podName,
			Summary: summary, // may be partial if the customer
			// hit Ctrl-C right at the banner
		}, nil
	}

	// 4. Final status check: even though log streaming ending
	//    SHOULD mean Pod completion, the apiserver may not have
	//    posted the terminal state by the time the log stream
	//    drained. Poll briefly to confirm — bounded at 30s
	//    because beyond that something else is wrong.
	outcome, err := finalJobStatus(ctx, cs, namespace, jobName)
	if err != nil {
		return nil, fmt.Errorf("reading final Job status for %s/%s: %w", namespace, jobName, err)
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
func waitForJobPod(ctx context.Context, cs kubernetes.Interface, namespace, jobName string) (string, error) {
	var podName string
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
			// Pick the first Pod with the label; Jobs default to
			// parallelism=1 so there's only one. If a future
			// jobs-manager spawns parallel Pods, the first one
			// to surface logs is the one we attach to.
			p := pods.Items[0]
			switch p.Status.Phase {
			case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
				// Running is the happy path; Succeeded/Failed
				// mean we missed the Running window (typical
				// for fast-completing ingestions). In all three
				// cases we can fetch logs.
				podName = p.Name
				return true, nil
			default:
				// Pending: still pulling image / scheduling. Keep
				// polling. The PodReadyTimeout caps this loop.
				return false, nil
			}
		})
	if err != nil {
		return "", err
	}
	return podName, nil
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
	// Default scanner buffer is 64 KB per line — fine for log
	// lines but bump to 1 MB to handle the (rare) case where a
	// single ingestion-error stacktrace has a multi-KB Python
	// traceback line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
			return parser.Result(), err
		}
	}
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
