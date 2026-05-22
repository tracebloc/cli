package submit

import (
	"context"
	"errors"
	"fmt"
	"io"

	"k8s.io/client-go/kubernetes"
)

// Options bundles every dependency Run needs. The CLI builds one
// from the resolved Phase 2/3 state (kubeconfig clientset, SA
// token, jobs-manager endpoint) + the flags from Phase 4
// (--detach, --idempotency-key, --image-digest).
type Options struct {
	// Submitter is how the POST reaches jobs-manager. Production
	// uses NewHTTPSubmitter(endpoint, token); tests inject a
	// fake that captures the request + returns a canned response.
	Submitter Submitter

	// Client is the kubernetes.Interface for the watch loop's
	// Job + Pod polls. Same clientset Phase 3 used.
	Client kubernetes.Interface

	// IngestConfigYAML is the synthesized YAML body to POST.
	// Phase 3 already produced this in canonical form
	// (push.SpecArgs.Build → yaml.Marshal) so we don't re-marshal
	// here.
	IngestConfigYAML string

	// IdempotencyKey overrides the auto-generated random key.
	// Empty = let BuildRequest generate a fresh one. Used by
	// the --idempotency-key flag for retry-safety across
	// invocations.
	IdempotencyKey string

	// ImageDigest optionally pins the ingestor image. Empty =
	// jobs-manager uses the cluster's configured default.
	ImageDigest string

	// Detach exits immediately after the 201 — no watch, no log
	// streaming, no summary. Used by CI scenarios where the
	// customer just wants the Job name in stdout and the run
	// proceeds asynchronously in the cluster.
	Detach bool

	// Out is the customer-facing log stream. Submit writes the
	// 201 announcement here, then either streams the Pod's logs
	// to it (live watch) or prints the Job name (detach). The
	// rendered summary panel also goes here.
	Out io.Writer
}

// Result is what Run reports back to the CLI orchestrator.
// Outcome drives the exit-code mapping (see cli/dataset.go's
// Phase 4 wiring); JobName + PodName are echoed back so the CLI
// can build "reconnect with kubectl logs <pod> -n <ns>" hints.
type Result struct {
	// Submit is the 201 response from jobs-manager. Non-nil on
	// any path that got past the POST (including --detach +
	// failed watches). nil only if the POST itself failed.
	Submit *SubmitResponse

	// Watch is the result of the watch loop. nil on --detach
	// (we never started watching). nil on early POST failure.
	Watch *WatchResult
}

// Run is the Phase 4 top-level entrypoint. Steps:
//
//  1. BuildRequest from the YAML + flags
//  2. POST via opts.Submitter; surface SubmitError verbatim
//  3. Print the 201 announcement (job_name / namespace / replay flag)
//  4. If --detach, exit
//  5. WatchJob until Pod terminates or ctx cancels
//  6. Render the parsed Summary panel
//
// Returns the Result + an error. Errors come from steps 1-2-5; on
// steps 3 + 4 + 6, success means "we got far enough" and the
// outcome of the actual ingestion is in Result.Watch.Outcome.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Out == nil {
		opts.Out = io.Discard
	}

	req, err := BuildRequest(opts.IngestConfigYAML, opts.IdempotencyKey, opts.ImageDigest)
	if err != nil {
		return nil, fmt.Errorf("building submit request: %w", err)
	}

	resp, err := opts.Submitter.Submit(ctx, req)
	if err != nil {
		return nil, err
	}

	// 201 announcement. Customer sees this whether --detach is
	// set or not, so they have the Job name for kubectl-poke
	// follow-up.
	if resp.Replay {
		_, _ = fmt.Fprintf(opts.Out,
			"Replayed: idempotency key matches a previous run; attaching to existing Job %s/%s\n",
			resp.Namespace, resp.JobName)
	} else {
		_, _ = fmt.Fprintf(opts.Out,
			"Submitted: jobs-manager spawned ingestor Job %s/%s\n",
			resp.Namespace, resp.JobName)
	}

	if opts.Detach {
		// --detach: print the reconnect hint and bail. The
		// cluster continues without us; the customer can come
		// back with `kubectl logs -f -n <ns> job/<job-name>`.
		_, _ = fmt.Fprintf(opts.Out,
			"Detached (no log streaming). Reconnect with: kubectl logs -f -n %s job/%s\n",
			resp.Namespace, resp.JobName)
		return &Result{Submit: resp}, nil
	}

	// Watch loop. ctx propagates SIGINT cancellation (main.go's
	// signal.NotifyContext); a Ctrl-C during the watch produces
	// Outcome=Detached + the reconnect hint below.
	_, _ = fmt.Fprintf(opts.Out, "Streaming logs from Job %s/%s:\n", resp.Namespace, resp.JobName)
	wr, err := WatchJob(ctx, opts.Client, resp.Namespace, resp.JobName, opts.Out)
	if err != nil {
		// Tag as WatchError so the orchestrator picks the
		// ingest-flavored exit code (9), not the submit-flavored
		// one (8). The cluster has already accepted the run by
		// this point — the CLI just failed to follow it.
		return &Result{Submit: resp}, &WatchError{Err: fmt.Errorf("watching ingestor Job: %w", err)}
	}

	// Detach-from-SIGINT path. Print the reconnect hint so the
	// customer knows the cluster keeps running.
	if wr.Outcome == JobOutcomeDetached {
		_, _ = fmt.Fprintln(opts.Out)
		_, _ = fmt.Fprintf(opts.Out,
			"Detached on signal. Ingestion continues in the cluster. "+
				"Reconnect with: kubectl logs -f -n %s job/%s\n",
			resp.Namespace, resp.JobName)
		return &Result{Submit: resp, Watch: wr}, nil
	}

	// Render the summary panel if the ingestor produced one.
	// Both Succeeded and Failed paths print it — on Failed, the
	// banner tells the customer what got partially through.
	if wr.Summary != nil {
		_, _ = fmt.Fprintln(opts.Out)
		_, _ = fmt.Fprint(opts.Out, RenderPanel(wr.Summary))
	}

	return &Result{Submit: resp, Watch: wr}, nil
}

// IsAuthError reports whether the error is the auth-flavored case
// (401/403 from jobs-manager). The orchestrator's exit-code
// mapping uses this to distinguish "your SA token doesn't work"
// from "your spec was rejected."
func IsAuthError(err error) bool {
	var se *SubmitError
	if !errors.As(err, &se) {
		return false
	}
	return se.StatusCode == 401 || se.StatusCode == 403
}

// WatchError wraps errors that originated in the watch phase
// (waitForJobPod, log streaming, finalJobStatus). The
// orchestrator distinguishes these from submit-phase errors so
// the exit-code mapping is correct: jobs-manager accepted the
// run already, the cluster is doing the work, the CLI just
// failed to follow along. Maps to exit code 9 (ingest-side
// problem), not 8 (submit-side problem). Bugbot flagged the
// previous "everything that wasn't auth → exit 8" version on
// PR #10.
type WatchError struct {
	Err error
}

func (e *WatchError) Error() string { return e.Err.Error() }
func (e *WatchError) Unwrap() error { return e.Err }

// IsWatchError reports whether err originated in the watch phase
// rather than the submit phase. The orchestrator's exit-code
// branch uses this directly.
func IsWatchError(err error) bool {
	var we *WatchError
	return errors.As(err, &we)
}
