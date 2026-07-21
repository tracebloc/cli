package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// TestRunDataDelete_OutputJSON pins the `data delete --output-json`
// contract (cli#297), mirroring data list's: stdout carries exactly one
// JSON object per run (human output stays on the Printer), the exit
// codes are unchanged, and every failure return still emits
// {status:"error", error, exit_code} via the deferred writer — so
// `… --output-json | jq` never sees empty stdout.
//
// Terminal statuses covered: deleted / dry-run / declined (all exit 0 —
// scripts must branch on status, not just the exit code), plus early
// (exit 2, before discovery) and late (exit 7, mid-teardown) failures.
func TestRunDataDelete_OutputJSON(t *testing.T) {
	origRCT, origList, origTD := resolveClusterTargetFn, listDatasetsFn, teardownFn
	t.Cleanup(func() {
		resolveClusterTargetFn, listDatasetsFn, teardownFn = origRCT, origList, origTD
	})

	resolveClusterTargetFn = func(_ context.Context, _ *ui.Printer, _ cluster.KubeconfigOptions, _ activeClientBinding, _ bool) (*clusterTarget, error) {
		return &clusterTarget{
			Resolved:  &cluster.ResolvedConfig{Context: "ctx", Namespace: "tracebloc"},
			Clientset: fake.NewSimpleClientset(),
			Release:   &cluster.ParentRelease{ReleaseName: "tracebloc", IngestorSAName: "tracebloc-ingestor"},
			PVC:       &cluster.SharedPVC{ClaimName: "client-pvc", MountPath: "/data/shared"},
		}, nil
	}
	listDatasetsFn = func(_ context.Context, _ kubernetes.Interface, _ *rest.Config, _ string) ([]string, error) {
		return []string{"churn"}, nil
	}
	teardownFn = func(_ context.Context, _ kubernetes.Interface, _ push.Executor, _ string, plan push.TeardownPlan, _ push.PodSpecOptions) (push.TeardownResult, error) {
		return push.TeardownResult{DroppedTable: true, RemovedPaths: plan.PVCPaths}, nil
	}

	run := func(a runDataDeleteArgs) (dataDeleteJSON, string, string, error) {
		var jsonBuf, human bytes.Buffer
		a.OutputJSON = true
		a.JSONOut = &jsonBuf
		a.Printer = ui.New(&human, ui.WithColor(false))
		err := runDataDelete(context.Background(), a)
		var got dataDeleteJSON
		if jsonBuf.Len() > 0 {
			if uerr := json.Unmarshal(jsonBuf.Bytes(), &got); uerr != nil {
				t.Fatalf("stdout is not a single JSON object: %v\n%s", uerr, jsonBuf.String())
			}
		}
		return got, jsonBuf.String(), human.String(), err
	}

	t.Run("success -> status deleted, real spelling, exit 0", func(t *testing.T) {
		// Mixed case in: the JSON must carry the case-resolved table name
		// (backend#1027), not the raw argument.
		got, raw, human, err := run(runDataDeleteArgs{Table: "Churn", Yes: true})
		if err != nil {
			t.Fatalf("want nil error (exit 0), got %v", err)
		}
		if got.Status != "deleted" || got.Table != "churn" || got.Database == "" {
			t.Errorf("want status=deleted table=churn (case-resolved) + database, got %+v", got)
		}
		if got.Namespace != "tracebloc" || got.Release != "tracebloc" {
			t.Errorf("want namespace/release from the resolved target, got %+v", got)
		}
		if len(got.RemovedPaths) == 0 || len(got.PVCPaths) == 0 {
			t.Errorf("want pvc_paths + removed_paths populated, got %+v", got)
		}
		if !strings.Contains(human, "Deleted") {
			t.Errorf("human output should still narrate on its own stream:\n%s", human)
		}
		if strings.Contains(raw, "Deleted ") {
			t.Errorf("human copy leaked into the JSON stream:\n%s", raw)
		}
	})

	t.Run("dry-run -> status dry-run, nothing removed, exit 0", func(t *testing.T) {
		got, _, _, err := run(runDataDeleteArgs{Table: "churn", DryRun: true})
		if err != nil {
			t.Fatalf("want nil error, got %v", err)
		}
		if got.Status != "dry-run" {
			t.Errorf("want status=dry-run, got %+v", got)
		}
		if len(got.RemovedPaths) != 0 {
			t.Errorf("dry-run must not report removed paths, got %+v", got.RemovedPaths)
		}
		// [] not null — a script indexing removed_paths must not explode.
		if _, raw, _, _ := run(runDataDeleteArgs{Table: "churn", DryRun: true}); !strings.Contains(raw, `"removed_paths": []`) {
			t.Errorf("nil removed paths should marshal as []:\n%s", raw)
		}
	})

	t.Run("confirm declined -> status declined, exit 0", func(t *testing.T) {
		no := false
		got, _, _, err := run(runDataDeleteArgs{Table: "churn", Prompter: &fakePrompter{confirm: &no}})
		if err != nil {
			t.Fatalf("declining is a safe exit 0, got %v", err)
		}
		if got.Status != "declined" {
			t.Errorf("want status=declined, got %+v", got)
		}
		if len(got.RemovedPaths) != 0 {
			t.Errorf("a declined delete must not report removed paths, got %+v", got.RemovedPaths)
		}
	})

	t.Run("early failure (invalid name, exit 2) -> error JSON via the defer", func(t *testing.T) {
		var jsonBuf bytes.Buffer
		err := runDataDelete(context.Background(), runDataDeleteArgs{
			Table:      "bad name!",
			Yes:        true,
			OutputJSON: true,
			JSONOut:    &jsonBuf,
			Printer:    ui.New(&bytes.Buffer{}, ui.WithColor(false)),
		})
		var ee *exitError
		if !errors.As(err, &ee) || ee.Code() != 2 {
			t.Fatalf("err = %v, want *exitError code 2", err)
		}
		var got map[string]any
		if e := json.Unmarshal(jsonBuf.Bytes(), &got); e != nil {
			t.Fatalf("stdout is not JSON on failure: %v\n%s", e, jsonBuf.String())
		}
		if got["status"] != "error" || got["exit_code"] != float64(2) || got["error"] == "" {
			t.Errorf("got %+v, want status=error exit_code=2 + message", got)
		}
	})

	t.Run("refused off-TTY without --yes (exit 3) -> error JSON", func(t *testing.T) {
		// This is what a scripted --output-json run without --yes hits:
		// the RunE wires no Prompter in JSON mode, so the refusal guard fires.
		var jsonBuf bytes.Buffer
		err := runDataDelete(context.Background(), runDataDeleteArgs{
			Table:      "churn",
			OutputJSON: true,
			JSONOut:    &jsonBuf,
			Printer:    ui.New(&bytes.Buffer{}, ui.WithColor(false)),
		})
		var ee *exitError
		if !errors.As(err, &ee) || ee.Code() != 3 {
			t.Fatalf("err = %v, want *exitError code 3", err)
		}
		var got map[string]any
		if e := json.Unmarshal(jsonBuf.Bytes(), &got); e != nil {
			t.Fatalf("stdout is not JSON on refusal: %v\n%s", e, jsonBuf.String())
		}
		if got["status"] != "error" || got["exit_code"] != float64(3) {
			t.Errorf("got %+v, want status=error exit_code=3", got)
		}
	})

	t.Run("late failure (teardown, exit 7) -> error JSON, no success object", func(t *testing.T) {
		teardownFn = func(_ context.Context, _ kubernetes.Interface, _ push.Executor, _ string, _ push.TeardownPlan, _ push.PodSpecOptions) (push.TeardownResult, error) {
			return push.TeardownResult{}, errors.New("could not reach the mysql pod")
		}
		t.Cleanup(func() {
			teardownFn = func(_ context.Context, _ kubernetes.Interface, _ push.Executor, _ string, plan push.TeardownPlan, _ push.PodSpecOptions) (push.TeardownResult, error) {
				return push.TeardownResult{DroppedTable: true, RemovedPaths: plan.PVCPaths}, nil
			}
		})
		var jsonBuf bytes.Buffer
		err := runDataDelete(context.Background(), runDataDeleteArgs{
			Table:      "churn",
			Yes:        true,
			OutputJSON: true,
			JSONOut:    &jsonBuf,
			Printer:    ui.New(&bytes.Buffer{}, ui.WithColor(false)),
		})
		var ee *exitError
		if !errors.As(err, &ee) || ee.Code() != 7 {
			t.Fatalf("err = %v, want *exitError code 7", err)
		}
		var got map[string]any
		if e := json.Unmarshal(jsonBuf.Bytes(), &got); e != nil {
			t.Fatalf("stdout is not JSON on teardown failure: %v\n%s", e, jsonBuf.String())
		}
		if got["status"] != "error" || got["exit_code"] != float64(7) {
			t.Errorf("got %+v, want status=error exit_code=7", got)
		}
	})
}

// TestDataDeleteCmd_OutputJSONNeverPrompts pins the RunE wiring: in
// --output-json mode no Prompter is created even on what looks like a
// TTY, so a scripted run can never hang on a survey prompt — it either
// has --yes/--dry-run or fails closed with exit 3 (asserted above).
// Wiring-level, so it goes through the cobra command, not runDataDelete.
func TestDataDeleteCmd_OutputJSONNeverPrompts(t *testing.T) {
	origList := listDatasetsFn
	t.Cleanup(func() { listDatasetsFn = origList })

	cmd := newDataDeleteCmd()
	// The production root command silences cobra's usage/error echo
	// (root.go); mirror that here so stdout carries only what the
	// command itself writes.
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"churn", "--output-json", "--kubeconfig", "/nonexistent/kubeconfig"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("want a kubeconfig failure, got nil")
	}
	// stdout must be the JSON error object — nothing else.
	var got map[string]any
	if e := json.Unmarshal(out.Bytes(), &got); e != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", e, out.String())
	}
	if got["status"] != "error" {
		t.Errorf("got %+v, want status=error", got)
	}
	// The human output went to stderr, not stdout.
	if strings.Contains(out.String(), "tracebloc") && !strings.Contains(out.String(), `"error"`) {
		t.Errorf("human output leaked to stdout:\n%s", out.String())
	}
}
