package cli

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
)

func hpDeploy(ns, name string, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func hpSaveProfile(t *testing.T, p *config.Profile) {
	t.Helper()
	cfg := &config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{"dev": p}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

// TestJobsManagerReady pins home.go:548 (0%): Ready iff the jobs-manager
// Deployment — the prefixed <release>-jobs-manager, or the unprefixed legacy
// name — has >=1 ready replica.
func TestJobsManagerReady(t *testing.T) {
	ctx := context.Background()
	rel := &cluster.ParentRelease{ReleaseName: "tracebloc"}

	t.Run("prefixed + ready -> true", func(t *testing.T) {
		cs := fake.NewSimpleClientset(hpDeploy("ns", "tracebloc-jobs-manager", 1))
		if !jobsManagerReady(ctx, cs, "ns", rel) {
			t.Error("a ready prefixed jobs-manager must be ready")
		}
	})
	t.Run("prefixed + zero replicas -> false", func(t *testing.T) {
		cs := fake.NewSimpleClientset(hpDeploy("ns", "tracebloc-jobs-manager", 0))
		if jobsManagerReady(ctx, cs, "ns", rel) {
			t.Error("zero ready replicas must not be ready")
		}
	})
	t.Run("unprefixed legacy fallback -> true", func(t *testing.T) {
		cs := fake.NewSimpleClientset(hpDeploy("ns", "jobs-manager", 1))
		if !jobsManagerReady(ctx, cs, "ns", &cluster.ParentRelease{ReleaseName: "other"}) {
			t.Error("the unprefixed jobs-manager fallback must be found")
		}
	})
	t.Run("absent -> false", func(t *testing.T) {
		if jobsManagerReady(ctx, fake.NewSimpleClientset(), "ns", rel) {
			t.Error("no deployment must be not-ready")
		}
	})
	t.Run("nil release still checks the unprefixed name", func(t *testing.T) {
		cs := fake.NewSimpleClientset(hpDeploy("ns", "jobs-manager", 1))
		if !jobsManagerReady(ctx, cs, "ns", nil) {
			t.Error("nil release must still find the unprefixed jobs-manager")
		}
	})
}

// TestMachineCapacity pins home.go:565 (0%): sum Ready nodes' allocatable, and
// return ok=false when the node list can't be read.
func TestMachineCapacity(t *testing.T) {
	ctx := context.Background()
	t.Run("ready node -> compute + ok", func(t *testing.T) {
		n := capNode("n1", "8", "32Gi", false)
		if _, ok := machineCapacity(ctx, fake.NewSimpleClientset(&n)); !ok {
			t.Error("a Ready node must yield compute")
		}
	})
	t.Run("node list error -> ok=false", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("list", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("forbidden")
		})
		if _, ok := machineCapacity(ctx, cs); ok {
			t.Error("a node-list failure must return ok=false")
		}
	})
}

// TestRealSignIn pins home.go:355 (was 60% — only the not-signed-in path): the
// signed-in success return carries the profile's email + first name.
func TestRealSignIn(t *testing.T) {
	t.Run("not signed in", func(t *testing.T) {
		t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
		if ok, _, _ := realSignIn(); ok {
			t.Error("empty config must read as not signed in")
		}
	})
	t.Run("signed in returns email + first name", func(t *testing.T) {
		t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
		hpSaveProfile(t, &config.Profile{Token: "x", Email: "alice@acme.io", FirstName: "Alice"})
		ok, email, first := realSignIn()
		if !ok || email != "alice@acme.io" || first != "Alice" {
			t.Fatalf("got (%v,%q,%q), want (true, alice@acme.io, Alice)", ok, email, first)
		}
	})
}

// TestRealRememberedClient pins home.go:431 (0%): provisioned keys on the cached
// namespace (the same signal the probe's ownership gate uses); the display name
// falls back to the client ID when no friendly name is cached.
func TestRealRememberedClient(t *testing.T) {
	t.Run("no config -> not provisioned", func(t *testing.T) {
		t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
		if prov, _ := realRememberedClient(); prov {
			t.Error("empty config must be unprovisioned")
		}
	})
	t.Run("namespace + name -> provisioned, named", func(t *testing.T) {
		t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
		hpSaveProfile(t, &config.Profile{Token: "x", ActiveClientNamespace: "acme", ActiveClientName: "acme-01"})
		prov, name := realRememberedClient()
		if !prov || name != "acme-01" {
			t.Fatalf("got (%v,%q), want (true, acme-01)", prov, name)
		}
	})
	t.Run("namespace but no name -> provisioned, ID fallback", func(t *testing.T) {
		t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
		hpSaveProfile(t, &config.Profile{Token: "x", ActiveClientNamespace: "acme", ActiveClientID: "abc-123"})
		prov, name := realRememberedClient()
		if !prov || name != "abc-123" {
			t.Fatalf("got (%v,%q), want (true, abc-123)", prov, name)
		}
	})
	t.Run("no namespace -> not provisioned", func(t *testing.T) {
		t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
		hpSaveProfile(t, &config.Profile{Token: "x", ActiveClientID: "abc-123"})
		if prov, _ := realRememberedClient(); prov {
			t.Error("no cached namespace must be unprovisioned")
		}
	})
}
