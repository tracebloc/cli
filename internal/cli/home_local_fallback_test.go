package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/tracebloc/cli/internal/cluster"
)

// fallbackRelease seeds the objects DiscoverParentRelease keys on (mirrors
// internal/cluster's discovery fixtures) plus a Ready jobs-manager so the
// probe classifies localLive.
func fallbackRelease(ns string) []interface{} {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tracebloc-jobs-manager",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "client",
				"app.kubernetes.io/instance":   "tracebloc",
				"app.kubernetes.io/managed-by": "Helm",
				"app.kubernetes.io/version":    "1.9.5",
				"helm.sh/chart":                "client-1.9.5",
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, Replicas: 1},
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "jobs-manager", Namespace: ns}}
	return []interface{}{dep, svc}
}

func stubFallbackSeams(t *testing.T, serverURL string, cs kubernetes.Interface, loadErr error) {
	t.Helper()
	origLoad, origCS := loadClusterFn, newClientsetFn
	t.Cleanup(func() { loadClusterFn, newClientsetFn = origLoad, origCS })
	loadClusterFn = func(cluster.KubeconfigOptions) (*cluster.ResolvedConfig, error) {
		if loadErr != nil {
			return nil, loadErr
		}
		return &cluster.ResolvedConfig{
			Namespace:  "tracebloc",
			ServerURL:  serverURL,
			RestConfig: &rest.Config{Host: serverURL},
		}, nil
	}
	newClientsetFn = func(*cluster.ResolvedConfig) (kubernetes.Interface, error) {
		if cs == nil {
			return nil, errors.New("no clientset expected on this path")
		}
		return cs, nil
	}
}

// #401: an empty active-client pointer must not read as "no environment" when
// a tracebloc release runs on a LOCAL cluster (the pre-#388 Windows install
// state: doctor said Ready, home said run-the-installer).
func TestLocalEnvFallback_AdoptsLocalRelease(t *testing.T) {
	o := fallbackRelease("tracebloc")
	cs := fake.NewClientset(o[0].(*appsv1.Deployment), o[1].(*corev1.Service))
	stubFallbackSeams(t, "https://127.0.0.1:6550", cs, nil)
	ep := localEnvFallback(context.Background())
	if ep.local != localLive || ep.name != "tracebloc" {
		t.Fatalf("=> %+v, want localLive named tracebloc", ep)
	}
}

// The §7.5 ownership guarantee survives: a REMOTE cluster in the kubeconfig is
// never adopted without the pointer — same honest no-release as before, and
// the clientset is never even dialed.
func TestLocalEnvFallback_RemoteClusterStaysGated(t *testing.T) {
	stubFallbackSeams(t, "https://k8s.corp.example:6443", nil, nil)
	if ep := localEnvFallback(context.Background()); ep.local != localNoRelease {
		t.Fatalf("=> %+v, want localNoRelease (gate holds for remote clusters)", ep)
	}
}

func TestLocalEnvFallback_NoKubeconfigIsNoRelease(t *testing.T) {
	stubFallbackSeams(t, "", nil, errors.New("no kubeconfig"))
	if ep := localEnvFallback(context.Background()); ep.local != localNoRelease {
		t.Fatalf("=> %+v, want localNoRelease", ep)
	}
}

func TestIsLocalServerURL(t *testing.T) {
	local := []string{
		"https://127.0.0.1:6550",
		"https://localhost:6443",
		"https://[::1]:6443",
		"https://0.0.0.0:6443", // k3d wildcard bind
		"https://host.docker.internal:6550",
	}
	remote := []string{
		"https://10.2.3.4:6443",
		"https://k8s.corp.example:443",
		"https://192.168.1.20:6443",
		"", "not a url",
	}
	for _, u := range local {
		if !isLocalServerURL(u) {
			t.Errorf("isLocalServerURL(%q) = false, want true", u)
		}
	}
	for _, u := range remote {
		if isLocalServerURL(u) {
			t.Errorf("isLocalServerURL(%q) = true, want false", u)
		}
	}
}

// #401: the Windows installer writes a tb.cmd shim (symlinks need admin); the
// alias check must accept it so remedies echo `tb` on Windows too.
func TestTbCmdAliasOurs(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tracebloc.exe")
	if tbCmdAliasOurs(dir, exe) {
		t.Fatal("no shim present must be false")
	}
	shim := "@echo off\r\n\"" + exe + "\" %*\r\n"
	if err := os.WriteFile(filepath.Join(dir, "tb.cmd"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if !tbCmdAliasOurs(dir, exe) {
		t.Fatal("a shim invoking this binary must be ours")
	}
	if err := os.WriteFile(filepath.Join(dir, "tb.cmd"), []byte("@echo off\r\nsome-other-tool %*\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if tbCmdAliasOurs(dir, exe) {
		t.Fatal("a shim invoking a different tool is not ours")
	}
	// Bugbot: mentioning the name is not ownership — neither a comment that
	// says "tracebloc" nor an invocation of a DIFFERENT tracebloc install.
	if err := os.WriteFile(filepath.Join(dir, "tb.cmd"),
		[]byte("@echo off\r\nrem tracebloc helper\r\nsome-other-tool %*\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if tbCmdAliasOurs(dir, exe) {
		t.Fatal("a shim merely mentioning tracebloc is not ours")
	}
	other := filepath.Join(dir, "elsewhere", "tracebloc.exe")
	if err := os.WriteFile(filepath.Join(dir, "tb.cmd"),
		[]byte("@echo off\r\n\""+other+"\" %*\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if tbCmdAliasOurs(dir, exe) {
		t.Fatal("a shim invoking a different tracebloc install is not ours")
	}
}
