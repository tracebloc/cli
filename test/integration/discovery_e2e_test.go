//go:build integration

// These scenarios exercise the real-apiserver seams the unit suite can only
// fake: cluster-wide release discovery (label-selector List against a live
// apiserver) and — the one that matters most — minting an ingestor token via
// the real TokenRequest subresource. Unit tests stub TokenRequest with a
// PrependReactor, so the modern-path token minting is only ever truly exercised
// here, against kind's apiserver (which is cluster-admin, so `create
// serviceaccounts/token` is permitted). Each test uses throwaway namespaces and
// cleans up after itself; the fixtures are label-only (replicas 0), so nothing
// schedules or pulls an image.
package integration

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/tracebloc/cli/internal/cluster"
)

// newThrowawayNS creates an auto-cleaned namespace with a generated name.
func newThrowawayNS(t *testing.T, cs kubernetes.Interface) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ns, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "tb-disc-"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	name := ns.Name
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		_ = cs.CoreV1().Namespaces().Delete(cctx, name, metav1.DeleteOptions{})
	})
	return name
}

// jobsManagerDeployment builds a chart-labeled `<release>-jobs-manager`
// Deployment — the hallmark DiscoverParentRelease keys on — with replicas 0
// (discovery reads labels + the pod-template env only; nothing needs to run).
func jobsManagerDeployment(ns, release, digest string) *appsv1.Deployment {
	replicas := int32(0)
	var env []corev1.EnvVar
	if digest != "" {
		env = append(env, corev1.EnvVar{Name: "INGESTOR_IMAGE_DIGEST", Value: digest})
	}
	sel := map[string]string{"app": release + "-jobs-manager"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release + "-jobs-manager",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "client",
				"app.kubernetes.io/instance":   release,
				"app.kubernetes.io/managed-by": "Helm",
				"helm.sh/chart":                "client-1.6.0",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: sel},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "jobs-manager", Image: "alpine:3", Env: env}}},
			},
		},
	}
}

// TestIntegration_DiscoverReleaseAndMintToken proves the real discovery +
// TokenRequest path end-to-end against a live apiserver: a chart-labeled
// jobs-manager Deployment is discovered (name, chart version, image digest), and
// a short-lived ingestor token is minted through the modern TokenRequest
// subresource — the exact call unit tests can only stub.
func TestIntegration_DiscoverReleaseAndMintToken(t *testing.T) {
	cs, _ := loadCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ns := newThrowawayNS(t, cs)

	if _, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "ingestor"}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create ingestor ServiceAccount: %v", err)
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx,
		jobsManagerDeployment(ns, "tracebloc", "sha256:deadbeef"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create jobs-manager Deployment: %v", err)
	}

	// Real label-selector discovery against the live apiserver.
	release, err := cluster.DiscoverParentRelease(ctx, cs, ns)
	if err != nil {
		t.Fatalf("DiscoverParentRelease: %v", err)
	}
	if release.ReleaseName != "tracebloc" {
		t.Errorf("ReleaseName = %q, want tracebloc", release.ReleaseName)
	}
	if release.ChartVersion != "1.6.0" {
		t.Errorf("ChartVersion = %q, want 1.6.0", release.ChartVersion)
	}
	if release.IngestorImageDigest != "sha256:deadbeef" {
		t.Errorf("IngestorImageDigest = %q, want the pod-template env value", release.IngestorImageDigest)
	}

	// The big one: mint via the REAL TokenRequest subresource.
	tok, err := cluster.MintIngestorToken(ctx, cs, ns, "ingestor", 600, nil)
	if err != nil {
		t.Fatalf("MintIngestorToken via real TokenRequest: %v", err)
	}
	if tok.Token == "" {
		t.Error("real TokenRequest returned an empty token")
	}
	if tok.Source != cluster.TokenSourceTokenRequest {
		t.Errorf("Source = %v, want TokenRequest (the modern path must work on a real apiserver)", tok.Source)
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("a real TokenRequest must stamp a server-authoritative ExpiresAt")
	}
}

// TestIntegration_FindClientNamespaces proves the cluster-wide fallback scan
// (§7.3) against a live apiserver: two throwaway namespaces each hosting a
// jobs-manager Deployment must both be found by the NamespaceAll list.
func TestIntegration_FindClientNamespaces(t *testing.T) {
	cs, _ := loadCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	nsA := newThrowawayNS(t, cs)
	nsB := newThrowawayNS(t, cs)
	for _, ns := range []string{nsA, nsB} {
		if _, err := cs.AppsV1().Deployments(ns).Create(ctx,
			jobsManagerDeployment(ns, "tracebloc", ""), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create jobs-manager in %s: %v", ns, err)
		}
	}

	found, err := cluster.FindClientNamespaces(ctx, cs)
	if err != nil {
		t.Fatalf("FindClientNamespaces (real cluster-wide list): %v", err)
	}
	set := make(map[string]bool, len(found))
	for _, n := range found {
		set[n] = true
	}
	if !set[nsA] || !set[nsB] {
		t.Errorf("scan must find both throwaway client namespaces (%s, %s); got %v", nsA, nsB, found)
	}
}
