package submit

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The full port-forward (PortForwardJobsManager) requires a real
// apiserver + SPDY upgrade, so it's out of scope for unit tests —
// covered by the EKS smoke. What IS testable: pickServicePod's
// Service→Pod resolution, which is the only client-go-only logic
// in the file.

// svc constructs a Service with the given selector. Used to seed
// the fake clientset.
func svc(name string, selector map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tracebloc"},
		Spec:       corev1.ServiceSpec{Selector: selector},
	}
}

// podForSvc constructs a Pod with labels matching the selector.
func podForSvc(name string, labels map[string]string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "tracebloc",
			Labels:    labels,
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// TestPickServicePod_HappyPath: a Service with one matching Running
// Pod resolves to that Pod's name.
func TestPickServicePod_HappyPath(t *testing.T) {
	sel := map[string]string{"app": "jobs-manager"}
	cs := fake.NewClientset(
		svc("jobs-manager", sel),
		podForSvc("jobs-manager-abc", sel, corev1.PodRunning),
	)
	p, err := pickServicePod(context.Background(), cs, "tracebloc", "jobs-manager")
	if err != nil {
		t.Fatalf("pickServicePod: %v", err)
	}
	if p.Name != "jobs-manager-abc" {
		t.Errorf("Pod name = %q, want jobs-manager-abc", p.Name)
	}
}

// TestPickServicePod_SkipsNonRunning: Pending / Failed Pods backing
// the same Service are filtered out — the port-forward only works
// against a Running Pod.
func TestPickServicePod_SkipsNonRunning(t *testing.T) {
	sel := map[string]string{"app": "jobs-manager"}
	cs := fake.NewClientset(
		svc("jobs-manager", sel),
		podForSvc("crashed", sel, corev1.PodFailed),
		podForSvc("pending", sel, corev1.PodPending),
		podForSvc("running", sel, corev1.PodRunning),
	)
	p, err := pickServicePod(context.Background(), cs, "tracebloc", "jobs-manager")
	if err != nil {
		t.Fatalf("pickServicePod: %v", err)
	}
	if p.Name != "running" {
		t.Errorf("Pod name = %q, want running", p.Name)
	}
}

// TestPickServicePod_NoMatchingPod: a Service whose Pods are all
// non-Running (or absent) surfaces a clear error pointing at the
// kubectl command to debug.
func TestPickServicePod_NoMatchingPod(t *testing.T) {
	sel := map[string]string{"app": "jobs-manager"}
	cs := fake.NewClientset(
		svc("jobs-manager", sel),
		podForSvc("crashed", sel, corev1.PodFailed),
	)
	_, err := pickServicePod(context.Background(), cs, "tracebloc", "jobs-manager")
	if err == nil {
		t.Fatal("pickServicePod returned nil on no-Running-Pod")
	}
	for _, want := range []string{
		"no Running Pod",
		"jobs-manager",
		"kubectl get pods",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestPickServicePod_ServiceMissing: trying to port-forward to a
// non-existent Service surfaces with the Service name in the error.
func TestPickServicePod_ServiceMissing(t *testing.T) {
	cs := fake.NewClientset() // empty
	_, err := pickServicePod(context.Background(), cs, "tracebloc", "missing-svc")
	if err == nil {
		t.Fatal("pickServicePod returned nil on missing Service")
	}
	if !strings.Contains(err.Error(), "missing-svc") {
		t.Errorf("error missing service name: %v", err)
	}
}

// TestPickServicePod_NoSelector: ExternalName Services and other
// selector-less shapes can't be port-forwarded by Pod lookup.
// Surface a clear error rather than silently picking nothing.
func TestPickServicePod_NoSelector(t *testing.T) {
	cs := fake.NewClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "externalname-svc", Namespace: "tracebloc"},
		// no Spec.Selector
	})
	_, err := pickServicePod(context.Background(), cs, "tracebloc", "externalname-svc")
	if err == nil {
		t.Fatal("pickServicePod returned nil on selector-less Service")
	}
	if !strings.Contains(err.Error(), "no selector") {
		t.Errorf("error missing selector-less framing: %v", err)
	}
}

// TestForwardedConnection_CloseIdempotent: Close is safe to call
// multiple times. defer-Close patterns at multiple levels of the
// orchestrator shouldn't risk a double-close panic.
func TestForwardedConnection_CloseIdempotent(t *testing.T) {
	stopCh := make(chan struct{})
	done := make(chan struct{})
	close(done) // simulate goroutine already finished
	f := &ForwardedConnection{LocalPort: 12345, stopCh: stopCh, done: done}
	f.Close()
	f.Close() // must not panic
	f.Close()
}
