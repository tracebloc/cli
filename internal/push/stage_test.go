package push

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// readyOnNextGet adds a reactor that marks any new stage Pod as
// Ready=True the moment after it's created. Without this, the fake
// clientset creates the Pod in default (empty) status and
// WaitForStagePodReady spins until its timeout. We can't easily
// patch the Pod from inside StageOptions construction, so the
// reactor is the standard pattern.
func readyOnNextGet(cs *fake.Clientset) {
	cs.PrependReactor("get", "pods",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			ga, ok := action.(k8stesting.GetAction)
			if !ok {
				return false, nil, nil
			}
			// Let the existing tracker handle the get to find the
			// real object, then patch it before returning. The
			// safest way: do the get ourselves, then synthesize a
			// Ready condition onto the result.
			pod, err := cs.Tracker().Get(action.GetResource(), action.GetNamespace(), ga.GetName())
			if err != nil {
				return false, nil, nil
			}
			p := pod.(*corev1.Pod)
			p.Status.Phase = corev1.PodRunning
			p.Status.Conditions = []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}}
			return true, p, nil
		})
}

// TestStage_HappyPath: orphan scan (empty) → create → wait ready
// → stream → delete. The fake clientset + fake executor let us
// verify the full orchestration without a real cluster.
func TestStage_HappyPath(t *testing.T) {
	root := imgcDir(t)
	layout, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	cs := fake.NewClientset()
	readyOnNextGet(cs)
	fe := &fakeExecutor{}
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = Stage(ctx, StageOptions{
		Client:         cs,
		Executor:       fe,
		Namespace:      "tracebloc",
		IngestorSAName: "ingestor",
		PVCClaimName:   "client-pvc",
		PVCMountPath:   "/data/shared",
		Layout:         layout,
		Table:          "cats_dogs",
		Out:            &out,
	})
	if err != nil {
		t.Fatalf("Stage: %v\nout:\n%s", err, out.String())
	}

	// Diagnostic output: customer should see the lifecycle
	// breadcrumbs. Pin a few key phrases so a regression that
	// silences them is caught.
	for _, want := range []string{"Created stage Pod", "Waiting for stage Pod", "Streaming", "Staged"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}

	// Executor must have been called with the right ns + a tar
	// stream on stdin (assert by inspecting captured tar).
	if fe.gotNS != "tracebloc" {
		t.Errorf("executor ns = %q, want tracebloc", fe.gotNS)
	}
	if len(fe.gotStdin) == 0 {
		t.Error("executor stdin was empty; expected tar archive bytes")
	}
}

// TestStage_CleanupRunsOnError: deferred DeleteStagePod must fire
// even when StreamLayout fails. Otherwise every failed push leaks
// an orphan Pod that can't be reused for retry (the random suffix
// avoids name collisions, but it's still cluster pollution).
func TestStage_CleanupRunsOnError(t *testing.T) {
	root := imgcDir(t)
	layout, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	cs := fake.NewClientset()
	readyOnNextGet(cs)
	fe := &fakeExecutor{
		errToReturn:       errors.New("simulated stream failure"),
		drainBeforeReturn: true,
	}
	var out bytes.Buffer

	err = Stage(context.Background(), StageOptions{
		Client:         cs,
		Executor:       fe,
		Namespace:      "tracebloc",
		IngestorSAName: "ingestor",
		PVCClaimName:   "client-pvc",
		PVCMountPath:   "/data/shared",
		Layout:         layout,
		Table:          "t1",
		Out:            &out,
	})
	if err == nil {
		t.Fatal("Stage returned nil on stream failure")
	}

	// Cleanup contract: NO stage Pods should remain after Stage
	// returns. The list-by-label query is the same one orphan.go
	// uses, so if any leaks it's user-visible in the next push.
	pods, listErr := cs.CoreV1().Pods("tracebloc").List(context.Background(),
		metav1.ListOptions{LabelSelector: StagePodManagedByLabel + "=" + StagePodManagedByValue})
	if listErr != nil {
		t.Fatalf("post-Stage list: %v", listErr)
	}
	if len(pods.Items) != 0 {
		var names []string
		for _, p := range pods.Items {
			names = append(names, p.Name)
		}
		t.Errorf("Stage leaked %d Pod(s) after stream failure: %v", len(pods.Items), names)
	}
}

// TestStage_OrphanWarningSurfaces: stale stage Pods from a
// previous crash get surfaced as a warning at the start of the
// next push. Validates the integration: orphan scan → format →
// print to Out.
func TestStage_OrphanWarningSurfaces(t *testing.T) {
	root := imgcDir(t)
	layout, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	cs := fake.NewClientset(stagePodWithAge("stale-from-crash", "cats_dogs", 30*time.Minute))
	readyOnNextGet(cs)
	fe := &fakeExecutor{}
	var out bytes.Buffer

	err = Stage(context.Background(), StageOptions{
		Client:         cs,
		Executor:       fe,
		Namespace:      "tracebloc",
		IngestorSAName: "ingestor",
		PVCClaimName:   "client-pvc",
		PVCMountPath:   "/data/shared",
		Layout:         layout,
		Table:          "new_push",
		Out:            &out,
	})
	if err != nil {
		t.Fatalf("Stage: %v\nout:\n%s", err, out.String())
	}

	if !strings.Contains(out.String(), "stale-from-crash") {
		t.Errorf("orphan warning didn't surface; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "kubectl delete pod") {
		t.Errorf("orphan warning missing actionable hint; output:\n%s", out.String())
	}
}

// TestStage_IngestorSANameFlowsToPod: the discovered ingestor SA name
// MUST land on the stage Pod's ServiceAccountName — otherwise customers
// whose ingestionAuthz policy names a non-default SA (#7) get pods
// running as the wrong SA (no PVC write access). Pin the integration
// here at the Stage layer since the wiring's effect is only observable
// end-to-end. Bugbot flagged the missing test coverage on PR-a.
func TestStage_IngestorSANameFlowsToPod(t *testing.T) {
	root := imgcDir(t)
	layout, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	cs := fake.NewClientset()
	readyOnNextGet(cs)
	fe := &fakeExecutor{}
	var out bytes.Buffer

	const customSA = "my-renamed-ingestor"
	_ = Stage(context.Background(), StageOptions{
		Client:         cs,
		Executor:       fe,
		Namespace:      "tracebloc",
		IngestorSAName: customSA,
		PVCClaimName:   "client-pvc",
		PVCMountPath:   "/data/shared",
		Layout:         layout,
		Table:          "t1",
		Out:            &out,
	})

	// List the stage Pods created (we deleted them in Stage's
	// defer, so we need to inspect the tracker BEFORE the test
	// process finishes — but the cleanup happens within Stage).
	// Workaround: assert via the fake clientset's action log —
	// look at the Create action's object directly.
	var seenSA string
	for _, action := range cs.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" {
			pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
			seenSA = pod.Spec.ServiceAccountName
			break
		}
	}
	if seenSA != customSA {
		t.Errorf("created Pod's ServiceAccountName = %q, want %q (the --ingestor-sa override)",
			seenSA, customSA)
	}
}

// TestStage_CancelledContext_StillCleansUp: when the parent ctx
// is cancelled (SIGINT scenario), the deferred cleanup MUST still
// run — using its own context with a fresh deadline. Without
// this, every Ctrl-C leaves an orphan Pod.
func TestStage_CancelledContext_StillCleansUp(t *testing.T) {
	root := imgcDir(t)
	layout, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	cs := fake.NewClientset()
	readyOnNextGet(cs)
	fe := &fakeExecutor{
		// Simulate the SPDY stream returning ctx.Err() because the
		// caller cancelled mid-stream.
		errToReturn: context.Canceled,
	}
	var out bytes.Buffer

	// Use a context we cancel immediately. Stage's create still
	// succeeds (fake clientset doesn't honor ctx on Create), but
	// the executor sees Canceled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = Stage(ctx, StageOptions{
		Client:         cs,
		Executor:       fe,
		Namespace:      "tracebloc",
		IngestorSAName: "ingestor",
		PVCClaimName:   "client-pvc",
		PVCMountPath:   "/data/shared",
		Layout:         layout,
		Table:          "interrupted",
		Out:            &out,
	})

	// Whether Stage returns an error or not, the cluster MUST be
	// clean. This is the SIGINT-safety contract.
	pods, _ := cs.CoreV1().Pods("tracebloc").List(context.Background(),
		metav1.ListOptions{LabelSelector: StagePodManagedByLabel + "=" + StagePodManagedByValue})
	if len(pods.Items) != 0 {
		t.Errorf("Stage leaked %d Pod(s) on context-cancel; SIGINT cleanup contract broken",
			len(pods.Items))
	}
}
