package push

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
