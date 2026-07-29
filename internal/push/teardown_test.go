package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestPlanTeardown pins the artifact set `dataset rm` targets: the
// MySQL table in IngestionDatabase + both PVC dirs (final dest +
// staging), in that order.
func TestPlanTeardown(t *testing.T) {
	plan := PlanTeardown("reg_train")

	if plan.Database != IngestionDatabase {
		t.Errorf("Database = %q, want %q", plan.Database, IngestionDatabase)
	}
	if plan.Table != "reg_train" {
		t.Errorf("Table = %q, want reg_train", plan.Table)
	}

	want := []string{
		"/data/shared/reg_train",
		"/data/shared/.tracebloc-staging/reg_train",
	}
	if len(plan.PVCPaths) != len(want) {
		t.Fatalf("PVCPaths = %v, want %v", plan.PVCPaths, want)
	}
	for i := range want {
		if plan.PVCPaths[i] != want[i] {
			t.Errorf("PVCPaths[%d] = %q, want %q", i, plan.PVCPaths[i], want[i])
		}
	}
}

// TestTeardown_RemovesViaStageIdentityPod is the regression test for
// tracebloc/client#259: `dataset rm` must NOT run the file `rm` inside
// the long-lived jobs-manager pod (a non-root uid that cannot delete
// the uid-65532-owned staging files). It must run it in a short-lived
// pod that mirrors the stage pod's identity (uid 65532 + fsGroup 65532),
// which owns the staging files and so can delete them on any volume type
// (hostPath included, where fsGroup is a no-op).
func TestTeardown_RemovesViaStageIdentityPod(t *testing.T) {
	// A running "mysql" pod must exist so step 1 (DROP TABLE) can locate
	// it. The teardown pod is created by Teardown itself and marked Ready
	// by the reactor (shared with stage_test.go).
	cs := fake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql-0", Namespace: "tracebloc"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "mysql"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	})
	readyOnNextGet(cs)
	fe := &fakeExecutor{}

	plan := PlanTeardown("reg_train")
	res, err := Teardown(context.Background(), cs, fe, "tracebloc", plan, PodSpecOptions{
		Namespace:    "tracebloc",
		PVCClaimName: "client-pvc",
		PVCMountPath: "/data/shared",
		Table:        "reg_train",
	})
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !res.DroppedTable {
		t.Error("DroppedTable = false, want true")
	}

	// The rm is the LAST Exec call. It must target the ephemeral stage
	// pod, NOT a jobs-manager pod — that's the #259 fix.
	if !strings.HasPrefix(fe.gotPod, "tracebloc-stage-") {
		t.Errorf("rm ran in pod %q, want the ephemeral stage pod (tracebloc-stage-*); "+
			"running it in the jobs-manager pod is the #259 bug", fe.gotPod)
	}
	if strings.Contains(fe.gotPod, "jobs-manager") {
		t.Errorf("rm ran in the jobs-manager pod (%q) — the #259 regression", fe.gotPod)
	}
	if fe.gotContainer != "stage" {
		t.Errorf("rm container = %q, want stage", fe.gotContainer)
	}
	wantCmd := "rm -rf " + strings.Join(plan.PVCPaths, " ")
	if got := strings.Join(fe.gotCmd, " "); got != wantCmd {
		t.Errorf("rm cmd = %q, want %q", got, wantCmd)
	}

	// The teardown pod must run as the stage uid (65532) + fsGroup so it
	// OWNS the staging files it deletes. Inspect the created Pod via the
	// fake clientset action log (the pod is deleted before the test ends).
	var sc *corev1.PodSecurityContext
	for _, action := range cs.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" {
			sc = action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Spec.SecurityContext
			break
		}
	}
	if sc == nil {
		t.Fatal("no Pod create observed — teardown did not spawn an ephemeral pod")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 65532 {
		t.Errorf("teardown pod RunAsUser = %v, want 65532", sc.RunAsUser)
	}
	if sc.FSGroup == nil || *sc.FSGroup != 65532 {
		t.Errorf("teardown pod FSGroup = %v, want 65532", sc.FSGroup)
	}

	// No leaked teardown pods after a successful run.
	pods, _ := cs.CoreV1().Pods("tracebloc").List(context.Background(),
		metav1.ListOptions{LabelSelector: StagePodManagedByLabel + "=" + StagePodManagedByValue})
	if len(pods.Items) != 0 {
		t.Errorf("Teardown leaked %d stage pod(s)", len(pods.Items))
	}
}

// TestCleanStaging_RemovesOnlyStagingPrefix pins the staging-leak fix:
// on a clean success the CLI reclaims ONLY .tracebloc-staging/<table>
// (StagedPrefix) — never the final table dir (FinalDestPrefix) and never
// the MySQL table — via the same ephemeral stage-identity pod Teardown
// uses (so the rm works by ownership on hostPath + CSI).
func TestCleanStaging_RemovesOnlyStagingPrefix(t *testing.T) {
	cs := fake.NewClientset()
	readyOnNextGet(cs)
	fe := &fakeExecutor{}

	if err := CleanStaging(context.Background(), cs, fe, "tracebloc", "reg_train", PodSpecOptions{
		Namespace:    "tracebloc",
		PVCClaimName: "client-pvc",
		PVCMountPath: "/data/shared",
		Table:        "reg_train",
	}); err != nil {
		t.Fatalf("CleanStaging: %v", err)
	}

	// The rm must target ONLY the staging prefix — not the final table dir.
	wantCmd := "rm -rf " + StagedPrefix("reg_train")
	if got := strings.Join(fe.gotCmd, " "); got != wantCmd {
		t.Errorf("rm cmd = %q, want %q", got, wantCmd)
	}
	if strings.Contains(strings.Join(fe.gotCmd, " "), FinalDestPrefix("reg_train")) {
		t.Errorf("rm cmd %q touched the final table dir — CleanStaging must never remove FinalDestPrefix", fe.gotCmd)
	}

	// It must run in the ephemeral stage-identity pod, not jobs-manager.
	if !strings.HasPrefix(fe.gotPod, "tracebloc-stage-") {
		t.Errorf("rm ran in pod %q, want the ephemeral stage pod (tracebloc-stage-*)", fe.gotPod)
	}
	if fe.gotContainer != "stage" {
		t.Errorf("rm container = %q, want stage", fe.gotContainer)
	}

	// No leaked cleanup pods.
	pods, _ := cs.CoreV1().Pods("tracebloc").List(context.Background(),
		metav1.ListOptions{LabelSelector: StagePodManagedByLabel + "=" + StagePodManagedByValue})
	if len(pods.Items) != 0 {
		t.Errorf("CleanStaging leaked %d stage pod(s)", len(pods.Items))
	}
}

// TestCleanStaging_PodCreateFailureReturnsError confirms the reclaim
// surfaces a pod-create failure as an error (the caller logs it as a
// non-fatal warning — a leftover staging copy must never fail an
// otherwise-successful ingest).
func TestCleanStaging_PodCreateFailureReturnsError(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("PSA denied")
	})
	fe := &fakeExecutor{}

	err := CleanStaging(context.Background(), cs, fe, "tracebloc", "reg_train", PodSpecOptions{
		Namespace: "tracebloc", PVCClaimName: "client-pvc", PVCMountPath: "/data/shared", Table: "reg_train",
	})
	if err == nil {
		t.Fatal("CleanStaging returned nil, want an error when the cleanup pod can't be created")
	}
	if fe.gotCmd != nil {
		t.Errorf("rm ran (%v) despite the pod never being created", fe.gotCmd)
	}
}

// TestTeardown_CleansBookkeepingRows pins the RFC-0003 I6 half of teardown
// (tracebloc/backend#1209): after the DROP, the ingestor's run-journal and
// salt rows for the table are deleted best-effort — and a failure there
// never fails a teardown whose DROP already succeeded.
func TestTeardown_CleansBookkeepingRows(t *testing.T) {
	newCS := func() *fake.Clientset {
		cs := fake.NewClientset(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "mysql-0", Namespace: "tracebloc"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "mysql"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		})
		readyOnNextGet(cs)
		return cs
	}
	opts := PodSpecOptions{
		Namespace:    "tracebloc",
		PVCClaimName: "client-pvc",
		PVCMountPath: "/data/shared",
		Table:        "ds_0f2ab1de3c444e558f66778899aabbcc",
	}
	plan := PlanTeardown("ds_0f2ab1de3c444e558f66778899aabbcc")

	t.Run("journal and salt rows are deleted for the dropped table", func(t *testing.T) {
		rec := &recordingExecutor{}
		res, err := Teardown(context.Background(), newCS(), rec, "tracebloc", plan, opts)
		if err != nil {
			t.Fatalf("Teardown: %v", err)
		}
		if !res.BookkeepingCleaned {
			t.Error("BookkeepingCleaned = false, want true")
		}
		// The SQL must arrive on STDIN with its quoted literal intact —
		// never through a shell -e argument, where the literal's single
		// quotes would be eaten by the shell (Bugbot: the DELETEs would
		// silently no-op forever).
		var journal, salt bool
		for _, call := range rec.calls {
			if strings.Contains(strings.Join(call.cmd, " "), "DELETE FROM") {
				t.Errorf("DELETE passed as a shell argument (%q) — must be fed on stdin", call.cmd)
			}
			stdin := string(call.stdin)
			want := "WHERE table_name='" + plan.Table + "'"
			if strings.Contains(stdin, "DELETE FROM") && strings.Contains(stdin, ingestRunsTable) && strings.Contains(stdin, want) {
				journal = true
			}
			if strings.Contains(stdin, "DELETE FROM") && strings.Contains(stdin, ingestMetaTable) && strings.Contains(stdin, want) {
				salt = true
			}
		}
		if !journal {
			t.Errorf("no stdin DELETE against %s with a quoted literal for %s observed", ingestRunsTable, plan.Table)
		}
		if !salt {
			t.Errorf("no stdin DELETE against %s with a quoted literal for %s observed", ingestMetaTable, plan.Table)
		}
	})

	t.Run("bookkeeping failure never fails the teardown", func(t *testing.T) {
		rec := &recordingExecutor{failWhenStdinContains: "DELETE FROM"}
		res, err := Teardown(context.Background(), newCS(), rec, "tracebloc", plan, opts)
		if err != nil {
			t.Fatalf("Teardown should tolerate bookkeeping failures, got: %v", err)
		}
		if !res.DroppedTable {
			t.Error("DroppedTable = false, want true")
		}
		if res.BookkeepingCleaned {
			t.Error("BookkeepingCleaned = true, want false when the DELETEs fail")
		}
		if len(res.RemovedPaths) == 0 {
			t.Error("PVC rm did not run — bookkeeping failure must not short-circuit step 2")
		}
	})
}

// recordingExecutor records every Exec call (command AND stdin) and can
// fail calls whose stdin matches a marker.
type recordingExecutor struct {
	calls                 []execCall
	failWhenStdinContains string
}

type execCall struct {
	pod, container string
	cmd            []string
	stdin          []byte
}

func (r *recordingExecutor) Exec(ctx context.Context, namespace, pod, container string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	r.calls = append(r.calls, execCall{pod: pod, container: container, cmd: cmd, stdin: in})
	if r.failWhenStdinContains != "" && strings.Contains(string(in), r.failWhenStdinContains) {
		return fmt.Errorf("simulated bookkeeping failure")
	}
	return nil
}
