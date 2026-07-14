package cli

import (
	"context"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// TestRealProbeEnv_Discovery covers the post-discovery paths of the home-screen
// environment probe (previously 35% — realProbeEnv called cluster.Load /
// cluster.NewClientset directly, so a test could only reach the ownership-gate
// and load-failure returns; discovery was dark). Routing through the
// loadClusterFn / newClientsetFn seam lets a fake clientset drive the three
// discovered states. The ownership-gate + load-failure paths stay covered by
// TestRealProbeEnv_OwnershipGate.
func TestRealProbeEnv_Discovery(t *testing.T) {
	t.Run("live release + ready jobs-manager -> localLive with compute", func(t *testing.T) {
		writeActiveClientConfig(t, "default", "acme-01") // binding.applied ⇒ probe proceeds
		jm := jmDep("default")
		jm.Status.ReadyReplicas = 1               // Ready ⇒ live, not just present
		node := capNode("n1", "8", "32Gi", false) // Ready node
		withClusterSeams(t, fake.NewSimpleClientset(jm, &node))

		ep := realProbeEnv(context.Background())
		if ep.local != localLive {
			t.Fatalf("ready release must be localLive, got %+v", ep)
		}
		if ep.name != "tracebloc" {
			t.Errorf("release name = %q, want tracebloc (from the jobs-manager instance label)", ep.name)
		}
		if !ep.hasCompute {
			t.Error("a live environment with a Ready node must surface compute")
		}
	})

	t.Run("release present but jobs-manager not Ready -> localDegraded", func(t *testing.T) {
		writeActiveClientConfig(t, "default", "acme-01")
		withClusterSeams(t, fake.NewSimpleClientset(jmDep("default"))) // ReadyReplicas 0

		ep := realProbeEnv(context.Background())
		if ep.local != localDegraded {
			t.Fatalf("present-but-not-Ready release must be localDegraded, got %+v", ep)
		}
	})

	t.Run("reachable cluster, no release -> localNoRelease", func(t *testing.T) {
		writeActiveClientConfig(t, "default", "acme-01")
		withClusterSeams(t, fake.NewSimpleClientset()) // empty cluster

		ep := realProbeEnv(context.Background())
		if ep.local != localNoRelease {
			t.Fatalf("reachable-but-no-release must be localNoRelease, got %+v", ep)
		}
	})
}
