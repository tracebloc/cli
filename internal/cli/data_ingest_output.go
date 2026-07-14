// The machine-readable output surface of `data ingest`: the --output-json
// wire types + writers, and classifyPushOutcome, which keeps the JSON
// status string and the process exit code in lockstep (kept in one file
// with writePushJSON deliberately — they are mutation-hardened together).
// Moved verbatim from data.go (cli#282) — behavior unchanged.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/submit"
)

// classifyPushOutcome maps the result of submit.Run to a machine-
// readable status string + the process exit error, kept in lockstep so
// --output-json's status always agrees with the exit code (a nil
// *exitError = success, exit 0). It also covers the error paths
// (auth/submit/watch) so --output-json can still emit a result object
// when submit.Run returns an error. (Bugbot #38.)
func classifyPushOutcome(res *submit.Result, err error) (string, *exitError) {
	if err != nil {
		switch {
		case submit.IsAuthError(err):
			return "auth_error", &exitError{code: exitAuth, err: err}
		case submit.IsWatchError(err):
			// jobs-manager accepted the run; the cluster is doing the
			// work, the CLI just couldn't follow along — ingest-side
			// (exit 9), not submit-side (8).
			return "watch_error", &exitError{code: exitIngestFailed, err: err}
		default:
			return "submit_error", &exitError{code: exitSubmitFailed, err: err}
		}
	}
	// --detach (no watch) or SIGINT-mid-watch: success; cluster runs on.
	if res == nil || res.Watch == nil || res.Watch.Outcome == submit.JobOutcomeDetached {
		return "detached", nil
	}
	switch res.Watch.Outcome {
	case submit.JobOutcomeFailed:
		return "failed", &exitError{code: exitIngestFailed, err: errors.New("ingestion Job exited non-zero — see logs above")}
	case submit.JobOutcomeUnknown:
		return "unknown", &exitError{code: exitIngestFailed, err: errors.New(
			"ingestion Job's final status couldn't be determined within the watch window — " +
				"check `kubectl get job -n " + res.Submit.Namespace + " " + res.Submit.JobName + "` for the outcome")}
	case submit.JobOutcomeSucceeded:
		// Job exited 0, but rows can still have failed — exit 9, and the
		// JSON status must say so, NOT "succeeded". (Bugbot #38.)
		if res.Watch.Summary != nil && res.Watch.Summary.HasFailures() {
			return "completed_with_failures", &exitError{code: exitIngestFailed, err: errors.New(
				"ingestion Job completed but the summary reports failures — see panel above")}
		}
		return "succeeded", nil
	}
	return "unknown", nil
}

// pushJSONResult is the machine-readable shape emitted by --output-json.
// It's a presentation type owned by the CLI layer, so submit.Summary
// stays json-tag-free and this wire format can evolve independently.
type pushJSONResult struct {
	Status    string           `json:"status"` // dry-run|succeeded|completed_with_failures|failed|detached|unknown|auth_error|submit_error|watch_error|error
	Table     string           `json:"table"`
	Category  string           `json:"category"`
	Intent    string           `json:"intent"`
	Namespace string           `json:"namespace,omitempty"`
	JobName   string           `json:"job_name,omitempty"`
	Summary   *pushJSONSummary `json:"summary,omitempty"`
	Error     string           `json:"error,omitempty"`
	ExitCode  int              `json:"exit_code,omitempty"`
}

type pushJSONSummary struct {
	IngestorID           string  `json:"ingestor_id,omitempty"`
	TotalRecords         int64   `json:"total_records"`
	InsertedRecords      int64   `json:"inserted_records"`
	SentToAPI            int64   `json:"sent_to_api"`
	SkippedRecords       int64   `json:"skipped_records"`
	FileTransferFailures int64   `json:"file_transfer_failures"`
	DBInsertFailures     int64   `json:"db_insert_failures"`
	SuccessRate          float64 `json:"success_rate"`
}

// writePushJSON serializes the push result to w (stdout in
// --output-json mode). Errors are dropped: marshaling our own struct
// can't fail in practice, and the exit code remains the contract.
func writePushJSON(w io.Writer, status string, spec map[string]any, s *submit.Summary, ns, jobName string) {
	res := pushJSONResult{
		Status:    status,
		Table:     fmt.Sprintf("%v", spec["table"]),
		Category:  fmt.Sprintf("%v", spec["category"]),
		Intent:    fmt.Sprintf("%v", spec["intent"]),
		Namespace: ns,
		JobName:   jobName,
	}
	if s != nil {
		res.Summary = &pushJSONSummary{
			IngestorID:           s.IngestorID,
			TotalRecords:         s.TotalRecords,
			InsertedRecords:      s.InsertedRecords,
			SentToAPI:            s.APISentRecords,
			SkippedRecords:       s.SkippedRecords,
			FileTransferFailures: s.FileTransferFailures,
			DBInsertFailures:     s.FailedRecords,
			SuccessRate:          s.SuccessRate(),
		}
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, string(b))
}

// writePushErrorJSON emits a JSON error object for --output-json runs
// that fail before a result is produced (validation, discovery,
// staging, token, port-forward). Keeps the stdout-always-JSON contract
// so a script parsing it never hits empty output on failure. (Bugbot #49)
func writePushErrorJSON(w io.Writer, sp push.SpecArgs, e error, code int) {
	res := pushJSONResult{
		Status:   "error",
		Table:    sp.Table,
		Category: sp.Category,
		Intent:   sp.Intent,
		Error:    e.Error(),
		ExitCode: code,
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, string(b))
}
