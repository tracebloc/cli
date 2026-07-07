package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

func TestSetActiveClient_CachesNamespaceAndName(t *testing.T) {
	p := &config.Profile{}
	setActiveClient(p, &api.ProvisionedClient{ID: 7, Name: "Lab A", Namespace: "lab-a", Location: "FR"})
	if p.ActiveClientID != "7" || p.ActiveClientNamespace != "lab-a" || p.ActiveClientName != "Lab A" {
		t.Errorf("profile after setActiveClient = %+v", p)
	}
}

func TestClientStateLabel(t *testing.T) {
	cases := map[int]string{
		clientStatusOnline:  "online",
		clientStatusOffline: "offline",
		clientStatusPending: "pending",
		99:                  "unknown",
	}
	for status, want := range cases {
		if got := clientStateLabel(status); got != want {
			t.Errorf("clientStateLabel(%d) = %q, want %q", status, got, want)
		}
	}
}

// writeActiveClientConfig writes a signed-in dev profile whose active client
// carries the given namespace + name, into a temp config dir.
func writeActiveClientConfig(t *testing.T, namespace, name string) {
	t.Helper()
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	cfg := &config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "tok", ActiveClientID: "5", ActiveClientNamespace: namespace, ActiveClientName: name},
	}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestBindActiveClientNamespace_DefaultsFromActiveClient(t *testing.T) {
	writeActiveClientConfig(t, "munich-radiology", "Munich Radiology")
	opts := cluster.KubeconfigOptions{}
	b := bindActiveClientNamespace(&opts)
	if !b.applied {
		t.Fatal("binding not applied")
	}
	if opts.Namespace != "munich-radiology" {
		t.Errorf("opts.Namespace = %q, want munich-radiology", opts.Namespace)
	}
	if b.name != "Munich Radiology" || b.namespace != "munich-radiology" {
		t.Errorf("binding = %+v", b)
	}
}

func TestBindActiveClientNamespace_ExplicitOverrideWins(t *testing.T) {
	writeActiveClientConfig(t, "munich-radiology", "Munich Radiology")

	// --namespace set → don't touch it.
	optsNS := cluster.KubeconfigOptions{Namespace: "chosen"}
	if b := bindActiveClientNamespace(&optsNS); b.applied || optsNS.Namespace != "chosen" {
		t.Errorf("with --namespace: applied=%v ns=%q, want false/chosen", b.applied, optsNS.Namespace)
	}

	// --context set → user is steering the cluster; don't bind.
	optsCtx := cluster.KubeconfigOptions{Context: "some-ctx"}
	if b := bindActiveClientNamespace(&optsCtx); b.applied || optsCtx.Namespace != "" {
		t.Errorf("with --context: applied=%v ns=%q, want false/empty", b.applied, optsCtx.Namespace)
	}
}

func TestBindActiveClientNamespace_NoActiveClient(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir()) // no config written → nothing cached
	opts := cluster.KubeconfigOptions{}
	if b := bindActiveClientNamespace(&opts); b.applied || opts.Namespace != "" {
		t.Errorf("no active client: applied=%v ns=%q, want false/empty", b.applied, opts.Namespace)
	}
}

func TestActiveClientBinding_Explain(t *testing.T) {
	noRelease := &exitError{code: 4, err: &noParentReleaseError{errors.New("no release")}}
	pvcMissing := &exitError{code: 4, err: errors.New("shared PVC not bound")}

	// Applied + "no release here" → rewritten to the §7.3 guidance.
	bound := activeClientBinding{applied: true, name: "gpu-box-01", namespace: "gpu-box-01"}
	got := bound.explain(noRelease)
	if got == noRelease {
		t.Fatal("expected a rewritten error")
	}
	if !strings.Contains(got.Error(), "another machine") || !strings.Contains(got.Error(), "gpu-box-01") {
		t.Errorf("rewritten error = %q", got.Error())
	}
	var ee *exitError
	if !errors.As(got, &ee) || ee.Code() != 4 {
		t.Errorf("rewritten error should stay exit 4, got %v", got)
	}

	// Applied but a PVC failure (release WAS found) → pass through untouched.
	if bound.explain(pvcMissing) != pvcMissing {
		t.Error("PVC-missing error should not be rewritten")
	}

	// Not applied → always pass through.
	if (activeClientBinding{}).explain(noRelease) != noRelease {
		t.Error("unbound explain should pass the error through")
	}
}

// jmDep builds a chart-labeled jobs-manager Deployment in the given namespace,
// for the fallback-scan tests (mirrors the cluster package's fixture).
func jmDep(namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tracebloc-jobs-manager",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "client",
				"app.kubernetes.io/instance":   "tracebloc",
				"app.kubernetes.io/managed-by": "Helm",
				"helm.sh/chart":                "client-1.6.0",
			},
		},
	}
}

// The cluster-wide fallback scan (§7.3): a default-namespace miss must find
// the single client in its slug namespace and retarget — visibly, not
// silently — instead of dead-ending on "default".
func TestDiscoverRelease_ScanFindsSingleClientElsewhere(t *testing.T) {
	cs := fake.NewSimpleClientset(jmDep("lukas-01"))
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	release, nsUsed, err := discoverRelease(context.Background(), p, cs, "default", true)
	if err != nil {
		t.Fatalf("expected scan to find the client, got: %v", err)
	}
	if nsUsed != "lukas-01" {
		t.Errorf("nsUsed = %q, want lukas-01", nsUsed)
	}
	if release == nil || release.ReleaseName != "tracebloc" {
		t.Errorf("release = %+v", release)
	}
	// never a silent redirect
	if !strings.Contains(buf.String(), "lukas-01") {
		t.Errorf("expected a visible note about the redirect, got: %q", buf.String())
	}
}

func TestDiscoverRelease_ScanMultipleNamespacesRefuses(t *testing.T) {
	cs := fake.NewSimpleClientset(jmDep("alpha"), jmDep("beta"))
	_, _, err := discoverRelease(context.Background(), nil, cs, "default", true)
	if err == nil {
		t.Fatal("expected an error for multiple client namespaces")
	}
	for _, want := range []string{"alpha", "beta", "--namespace", "client use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %s", want, err)
		}
	}
	if !errors.Is(err, cluster.ErrNoParentRelease) {
		t.Errorf("multi-namespace refusal should stay errors.Is-identifiable, got: %v", err)
	}
}

func TestDiscoverRelease_NoScanWhenExplicit(t *testing.T) {
	// The client exists in lukas-01, but the caller pinned the namespace —
	// the scan must NOT engage and the plain discovery error stands.
	cs := fake.NewSimpleClientset(jmDep("lukas-01"))
	_, nsUsed, err := discoverRelease(context.Background(), nil, cs, "default", false)
	if err == nil {
		t.Fatal("expected the namespace miss to stand when scan is disallowed")
	}
	if nsUsed != "default" {
		t.Errorf("nsUsed = %q, want default (no retarget)", nsUsed)
	}
	if !errors.Is(err, cluster.ErrNoParentRelease) {
		t.Errorf("expected ErrNoParentRelease, got: %v", err)
	}
}

func TestDiscoverRelease_ScanFindsNothingKeepsOriginalError(t *testing.T) {
	cs := fake.NewSimpleClientset() // empty cluster
	_, _, err := discoverRelease(context.Background(), nil, cs, "default", true)
	if err == nil {
		t.Fatal("expected an error on an empty cluster")
	}
	if !errors.Is(err, cluster.ErrNoParentRelease) {
		t.Errorf("expected ErrNoParentRelease, got: %v", err)
	}
	// The no-client error must stay customer-actionable without Helm.
	if strings.Contains(err.Error(), "helm") {
		t.Errorf("error must not tell customers to run helm: %s", err)
	}
}

// The §7.5 contract at the caller level: the scan may engage ONLY when nobody
// chose the namespace — never for an explicit flag, never for a binding miss
// (which could silently retarget a different machine's client).
func TestActiveClientBinding_AllowScan(t *testing.T) {
	cases := []struct {
		name string
		b    activeClientBinding
		want bool
	}{
		{"kubeconfig default (nobody chose)", activeClientBinding{}, true},
		{"explicit --namespace/--context", activeClientBinding{explicit: true}, false},
		{"active-client binding applied", activeClientBinding{applied: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.b.allowScan(); got != c.want {
				t.Errorf("allowScan() = %v, want %v", got, c.want)
			}
		})
	}
}
