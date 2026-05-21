package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("os.UserHomeDir failed in this environment, skipping: %v", err)
	}

	cases := []struct {
		in       string
		wantFunc func(string) bool
	}{
		{
			in:       "",
			wantFunc: func(got string) bool { return got == "" },
		},
		{
			in:       "/absolute/path/to/kubeconfig",
			wantFunc: func(got string) bool { return got == "/absolute/path/to/kubeconfig" },
		},
		{
			in:       "relative/path/kubeconfig",
			wantFunc: func(got string) bool { return got == "relative/path/kubeconfig" },
		},
		{
			in: "~/.kube/config",
			wantFunc: func(got string) bool {
				want := filepath.Join(home, ".kube/config")
				return got == want
			},
		},
		{
			in: "~/some/file",
			wantFunc: func(got string) bool {
				want := filepath.Join(home, "some/file")
				return got == want
			},
		},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := expandPath(c.in)
			if !c.wantFunc(got) {
				t.Errorf("expandPath(%q) = %q (failed predicate)", c.in, got)
			}
		})
	}
}

// Load() involves real filesystem + clientcmd parsing; it's covered
// indirectly by the cluster-info integration path. A unit test
// would require writing a temp kubeconfig and feeding it back —
// useful but lower-priority than the discover/token tests since
// the clientcmd library is well-trodden. Pin the smallest contract:
// an empty Options{} doesn't panic.
func TestLoad_EmptyOptionsDoesNotPanic(t *testing.T) {
	// May succeed or fail depending on whether the test runner has
	// a kubeconfig available; either is fine. The point of this
	// test is to ensure the function returns rather than crashes.
	_, err := Load(KubeconfigOptions{})
	if err != nil && !looksLikeKubeconfigError(err.Error()) {
		// Defense-in-depth: if Load() ever starts wrapping
		// errors in a way that loses the "kubeconfig" context,
		// flag it so the customer-facing diagnostic stays clear.
		t.Logf("Load() failed with unfamiliar error: %v", err)
	}
}

func looksLikeKubeconfigError(s string) bool {
	for _, marker := range []string{"kubeconfig", "config", "context", "namespace"} {
		if strings.Contains(strings.ToLower(s), marker) {
			return true
		}
	}
	return false
}
