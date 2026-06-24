package cluster

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestClusterIDFrom(t *testing.T) {
	cs := fake.NewClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uid-abc-123"},
	})
	got, err := clusterIDFrom(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "uid-abc-123" {
		t.Errorf("cluster id = %q, want uid-abc-123", got)
	}
}

func TestClusterIDFrom_NoNamespace(t *testing.T) {
	cs := fake.NewClientset() // empty cluster — no kube-system
	if _, err := clusterIDFrom(context.Background(), cs); err == nil {
		t.Error("expected an error when kube-system is absent")
	}
}
