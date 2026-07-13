package submit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// fakeSubmitter captures the request + returns a canned response.
// Used by Run() tests to exercise the orchestrator without needing
// an httptest.Server (covered by client_test.go).
type fakeSubmitter struct {
	gotRequest *SubmitRequest
	resp       *SubmitResponse
	err        error
}

func (f *fakeSubmitter) Submit(_ context.Context, req *SubmitRequest) (*SubmitResponse, error) {
	f.gotRequest = req
	return f.resp, f.err
}

// TestRun_DetachPath_HappyPath: --detach exits immediately after
// the 201 with the reconnect hint. No watch loop, no log streaming.
func TestRun_DetachPath_HappyPath(t *testing.T) {
	sub := &fakeSubmitter{
		resp: &SubmitResponse{
			JobName:   "ingestor-abc",
			Namespace: "tracebloc",
			Replay:    false,
		},
	}
	var out bytes.Buffer

	res, err := Run(context.Background(), Options{
		Submitter:        sub,
		Client:           fake.NewClientset(),
		IngestConfigYAML: "apiVersion: tracebloc.io/v1\n",
		Detach:           true,
		Out:              &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Submit == nil || res.Submit.JobName != "ingestor-abc" {
		t.Errorf("Result.Submit lost: %+v", res.Submit)
	}
	if res.Watch != nil {
		t.Errorf("Result.Watch = %+v, want nil (detach skips watch)", res.Watch)
	}
	for _, want := range []string{
		"Submitted — tracebloc is validating your data",
		"Detached — the ingestion runs in the background",
		"kubectl logs -f -n tracebloc job/ingestor-abc",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q in:\n%s", want, out.String())
		}
	}
}

// TestRun_ReplayPath: replay=true changes the announcement
// wording — "attaching to the run already in progress" — because the
// cluster is already doing the work.
func TestRun_ReplayPath(t *testing.T) {
	sub := &fakeSubmitter{
		resp: &SubmitResponse{
			JobName:   "ingestor-existing",
			Namespace: "tracebloc",
			Replay:    true,
		},
	}
	var out bytes.Buffer

	_, err := Run(context.Background(), Options{
		Submitter:        sub,
		Client:           fake.NewClientset(),
		IngestConfigYAML: "yaml",
		Detach:           true, // skip the watch for this test
		Out:              &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "matches a previous run") {
		t.Errorf("output missing replay framing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "attaching to the run already in progress") {
		t.Errorf("output missing replay-specific wording:\n%s", out.String())
	}
}

// TestRun_SubmitErrorPropagates: a non-2xx from jobs-manager
// stops Run before any watching happens. The error surfaces with
// jobs-manager's body framing (the client.go path).
func TestRun_SubmitErrorPropagates(t *testing.T) {
	sub := &fakeSubmitter{
		err: &SubmitError{
			StatusCode: 422,
			Body:       `{"detail":"bad spec"}`,
			Endpoint:   "http://jm/internal/submit-ingestion-run",
		},
	}
	var out bytes.Buffer

	_, err := Run(context.Background(), Options{
		Submitter:        sub,
		Client:           fake.NewClientset(),
		IngestConfigYAML: "yaml",
		Out:              &out,
	})
	if err == nil {
		t.Fatal("Run returned nil on submit error")
	}
	if !isSubmitError(err) {
		t.Errorf("err is not *SubmitError: %T", err)
	}
}

// TestRun_BuildRequestErrorPropagates: a crypto/rand failure in
// BuildRequest stops Run before the submitter even gets called.
// We can't easily mock crypto/rand, but we can verify the error
// path is wired by checking that any failure here doesn't reach
// the submitter. This test is more about the contract than the
// trigger.
func TestRun_PassesRequestFieldsThrough(t *testing.T) {
	sub := &fakeSubmitter{
		resp: &SubmitResponse{JobName: "j", Namespace: "ns"},
	}
	var out bytes.Buffer

	_, err := Run(context.Background(), Options{
		Submitter:        sub,
		Client:           fake.NewClientset(),
		IngestConfigYAML: "yaml-content-verbatim",
		IdempotencyKey:   "my-key-override",
		ImageDigest:      "sha256:abc",
		Detach:           true,
		Out:              &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sub.gotRequest == nil {
		t.Fatal("submitter never called")
	}
	if sub.gotRequest.IngestConfig != "yaml-content-verbatim" {
		t.Errorf("IngestConfig = %q, want yaml-content-verbatim",
			sub.gotRequest.IngestConfig)
	}
	if sub.gotRequest.IdempotencyKey != "my-key-override" {
		t.Errorf("IdempotencyKey = %q, want override value",
			sub.gotRequest.IdempotencyKey)
	}
	if sub.gotRequest.ImageDigest != "sha256:abc" {
		t.Errorf("ImageDigest = %q, want sha256:abc",
			sub.gotRequest.ImageDigest)
	}
}

// TestRun_PrintsCorrelationId_GeneratedKey: the auto-generated
// idempotency key doubles as the end-to-end correlation id
// (backend#1028 item 3) — the same string becomes the Job's
// TRACEBLOC_INGEST_CORRELATION_ID env, its ingestion-run label, and
// the backend registration payload's correlation_id. Without this
// line the customer has no copy of the one id that threads all
// layers together.
func TestRun_PrintsCorrelationId_GeneratedKey(t *testing.T) {
	sub := &fakeSubmitter{
		resp: &SubmitResponse{JobName: "j", Namespace: "ns"},
	}
	var out bytes.Buffer

	_, err := Run(context.Background(), Options{
		Submitter:        sub,
		Client:           fake.NewClientset(),
		IngestConfigYAML: "yaml",
		Detach:           true,
		Out:              &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sub.gotRequest == nil {
		t.Fatal("submitter never called")
	}
	want := "Correlation id: " + sub.gotRequest.IdempotencyKey
	if !strings.Contains(out.String(), want) {
		t.Errorf("output missing %q in:\n%s", want, out.String())
	}
}

// TestRun_PrintsCorrelationId_OverrideAndReplay: an explicit
// --idempotency-key override is echoed verbatim, and the line also
// prints on the replay path — a replayed run is exactly when the
// customer reaches for the id to find the already-running Job.
func TestRun_PrintsCorrelationId_OverrideAndReplay(t *testing.T) {
	sub := &fakeSubmitter{
		resp: &SubmitResponse{JobName: "j", Namespace: "ns", Replay: true},
	}
	var out bytes.Buffer

	_, err := Run(context.Background(), Options{
		Submitter:        sub,
		Client:           fake.NewClientset(),
		IngestConfigYAML: "yaml",
		IdempotencyKey:   "nightly-claims-2026.07",
		Detach:           true,
		Out:              &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "Correlation id: nightly-claims-2026.07") {
		t.Errorf("output missing correlation id line in:\n%s", out.String())
	}
}

// TestRun_NilOutDefaultsToDiscard: callers passing nil Out
// shouldn't panic. The orchestrator silently discards output.
func TestRun_NilOutDefaultsToDiscard(t *testing.T) {
	sub := &fakeSubmitter{
		resp: &SubmitResponse{JobName: "j", Namespace: "ns"},
	}
	_, err := Run(context.Background(), Options{
		Submitter:        sub,
		Client:           fake.NewClientset(),
		IngestConfigYAML: "yaml",
		Detach:           true,
		Out:              nil,
	})
	if err != nil {
		t.Fatalf("Run with nil Out panicked or errored: %v", err)
	}
}

// TestIsWatchError: pin the contract that the orchestrator uses
// to distinguish watch-phase failures (exit 9) from submit-phase
// failures (exit 8). Bugbot r1 found the missing distinction;
// this test guards against a regression that drops the typing.
func TestIsWatchError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("plain"), false},
		{"submit error", &SubmitError{StatusCode: 422}, false},
		{"watch error", &WatchError{Err: errors.New("inner")}, true},
		{"wrapped watch error", fmt.Errorf("outer: %w", &WatchError{Err: errors.New("inner")}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsWatchError(c.err); got != c.want {
				t.Errorf("IsWatchError = %v, want %v", got, c.want)
			}
		})
	}
}

// TestIsAuthError: helper smoke test. Pin the contract used by
// the CLI's exit-code mapping.
func TestIsAuthError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"non-submit error", errors.New("network"), false},
		{"submit 401", &SubmitError{StatusCode: 401}, true},
		{"submit 403", &SubmitError{StatusCode: 403}, true},
		{"submit 422", &SubmitError{StatusCode: 422}, false},
		{"submit 500", &SubmitError{StatusCode: 500}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsAuthError(c.err); got != c.want {
				t.Errorf("IsAuthError = %v, want %v", got, c.want)
			}
		})
	}
}
