package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/submit"
	"github.com/tracebloc/cli/internal/ui"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The money path (#1009): submit → classify → exit-code → JSON → reclaim.
// These tests pin the outcome matrix — including the "must NOT reclaim on
// partial failure" gate — without standing up a cluster, via the seams
// (mintIngestorTokenFn / portForwardJobsManagerFn / submitRunFn /
// cleanStagingFn) that runIngestionRun goes through.

func succeededResult() *submit.Result {
	return &submit.Result{
		Submit: &submit.SubmitResponse{Namespace: "tracebloc", JobName: "ingestor-x"},
		// A genuinely clean run: every stage counter equal (total == inserted
		// == api_sent). APISentRecords must be set too — the ingestor-aligned
		// HasFailures() treats api_sent < inserted as a partial, so omitting it
		// (defaulting to 0) would misclassify this "succeeded" row as a failure.
		Watch:  &submit.WatchResult{Outcome: submit.JobOutcomeSucceeded, Summary: &submit.Summary{TotalRecords: 2, InsertedRecords: 2, APISentRecords: 2}},
	}
}

func partialResult() *submit.Result {
	return &submit.Result{
		Submit: &submit.SubmitResponse{Namespace: "tracebloc", JobName: "ingestor-x"},
		Watch:  &submit.WatchResult{Outcome: submit.JobOutcomeSucceeded, Summary: &submit.Summary{TotalRecords: 2, InsertedRecords: 1, FailedRecords: 1}},
	}
}

// TestShouldReclaimStaging pins the must-NOT-reclaim-on-partial gate: the
// staged source is reclaimed ONLY on a clean success.
func TestShouldReclaimStaging(t *testing.T) {
	if !shouldReclaimStaging("succeeded") {
		t.Error(`shouldReclaimStaging("succeeded") = false, want true`)
	}
	for _, st := range []string{
		"completed_with_failures", "failed", "unknown", "detached",
		"auth_error", "submit_error", "watch_error", "dry-run", "error", "",
	} {
		if shouldReclaimStaging(st) {
			t.Errorf("shouldReclaimStaging(%q) = true, want false — a non-clean run must keep the source", st)
		}
	}
}

// TestRunIngestionRun_Matrix drives the whole outcome tail through the seams:
// per row it asserts the exit code, whether the staging reclaim ran, and the
// emitted --output-json status — all in lockstep.
func TestRunIngestionRun_Matrix(t *testing.T) {
	origMint, origPF, origRun, origClean := mintIngestorTokenFn, portForwardJobsManagerFn, submitRunFn, cleanStagingFn
	defer func() {
		mintIngestorTokenFn, portForwardJobsManagerFn, submitRunFn, cleanStagingFn = origMint, origPF, origRun, origClean
	}()

	target := &clusterTarget{
		Resolved:  &cluster.ResolvedConfig{Namespace: "tracebloc"},
		Clientset: nil, // the seams ignore it; the reclaim SPDYExecutor literal doesn't deref
		Release:   &cluster.ParentRelease{IngestorSAName: "ingestor", JobsManagerServiceName: "jm", JobsManagerPort: 8080},
		PVC:       &cluster.SharedPVC{ClaimName: "pvc", MountPath: "/data/shared"},
	}
	spec := map[string]any{"table": "t", "category": "image_classification", "intent": "train", "label": "label"}

	cases := []struct {
		name        string
		mintErr     error
		pfErr       error
		submitRes   *submit.Result
		submitErr   error
		wantCode    int // 0 == success (nil err)
		wantStatus  string
		wantReclaim bool
		wantJSON    bool
	}{
		{"succeeded", nil, nil, succeededResult(), nil, 0, "succeeded", true, true},
		{"partial", nil, nil, partialResult(), nil, 9, "completed_with_failures", false, true},
		{"failed", nil, nil, &submit.Result{Submit: &submit.SubmitResponse{Namespace: "tracebloc", JobName: "j"}, Watch: &submit.WatchResult{Outcome: submit.JobOutcomeFailed}}, nil, 9, "failed", false, true},
		{"detached", nil, nil, &submit.Result{Submit: &submit.SubmitResponse{Namespace: "tracebloc", JobName: "j"}, Watch: nil}, nil, 0, "detached", false, true},
		{"submit-auth", nil, nil, nil, &submit.SubmitError{StatusCode: 401}, 5, "auth_error", false, true},
		{"submit-5xx", nil, nil, nil, &submit.SubmitError{StatusCode: 500}, 8, "submit_error", false, true},
		{"watch-err", nil, nil, nil, &submit.WatchError{Err: errors.New("x")}, 9, "watch_error", false, true},
		// mint / port-forward failures return BEFORE the JSON emit and the
		// reclaim; jsonEmitted is false (runDataIngest's error defer covers it).
		{"mint-fail", errors.New("mint boom"), nil, nil, nil, 5, "", false, false},
		{"pf-fail", nil, errors.New("pf boom"), nil, nil, 8, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reclaimCalled := false
			mintIngestorTokenFn = func(_ context.Context, _ kubernetes.Interface, _, _ string, _ int64, _ []string) (*cluster.IngestorToken, error) {
				if c.mintErr != nil {
					return nil, c.mintErr
				}
				return &cluster.IngestorToken{Token: "tok"}, nil
			}
			portForwardJobsManagerFn = func(_ context.Context, _ kubernetes.Interface, _ *rest.Config, _, _ string, _ int) (*submit.ForwardedConnection, error) {
				if c.pfErr != nil {
					return nil, c.pfErr
				}
				return &submit.ForwardedConnection{LocalPort: 12345}, nil
			}
			submitRunFn = func(_ context.Context, _ submit.Options) (*submit.Result, error) {
				return c.submitRes, c.submitErr
			}
			cleanStagingFn = func(_ context.Context, _ kubernetes.Interface, _ push.Executor, _, _ string, _ push.PodSpecOptions) error {
				reclaimCalled = true
				return nil
			}

			var jsonBuf bytes.Buffer
			a := runDataIngestArgs{
				Spec:       push.SpecArgs{Table: "t"},
				Printer:    ui.New(io.Discard, ui.WithColor(false)),
				OutputJSON: true,
				JSONOut:    &jsonBuf,
			}
			je, err := runIngestionRun(context.Background(), io.Discard, a, target, []byte("yaml"), spec)

			code := 0
			if err != nil {
				var ee *exitError
				if !errors.As(err, &ee) {
					t.Fatalf("err is not *exitError: %v", err)
				}
				code = ee.Code()
			}
			if code != c.wantCode {
				t.Errorf("exit code = %d, want %d", code, c.wantCode)
			}
			if reclaimCalled != c.wantReclaim {
				t.Errorf("reclaim called = %v, want %v (only a clean success reclaims)", reclaimCalled, c.wantReclaim)
			}
			if je != c.wantJSON {
				t.Errorf("jsonEmitted = %v, want %v", je, c.wantJSON)
			}
			if c.wantJSON {
				var got pushJSONResult
				if err := json.Unmarshal(jsonBuf.Bytes(), &got); err != nil {
					t.Fatalf("emitted JSON invalid: %v (%q)", err, jsonBuf.String())
				}
				if got.Status != c.wantStatus {
					t.Errorf("emitted JSON status = %q, want %q", got.Status, c.wantStatus)
				}
			} else if jsonBuf.Len() != 0 {
				t.Errorf("expected no JSON on the pre-submit failure path, got %q", jsonBuf.String())
			}
		})
	}
}

// TestSeamsWiredToRealFns guards that the indirection didn't accidentally
// leave a seam nil (a nil seam would panic the money path in production).
func TestSeamsWiredToRealFns(t *testing.T) {
	if mintIngestorTokenFn == nil || portForwardJobsManagerFn == nil ||
		submitRunFn == nil || cleanStagingFn == nil {
		t.Fatal("a money-path seam is nil — production would panic")
	}
}
