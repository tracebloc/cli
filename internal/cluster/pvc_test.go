package cluster

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// seedPVC returns a corev1.PersistentVolumeClaim wired to look like
// the chart's shared-data claim. Lets each test mutate one field
// (phase, access mode) to exercise its specific branch.
func seedPVC(phase corev1.PersistentVolumeClaimPhase, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
	if len(modes) == 0 {
		modes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SharedPVCClaimName,
			Namespace: "tracebloc",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: modes,
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: phase,
		},
	}
}

func TestDiscoverSharedPVC_HappyPath(t *testing.T) {
	cs := fake.NewClientset(seedPVC(corev1.ClaimBound))
	got, err := DiscoverSharedPVC(context.Background(), cs, "tracebloc")
	if err != nil {
		t.Fatalf("DiscoverSharedPVC: %v", err)
	}
	if got.ClaimName != SharedPVCClaimName {
		t.Errorf("ClaimName = %q, want %q", got.ClaimName, SharedPVCClaimName)
	}
	if got.MountPath != SharedPVCMountPath {
		t.Errorf("MountPath = %q, want %q", got.MountPath, SharedPVCMountPath)
	}
	if got.Phase != corev1.ClaimBound {
		t.Errorf("Phase = %v, want Bound", got.Phase)
	}
	if !got.IsReadWriteMany() {
		t.Errorf("IsReadWriteMany() = false on a RWX-seeded PVC")
	}
}

func TestDiscoverSharedPVC_NotFound(t *testing.T) {
	// Empty cluster — the parent-release check should normally
	// have failed first, but we still pin the diagnostic for the
	// case where customers renamed the PVC out-of-band.
	cs := fake.NewClientset()
	_, err := DiscoverSharedPVC(context.Background(), cs, "tracebloc")
	if err == nil {
		t.Fatal("DiscoverSharedPVC returned nil error on empty cluster")
	}
	// Diagnostic must point at the v0.2 follow-up and `kubectl
	// get pvc` command — those are the actionable bits.
	for _, want := range []string{SharedPVCClaimName, "v0.2", "kubectl get pvc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %s", want, err)
		}
	}
}

func TestDiscoverSharedPVC_UnboundPhase(t *testing.T) {
	// Most common cause: missing StorageClass on EKS where the
	// admin removed gp2-default before installing the chart. The
	// pre-flight diagnostic must call out StorageClass explicitly
	// so customers know what to fix.
	cs := fake.NewClientset(seedPVC(corev1.ClaimPending))
	_, err := DiscoverSharedPVC(context.Background(), cs, "tracebloc")
	if err == nil {
		t.Fatal("DiscoverSharedPVC returned nil error on Pending PVC")
	}
	for _, want := range []string{"Pending", "not Bound", "StorageClass"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %s", want, err)
		}
	}
}

func TestIsReadWriteMany_RWO(t *testing.T) {
	// ReadWriteOnce — common on cheap-storage clusters (single-node
	// minikube, single-zone EBS). Phase 3 still works against RWO
	// (the scheduler co-locates), but PR-b will print a warning.
	// Pin the API surface that the warning logic will key off.
	cs := fake.NewClientset(seedPVC(corev1.ClaimBound, corev1.ReadWriteOnce))
	got, err := DiscoverSharedPVC(context.Background(), cs, "tracebloc")
	if err != nil {
		t.Fatalf("DiscoverSharedPVC: %v", err)
	}
	if got.IsReadWriteMany() {
		t.Errorf("IsReadWriteMany() = true on a RWO-seeded PVC")
	}
}

func TestIsReadWriteMany_RWXMixedWithOther(t *testing.T) {
	// A few cloud providers (specifically EFS-on-EKS) advertise
	// both RWX and ROX. As long as RWX is in the list, our stage
	// Pod can schedule freely.
	cs := fake.NewClientset(seedPVC(corev1.ClaimBound,
		corev1.ReadOnlyMany, corev1.ReadWriteMany))
	got, err := DiscoverSharedPVC(context.Background(), cs, "tracebloc")
	if err != nil {
		t.Fatalf("DiscoverSharedPVC: %v", err)
	}
	if !got.IsReadWriteMany() {
		t.Errorf("IsReadWriteMany() = false on a mixed-mode PVC including RWX")
	}
}
