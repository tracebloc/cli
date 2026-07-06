package cluster

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// clusterIDReadTimeout bounds the best-effort anchor read in ClusterID: a
// kubeconfig pointing at an unreachable API server would otherwise hang the
// kube-system GET for the OS TCP timeout, stalling a `client create` that is
// meant to degrade to a non-anchored mint instead.
const clusterIDReadTimeout = 8 * time.Second

// ClusterID reads the kube-system namespace UID — the stable per-cluster
// fingerprint RFC-0001 keys client idempotency on (§6.3 / §7.2; backend#883).
// It needs a reachable cluster and RBAC to GET namespaces/kube-system. Callers
// treat a failure as "couldn't read the cluster identity" and fall back to a
// non-anchored (dual-mode) provision — they must not block on it.
func ClusterID(ctx context.Context, opts KubeconfigOptions) (string, error) {
	rc, err := Load(opts)
	if err != nil {
		return "", err
	}
	// Best-effort read — callers must not block on it (see the doc above), so cap
	// it; otherwise an unreachable API server hangs the GET below.
	rc.RestConfig.Timeout = clusterIDReadTimeout
	cs, err := NewClientset(rc)
	if err != nil {
		return "", err
	}
	return clusterIDFrom(ctx, cs)
}

// DiscoverInClusterClient loads the target cluster from opts and discovers a live
// tracebloc client already installed on it (see DiscoverInClusterClientID —
// RFC-0001 §7.2 step 1, the anchor for R7 adopt-backfill). Mirrors ClusterID's
// best-effort, time-bounded read so `client create` never blocks on it: a load /
// connect failure returns the error and callers fall back to a plain create.
func DiscoverInClusterClient(ctx context.Context, opts KubeconfigOptions) (*InClusterClient, error) {
	rc, err := Load(opts)
	if err != nil {
		return nil, err
	}
	rc.RestConfig.Timeout = clusterIDReadTimeout
	cs, err := NewClientset(rc)
	if err != nil {
		return nil, err
	}
	return DiscoverInClusterClientID(ctx, cs)
}

// clusterIDFrom reads the kube-system UID from a clientset. Split out so it can be
// exercised with a fake clientset without a real cluster.
func clusterIDFrom(ctx context.Context, cs kubernetes.Interface) (string, error) {
	ns, err := cs.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading kube-system namespace UID: %w", err)
	}
	uid := string(ns.UID)
	if uid == "" {
		return "", fmt.Errorf("kube-system namespace has no UID")
	}
	return uid, nil
}
