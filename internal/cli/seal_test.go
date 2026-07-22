package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/helm"
	"github.com/tracebloc/cli/internal/ui"
)

// stubSealTarget fakes cluster resolution for the seal check (no kubeconfig,
// no apiserver): the release "acme" in namespace "acme" — the same fake-target
// pattern the data-delete tests use.
func stubSealTarget(t *testing.T) {
	t.Helper()
	orig := resolveClusterTargetFn
	resolveClusterTargetFn = func(_ context.Context, _ *ui.Printer, _ cluster.KubeconfigOptions, _ activeClientBinding, _ bool) (*clusterTarget, error) {
		return &clusterTarget{
			Resolved: &cluster.ResolvedConfig{Context: "ctx", Namespace: "acme"},
			Release:  &cluster.ParentRelease{ReleaseName: "acme"},
		}, nil
	}
	t.Cleanup(func() { resolveClusterTargetFn = orig })
}

// stubSealHelm fakes the two helm seams: the hook listing and the per-check
// run. run receives the hook name and reports that check's outcome; it also
// records every (target, hook) invocation for order/argv assertions.
type stubSealHelm struct {
	hooks    []helm.TestHook
	listErr  error
	listGot  helm.TestTarget
	runs     []string
	runGot   helm.TestTarget
	runOut   map[string]string
	runErr   map[string]error
	timeouts []time.Duration
}

func (s *stubSealHelm) install(t *testing.T) {
	t.Helper()
	origList, origRun := listTestHooksFn, runHelmTestFn
	listTestHooksFn = func(_ context.Context, tt helm.TestTarget) ([]helm.TestHook, error) {
		s.listGot = tt
		return s.hooks, s.listErr
	}
	runHelmTestFn = func(_ context.Context, tt helm.TestTarget, hookName string, timeout time.Duration) (string, error) {
		s.runGot = tt
		s.runs = append(s.runs, hookName)
		s.timeouts = append(s.timeouts, timeout)
		return s.runOut[hookName], s.runErr[hookName]
	}
	t.Cleanup(func() { listTestHooksFn, runHelmTestFn = origList, origRun })
}

// sealHooks is the chart's suite as the parallel chart work labels it: both
// conformance Jobs carrying tracebloc.io/seal-check=true, one with a hint.
func sealHooks() []helm.TestHook {
	return []helm.TestHook{
		{Kind: "Job", Name: "acme-egress-enforcement-check", SealCheck: true,
			SealHint: "ensure the CNI enforces egress NetworkPolicy, then re-run"},
		{Kind: "Job", Name: "acme-egress-reachability-check", SealCheck: true},
	}
}

func runSeal(t *testing.T, timeout time.Duration) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runSealCheck(context.Background(), ui.New(&out), cluster.KubeconfigOptions{
		Path: "/tmp/kc", Context: "kind-acme",
	}, timeout)
	return out.String(), err
}

// Every check passes → Sealed, exit 0, per-check ✓ lines, and helm was pointed
// at the resolved release/namespace/kubeconfig — never the ambient context.
func TestSeal_AllPass_Sealed(t *testing.T) {
	stubSealTarget(t)
	s := &stubSealHelm{hooks: sealHooks()}
	s.install(t)

	out, err := runSeal(t, 90*time.Second)
	if err != nil {
		t.Fatalf("sealed run must exit 0, got: %v", err)
	}
	for _, want := range []string{
		`Seal check — secure environment "acme"`,
		"✓ egress-enforcement-check",
		"✓ egress-reachability-check",
		"Sealed — all 2 conformance checks passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if len(s.runs) != 2 || s.runs[0] != "acme-egress-enforcement-check" || s.runs[1] != "acme-egress-reachability-check" {
		t.Errorf("helm test runs = %v", s.runs)
	}
	wantTT := helm.TestTarget{Release: "acme", Namespace: "acme", Kubeconfig: "/tmp/kc", KubeContext: "kind-acme"}
	if s.listGot != wantTT || s.runGot != wantTT {
		t.Errorf("helm targets = list %+v run %+v, want %+v", s.listGot, s.runGot, wantTT)
	}
	if s.timeouts[0] != 90*time.Second {
		t.Errorf("per-check timeout = %v, want 90s", s.timeouts[0])
	}
}

// One check fails → Unsealed (exit 2, silent — the verdict was already
// rendered), the OTHER check still runs (no first-failure abort), the failure
// line carries helm's distilled reason, and the hint is the chart's seal-hint.
func TestSeal_OneFails_Unsealed_AllChecksStillRun(t *testing.T) {
	stubSealTarget(t)
	s := &stubSealHelm{
		hooks: sealHooks(),
		runOut: map[string]string{
			"acme-egress-enforcement-check": "NAME: acme\nError: 1 error occurred:\n\t* job failed: BackoffLimitExceeded\n",
		},
		runErr: map[string]error{"acme-egress-enforcement-check": errors.New("exit status 1")},
	}
	s.install(t)

	out, err := runSeal(t, 0)
	if got := ExitCodeFromError(err); got != exitChecksFailed {
		t.Fatalf("exit code = %d, want %d", got, exitChecksFailed)
	}
	if !IsSilentError(err) {
		t.Fatalf("verdict already rendered — the exit error must be silent, got: %v", err)
	}
	for _, want := range []string{
		"✗ egress-enforcement-check — job failed: BackoffLimitExceeded",
		"ensure the CNI enforces egress NetworkPolicy, then re-run",
		"✓ egress-reachability-check", // the suite kept going past the failure
		"Unsealed — 1 of 2 conformance checks failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Sealed — all") {
		t.Errorf("a failed suite must never print the sealed verdict:\n%s", out)
	}
	if len(s.runs) != 2 {
		t.Errorf("both checks must run despite the first failing, ran: %v", s.runs)
	}
}

// A failing check with no chart hint falls back to the kubectl-logs pointer
// (job/<name> for a Job hook), so the failure is still actionable.
func TestSeal_FailureHintFallsBackToKubectlLogs(t *testing.T) {
	stubSealTarget(t)
	s := &stubSealHelm{
		hooks:  []helm.TestHook{{Kind: "Job", Name: "acme-egress-reachability-check", SealCheck: true}},
		runErr: map[string]error{"acme-egress-reachability-check": errors.New("exit status 1")},
	}
	s.install(t)

	out, _ := runSeal(t, 0)
	if want := "see why: kubectl logs -n acme job/acme-egress-reachability-check"; !strings.Contains(out, want) {
		t.Errorf("output missing the kubectl-logs hint %q:\n%s", want, out)
	}
}

// No test hooks at all → honestly UNKNOWN: exit 2 (a script must not read
// "couldn't verify" as sealed), the word "sealed" only in the disclaimer.
func TestSeal_NoChecks_Unknown(t *testing.T) {
	stubSealTarget(t)
	s := &stubSealHelm{}
	s.install(t)

	out, err := runSeal(t, 0)
	if got := ExitCodeFromError(err); got != exitChecksFailed {
		t.Fatalf("exit code = %d, want %d", got, exitChecksFailed)
	}
	if !IsSilentError(err) {
		t.Fatalf("unknown verdict is rendered, not errored — got: %v", err)
	}
	if !strings.Contains(out, "Seal unknown — this chart ships no conformance checks") {
		t.Errorf("output missing the unknown verdict:\n%s", out)
	}
	if strings.Contains(out, "Sealed — all") {
		t.Errorf("an unverifiable environment must never be claimed sealed:\n%s", out)
	}
	if len(s.runs) != 0 {
		t.Errorf("nothing to run, but helm test ran: %v", s.runs)
	}
}

// An older chart with test hooks but no seal labels → run ALL of its tests and
// say so (the fallback note), rather than claiming unknown while checks exist.
func TestSeal_UnlabeledHooks_FallbackRunsAll(t *testing.T) {
	stubSealTarget(t)
	s := &stubSealHelm{hooks: []helm.TestHook{
		{Kind: "Job", Name: "acme-egress-enforcement-check"},
		{Kind: "Job", Name: "acme-egress-reachability-check"},
	}}
	s.install(t)

	out, err := runSeal(t, 0)
	if err != nil {
		t.Fatalf("passing fallback suite must exit 0, got: %v", err)
	}
	if !strings.Contains(out, "doesn't mark a seal-check suite yet — running all of its checks") {
		t.Errorf("output missing the fallback note:\n%s", out)
	}
	if len(s.runs) != 2 {
		t.Errorf("fallback must run every test hook, ran: %v", s.runs)
	}
}

// When the chart labels a seal suite, ONLY the labelled checks run — unlabeled
// helper tests aren't part of the conformance verdict — and no fallback note.
func TestSeal_LabeledSubsetOnly(t *testing.T) {
	stubSealTarget(t)
	s := &stubSealHelm{hooks: []helm.TestHook{
		{Kind: "Job", Name: "acme-egress-enforcement-check", SealCheck: true},
		{Kind: "Job", Name: "acme-smoke-helper"},
	}}
	s.install(t)

	out, err := runSeal(t, 0)
	if err != nil {
		t.Fatalf("want exit 0, got: %v", err)
	}
	if len(s.runs) != 1 || s.runs[0] != "acme-egress-enforcement-check" {
		t.Errorf("only the labelled check should run, ran: %v", s.runs)
	}
	if strings.Contains(out, "running all of its checks") {
		t.Errorf("fallback note must not print when the chart labels a suite:\n%s", out)
	}
	if !strings.Contains(out, "Sealed — the conformance check passed") {
		t.Errorf("verdict must count only the seal suite (singular wording for one check):\n%s", out)
	}
}

// The chart's seal-name annotation overrides the de-prefixed hook name.
func TestSeal_SealNameAnnotationWins(t *testing.T) {
	stubSealTarget(t)
	s := &stubSealHelm{hooks: []helm.TestHook{
		{Kind: "Job", Name: "acme-egress-enforcement-check", SealCheck: true, SealName: "egress lockdown enforced"},
	}}
	s.install(t)

	out, _ := runSeal(t, 0)
	if !strings.Contains(out, "✓ egress lockdown enforced") {
		t.Errorf("output missing the annotated check name:\n%s", out)
	}
}

// Hook enumeration failing is an ERROR (exit 1 with the cause), never a
// verdict: we don't know the environment's state, so we say neither sealed,
// unsealed, nor unknown.
func TestSeal_HookListError_NoVerdict(t *testing.T) {
	stubSealTarget(t)
	s := &stubSealHelm{listErr: errors.New("helm get hooks acme: exit status 1\nKubernetes cluster unreachable")}
	s.install(t)

	out, err := runSeal(t, 0)
	if err == nil || !strings.Contains(err.Error(), "couldn't read the chart's conformance checks") {
		t.Fatalf("want the enumeration failure surfaced, got: %v", err)
	}
	if got := ExitCodeFromError(err); got != exitFailure {
		t.Errorf("exit code = %d, want %d", got, exitFailure)
	}
	for _, verdict := range []string{"Sealed", "Unsealed", "Seal unknown"} {
		if strings.Contains(out, verdict) {
			t.Errorf("no verdict may render when the checks couldn't be read; got %q in:\n%s", verdict, out)
		}
	}
}

// A cluster-resolution failure propagates with its own exit contract (here the
// §7.3 binding-miss rewrite path returns exit 4) — the seal check adds nothing.
func TestSeal_ResolveErrorPropagates(t *testing.T) {
	orig := resolveClusterTargetFn
	resolveClusterTargetFn = func(_ context.Context, _ *ui.Printer, _ cluster.KubeconfigOptions, _ activeClientBinding, _ bool) (*clusterTarget, error) {
		return nil, &exitError{code: exitNoWorkspace, err: errors.New("no tracebloc client found in namespace \"acme\"")}
	}
	t.Cleanup(func() { resolveClusterTargetFn = orig })
	s := &stubSealHelm{}
	s.install(t)

	_, err := runSeal(t, 0)
	if got := ExitCodeFromError(err); got != exitNoWorkspace {
		t.Fatalf("exit code = %d, want %d (%v)", got, exitNoWorkspace, err)
	}
}

// Ctrl-C mid-suite exits quietly (130) with NO verdict: the remaining checks
// didn't fail — they never ran — and a fake Unsealed would misstate the
// environment.
func TestSeal_CancelledMidSuite_NoVerdict(t *testing.T) {
	stubSealTarget(t)
	ctx, cancel := context.WithCancel(context.Background())
	origList, origRun := listTestHooksFn, runHelmTestFn
	listTestHooksFn = func(_ context.Context, _ helm.TestTarget) ([]helm.TestHook, error) {
		return sealHooks(), nil
	}
	ran := 0
	runHelmTestFn = func(_ context.Context, _ helm.TestTarget, _ string, _ time.Duration) (string, error) {
		ran++
		cancel() // the user hits Ctrl-C while the first check runs
		return "", errors.New("context canceled")
	}
	t.Cleanup(func() { listTestHooksFn, runHelmTestFn = origList, origRun })

	var out bytes.Buffer
	err := runSealCheck(ctx, ui.New(&out), cluster.KubeconfigOptions{}, 0)
	if got := ExitCodeFromError(err); got != exitInterrupted {
		t.Fatalf("exit code = %d, want %d (%v)", got, exitInterrupted, err)
	}
	if !IsSilentError(err) {
		t.Fatalf("Ctrl-C must exit quietly, got: %v", err)
	}
	if ran != 1 {
		t.Errorf("no further checks may run after cancellation, ran %d", ran)
	}
	for _, verdict := range []string{"Sealed", "Unsealed", "Seal unknown"} {
		if strings.Contains(out.String(), verdict) {
			t.Errorf("no verdict may render on a cancelled run; got %q in:\n%s", verdict, out.String())
		}
	}
}

// helmFailureDetail distills helm's combined output into the one-line reason:
// the specific hook bullet first, then the Error: line, then the raw error —
// and never an empty string for a failure.
func TestHelmFailureDetail(t *testing.T) {
	cases := []struct {
		name, out string
		err       error
		want      string
	}{
		{"bullet wins", "Error: 1 error occurred:\n\t* job failed: BackoffLimitExceeded\n", errors.New("exit status 1"), "job failed: BackoffLimitExceeded"},
		{"error line when no bullet", "Error: timed out waiting for the condition\n", errors.New("exit status 1"), "timed out waiting for the condition"},
		{"raw error fallback", "", errors.New("context deadline exceeded\nmore"), "context deadline exceeded"},
		{"nil error is not a failure", "whatever", nil, ""},
	}
	for _, c := range cases {
		if got := helmFailureDetail(c.out, c.err); got != c.want {
			t.Errorf("%s: helmFailureDetail = %q, want %q", c.name, got, c.want)
		}
	}
	if got := helmFailureDetail(strings.Repeat("* x", 1)+strings.Repeat("y", 300), errors.New("x")); len([]rune(got)) > 140 {
		t.Errorf("detail not capped to one readable line: %d runes", len([]rune(got)))
	}
}

// Cobra-level flag guards: the modes are mutually exclusive, and flags that
// only one mode consumes are rejected without it instead of silently ignored.
func TestSeal_FlagGuards(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"client", "status", "--wait", "--seal"}, "--wait and --seal are separate modes"},
		{[]string{"client", "status", "--timeout", "5s"}, "--timeout has no effect without --wait or --seal"},
		{[]string{"client", "status", "--namespace", "acme"}, "--namespace has no effect without --seal"},
		{[]string{"client", "status", "--kubeconfig", "/tmp/kc"}, "--kubeconfig has no effect without --seal"},
		{[]string{"client", "status", "--context", "kind"}, "--context has no effect without --seal"},
	}
	for _, c := range cases {
		_, err := runCmd(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
}

// `client status --seal --timeout` is a valid pair (the per-check budget) and
// must reach the seal path, not the --timeout guard.
func TestSeal_TimeoutWithSealAccepted(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	stubSealTarget(t)
	s := &stubSealHelm{hooks: sealHooks()}
	s.install(t)

	if _, err := runCmd(t, "client", "status", "--seal", "--timeout", "45s"); err != nil {
		t.Fatalf("--seal --timeout: %v", err)
	}
	if len(s.timeouts) == 0 || s.timeouts[0] != 45*time.Second {
		t.Errorf("per-check timeouts = %v, want 45s", s.timeouts)
	}
}
