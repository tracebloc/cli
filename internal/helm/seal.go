// Seal-check plumbing — the helm side of `tracebloc client status --seal`
// (cli#393, RFC-0003 §8.2 D12).
//
// THE CONTRACT WITH THE CHART. The chart's conformance checks are ordinary
// `helm test` hooks (today: the egress-enforcement probe and the
// backend-reachability check; backend#1184 grows the suite). The consolidated
// suite members carry the SealCheckLabel; two optional annotations refine how
// the CLI presents a check. Everything degrades gracefully against an older
// chart: no labelled hooks → the CLI falls back to ALL of the chart's helm
// tests; no test hooks at all → the CLI reports the seal as unverifiable
// (never silently sealed — the chart's own design stance).
//
// Discovery reads `helm get hooks` (the release's stored hook manifests), so
// the names fed to `helm test --filter name=…` come from the same release
// store the test action reads — they can't drift apart.
//
// (The package comment lives in upgrade.go.)

package helm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// SealCheckLabel marks a helm-test hook as part of the chart's seal-check
	// (conformance) suite: `tracebloc.io/seal-check: "true"`.
	SealCheckLabel = "tracebloc.io/seal-check"
	// SealNameAnnotation optionally overrides the display name of a check.
	SealNameAnnotation = "tracebloc.io/seal-name"
	// SealHintAnnotation optionally carries a one-line remediation hint the CLI
	// shows when that check fails.
	SealHintAnnotation = "tracebloc.io/seal-hint"
)

// TestTarget identifies the release the helm-test commands act on. Kubeconfig
// and KubeContext pin helm to the SAME cluster the CLI resolved — never the
// ambient current-context — mirroring nodeboot.UninstallChart; both are
// appended only when non-empty.
type TestTarget struct {
	Release     string // helm release name (from cluster discovery)
	Namespace   string // release namespace
	Kubeconfig  string // kubeconfig path (empty = ambient $KUBECONFIG/~/.kube/config)
	KubeContext string // kubeconfig context (empty = current-context)
}

// kubeFlags renders the optional cluster-pinning flags helm accepts on every
// subcommand this package drives.
func (t TestTarget) kubeFlags() []string {
	var f []string
	if t.Kubeconfig != "" {
		f = append(f, "--kubeconfig", t.Kubeconfig)
	}
	if t.KubeContext != "" {
		f = append(f, "--kube-context", t.KubeContext)
	}
	return f
}

// TestHook describes one helm-test hook rendered in the release's chart.
type TestHook struct {
	Kind      string // manifest kind (Job / Pod) — drives the kubectl-logs pointer on failure
	Name      string // hook resource name; what `helm test --filter name=…` matches
	SealCheck bool   // labelled tracebloc.io/seal-check="true" (the consolidated suite)
	SealName  string // tracebloc.io/seal-name annotation ("" when absent)
	SealHint  string // tracebloc.io/seal-hint annotation ("" when absent)
}

// ListTestHooks enumerates the release's helm-test hooks from its stored hook
// manifests (`helm get hooks`). Only hooks whose `helm.sh/hook` annotation
// declares the test event are returned; install/upgrade/delete hooks are not
// conformance checks. A release with no hooks yields an empty slice, nil error.
func ListTestHooks(ctx context.Context, t TestTarget) ([]TestHook, error) {
	args := append([]string{"get", "hooks", t.Release, "--namespace", t.Namespace}, t.kubeFlags()...)
	out, err := Runner(ctx, "helm", args...)
	if err != nil {
		// Wrap with helm's own output (e.g. "release: not found", cluster
		// unreachable) — that text is the actionable part, not the exit status.
		return nil, fmt.Errorf("helm get hooks %s: %w\n%s", t.Release, err, strings.TrimSpace(out))
	}
	return parseTestHooks(out)
}

// hookManifest is the minimal slice of a rendered hook manifest the seal check
// reads. Everything else in the document is ignored.
type hookManifest struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

// parseTestHooks decodes the multi-document YAML stream `helm get hooks`
// prints and keeps the test hooks. A document that fails to decode is a hard
// error, not a skip: a manifest we can't parse could be a conformance check,
// and silently dropping it from the verdict would overstate the seal.
func parseTestHooks(manifests string) ([]TestHook, error) {
	dec := yaml.NewDecoder(strings.NewReader(manifests))
	var hooks []TestHook
	for {
		var m hookManifest
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return hooks, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parsing the release's hook manifests: %w", err)
		}
		// Empty documents (comment-only separators) decode to a zero value.
		if m.Metadata.Name == "" || !isTestHook(m.Metadata.Annotations["helm.sh/hook"]) {
			continue
		}
		hooks = append(hooks, TestHook{
			Kind:      m.Kind,
			Name:      m.Metadata.Name,
			SealCheck: m.Metadata.Labels[SealCheckLabel] == "true",
			SealName:  m.Metadata.Annotations[SealNameAnnotation],
			SealHint:  m.Metadata.Annotations[SealHintAnnotation],
		})
	}
}

// isTestHook reports whether a `helm.sh/hook` annotation value declares the
// test event. The value is a comma-separated event list; "test-success" is the
// legacy helm-2 spelling helm 3 still runs as a test.
func isTestHook(annotation string) bool {
	for _, event := range strings.Split(annotation, ",") {
		switch strings.TrimSpace(event) {
		case "test", "test-success":
			return true
		}
	}
	return false
}

// RunTest runs ONE of the release's test hooks (`helm test --filter name=…`)
// and returns helm's combined output plus its raw error. The caller derives
// the per-check verdict from the error (exit code is the pass/fail contract)
// and the failure detail from the output — so neither is wrapped or trimmed
// here.
//
// One invocation per check, not one `helm test` for the whole suite, is
// deliberate: helm stops a suite at the first failure, and the seal check must
// report EVERY check's state (the partially-degraded picture), not just the
// first break.
func RunTest(ctx context.Context, t TestTarget, hookName string, timeout time.Duration) (string, error) {
	args := []string{"test", t.Release, "--namespace", t.Namespace, "--filter", "name=" + hookName}
	if timeout > 0 {
		args = append(args, "--timeout", timeout.String())
	}
	args = append(args, t.kubeFlags()...)
	return Runner(ctx, "helm", args...)
}
