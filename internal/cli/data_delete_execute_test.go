package cli

import (
	"bytes"
	"context"
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

// TestRunDataDelete_Execute covers the destructive teardown path — the P0 the
// coverage audit flagged (runDataDelete was 22%; every existing test stopped at
// cluster discovery). It drives the command past discovery via the
// resolveClusterTargetFn seam (a canned target — no k8s fixture needed) and the
// listDatasetsFn seam (target resolution), then exercises the three teardown
// outcomes through the new teardownFn seam:
//   - clean success  -> exit 0 + a "Deleted" line
//   - table dropped but file removal fails -> exit 7 + the recovery hint
//     (the idempotent-DROP re-run guidance, backend#1027's sibling)
//   - teardown fails before the drop -> exit 7 "teardown failed"
//
// The mixed-case "Churn" case also pins the case-insensitive resolveDeleteTarget
// match (backend#1027: a mis-cased name used to DROP nothing and still exit 0).
func TestRunDataDelete_Execute(t *testing.T) {
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

	run := func(table string) (string, error) {
		var buf bytes.Buffer
		err := runDataDelete(context.Background(), runDataDeleteArgs{
			Table: table, Yes: true, Printer: ui.New(&buf),
		})
		return buf.String(), err
	}

	t.Run("clean teardown -> success", func(t *testing.T) {
		teardownFn = func(_ context.Context, _ kubernetes.Interface, _ push.Executor, _ string, _ push.TeardownPlan, _ push.PodSpecOptions) (push.TeardownResult, error) {
			return push.TeardownResult{RemovedPaths: []string{"/data/shared/churn"}}, nil
		}
		out, err := run("churn")
		if err != nil {
			t.Fatalf("clean teardown: want nil error, got %v", err)
		}
		if !strings.Contains(out, "Deleted") {
			t.Errorf("want a success line, got:\n%s", out)
		}
	})

	t.Run("table dropped but file removal fails -> exit 7 + recovery hint", func(t *testing.T) {
		teardownFn = func(_ context.Context, _ kubernetes.Interface, _ push.Executor, _ string, _ push.TeardownPlan, _ push.PodSpecOptions) (push.TeardownResult, error) {
			return push.TeardownResult{DroppedTable: true}, errors.New("pod exec failed")
		}
		// Mixed case also exercises the case-insensitive resolveDeleteTarget match.
		_, err := run("Churn")
		var ee *exitError
		if !errors.As(err, &ee) || ee.Code() != 7 {
			t.Fatalf("partial failure: want exit 7, got %v", err)
		}
		if !strings.Contains(err.Error(), "was dropped, but removing its files failed") {
			t.Errorf("want the dropped-but-files-remain recovery message, got: %v", err)
		}
	})

	t.Run("teardown fails before the drop -> exit 7 teardown failed", func(t *testing.T) {
		teardownFn = func(_ context.Context, _ kubernetes.Interface, _ push.Executor, _ string, _ push.TeardownPlan, _ push.PodSpecOptions) (push.TeardownResult, error) {
			return push.TeardownResult{}, errors.New("could not reach the mysql pod")
		}
		_, err := run("churn")
		var ee *exitError
		if !errors.As(err, &ee) || ee.Code() != 7 {
			t.Fatalf("pre-drop failure: want exit 7, got %v", err)
		}
		if !strings.Contains(err.Error(), "teardown failed") {
			t.Errorf("want a plain \"teardown failed\" message, got: %v", err)
		}
	})
}
