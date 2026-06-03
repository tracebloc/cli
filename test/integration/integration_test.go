//go:build integration

// Package integration holds CLI integration tests that run against a
// REAL Kubernetes cluster (kind in CI; any KUBECONFIG-reachable
// cluster locally). They cover the real-I/O seams the mock-based unit
// suite can't reach: kubeconfig→clientset connectivity, and — the big
// one — the SPDYExecutor tar-over-exec stream against a live Pod + PVC
// (internal/push.SPDYExecutor.Exec, 0% in unit coverage).
//
// Run:
//
//	make test-integration
//	# or: go test -tags integration ./test/integration/ -v
//
// Requires a reachable cluster with a default StorageClass and the
// ability to pull the digest-pinned alpine stage-pod image. Each test
// creates its own throwaway namespace and cleans up after itself.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
)

// loadCluster builds a clientset + rest.Config from the ambient
// kubeconfig — exercising the real cluster.Load + cluster.NewClientset
// path (NewClientset is 0% in unit coverage).
func loadCluster(t *testing.T) (kubernetes.Interface, *rest.Config) {
	t.Helper()
	resolved, err := cluster.Load(cluster.KubeconfigOptions{})
	if err != nil {
		t.Fatalf("cluster.Load (need a reachable kubeconfig): %v", err)
	}
	cs, err := cluster.NewClientset(resolved)
	if err != nil {
		t.Fatalf("cluster.NewClientset: %v", err)
	}
	return cs, resolved.RestConfig
}

// TestIntegration_Connectivity proves the kubeconfig→clientset path
// reaches a live API server.
func TestIntegration_Connectivity(t *testing.T) {
	cs, _ := loadCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nss, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(nss.Items) == 0 {
		t.Fatal("cluster reports zero namespaces — unexpected")
	}
	t.Logf("connected: %d namespaces", len(nss.Items))
}

// TestIntegration_StageAndVerify is the core integration test: it
// stages a tiny dataset onto a real PVC via the real SPDYExecutor
// tar-over-exec stream, then exec's back into the pod to prove the
// files actually landed. This is the seam (push.SPDYExecutor.Exec +
// StreamLayout) that has zero unit coverage and where every live bug
// this project hit actually lived.
func TestIntegration_StageAndVerify(t *testing.T) {
	cs, restConfig := loadCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Throwaway namespace, auto-cleaned (cascades the PVC + pod).
	ns, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "tb-it-"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	nsName := ns.Name
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		_ = cs.CoreV1().Namespaces().Delete(cctx, nsName, metav1.DeleteOptions{})
	})

	// Shared PVC (RWO + default StorageClass), mirroring the chart's
	// client-pvc that the stage pod mounts at /data/shared.
	const pvcName = "client-pvc"
	const mountPath = "/data/shared"
	_, err = cs.CoreV1().PersistentVolumeClaims(nsName).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create PVC: %v", err)
	}

	// A minimal local dataset to stage.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "labels.csv"),
		[]byte("filename,label\n001.jpg,cat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "001.jpg"),
		[]byte("\xff\xd8\xff\xe0-integration-marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout, err := push.Discover(dir)
	if err != nil {
		t.Fatalf("push.Discover: %v", err)
	}

	const table = "ittest"
	exec := &push.SPDYExecutor{Config: restConfig, Client: cs}

	// Stage pod → Ready → tar-over-exec stream.
	podName, err := push.CreateStagePod(ctx, cs, push.PodSpecOptions{
		Namespace:          nsName,
		PVCClaimName:       pvcName,
		PVCMountPath:       mountPath,
		Table:              table,
		ServiceAccountName: "default",
	})
	if err != nil {
		t.Fatalf("CreateStagePod: %v", err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dcancel()
		_ = push.DeleteStagePod(dctx, cs, nsName, podName)
	}()

	if _, err := push.WaitForStagePodReady(ctx, cs, nsName, podName); err != nil {
		t.Fatalf("WaitForStagePodReady: %v", err)
	}

	if err := push.StreamLayout(ctx, exec, nsName, podName, "stage", layout, table, push.NoOpProgress{}); err != nil {
		t.Fatalf("StreamLayout (real SPDYExecutor): %v", err)
	}

	// Exec back into the still-running pod to prove the bytes landed.
	dest := push.StagedPrefix(table)
	var stdout, stderr bytes.Buffer
	if err := exec.Exec(ctx, nsName, podName, "stage",
		[]string{"/bin/sh", "-c", fmt.Sprintf("cat %q/labels.csv; echo; ls %q/images", dest, dest)},
		nil, &stdout, &stderr); err != nil {
		t.Fatalf("verify exec: %v (stderr: %s)", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{"filename,label", "001.jpg"} {
		if !strings.Contains(got, want) {
			t.Errorf("staged content missing %q at %s; got:\n%s", want, dest, got)
		}
	}
	t.Logf("staged + verified at %s:\n%s", dest, got)
}
