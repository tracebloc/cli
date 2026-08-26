package helm

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeRunner records every shell-out and returns scripted output, so the whole
// upgrade path is exercised without a real helm/cluster/network.
type fakeRunner struct {
	calls [][]string
	// help is the output returned for `helm upgrade --help` (the reuse-flag probe).
	help string
	// fail, when set, makes the matching call return an error.
	failOn string
	// failErr overrides the error returned on a failOn match (default errBoom) —
	// e.g. a real *exec.ExitError so the interrupt classifier can be exercised.
	failErr error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	joined := strings.Join(call, " ")
	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		if f.failErr != nil {
			return "boom", f.failErr
		}
		return "boom", errBoom
	}
	if len(args) >= 2 && args[0] == "upgrade" && args[1] == "--help" {
		return f.help, nil
	}
	return "", nil
}

var errBoom = &boomErr{}

type boomErr struct{}

func (*boomErr) Error() string { return "boom" }

// install swaps the package Runner for the fake and restores it after.
func install(t *testing.T, f *fakeRunner) {
	t.Helper()
	orig := Runner
	Runner = f.run
	t.Cleanup(func() { Runner = orig })
}

func (f *fakeRunner) upgradeCall() ([]string, bool) {
	for _, c := range f.calls {
		// The mutating upgrade is `helm upgrade <release> <chart> …`, distinct
		// from the `helm upgrade --help` probe.
		if len(c) >= 2 && c[0] == "helm" && c[1] == "upgrade" && (len(c) < 3 || c[2] != "--help") {
			return c, true
		}
	}
	return nil, false
}

func baseParams() UpgradeParams {
	return UpgradeParams{
		Release:      "client-123",
		Namespace:    "tb-ns",
		KubeContext:  "my-ctx",
		Kubeconfig:   "/tmp/kc",
		ChartVersion: "1.3.5",
		Env: map[string]string{
			"RESOURCE_REQUESTS": "cpu=4,memory=16Gi",
			"RESOURCE_LIMITS":   "cpu=4,memory=16Gi",
		},
	}
}

func TestUpgrade_BuildsRightArgsAndValues(t *testing.T) {
	f := &fakeRunner{help: "…\n  --reset-then-reuse-values …\n"}
	install(t, f)

	plan, err := Upgrade(context.Background(), baseParams())
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	call, ok := f.upgradeCall()
	if !ok {
		t.Fatalf("no `helm upgrade` call recorded; calls=%v", f.calls)
	}
	joined := strings.Join(call, " ")
	for _, want := range []string{
		"upgrade client-123", "--namespace tb-ns", "--kube-context my-ctx",
		"--kubeconfig /tmp/kc", "--version 1.3.5", "--reset-then-reuse-values",
		"-f", "--wait", "--timeout 5m", ChartRef,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("upgrade args missing %q:\n%s", want, joined)
		}
	}
	// --set must NOT be used (the comma footgun the -f file exists to dodge).
	if strings.Contains(joined, "--set") {
		t.Errorf("must not use --set:\n%s", joined)
	}
	// Values file carries the env under `env:` with quoted values.
	for _, want := range []string{"env:", `RESOURCE_LIMITS: "cpu=4,memory=16Gi"`, `RESOURCE_REQUESTS: "cpu=4,memory=16Gi"`} {
		if !strings.Contains(plan.ValuesYAML, want) {
			t.Errorf("values YAML missing %q:\n%s", want, plan.ValuesYAML)
		}
	}
}

func TestUpgrade_VersionGateFallsBackToReuseValues(t *testing.T) {
	// helm --help lacks --reset-then-reuse-values → fall back to --reuse-values.
	f := &fakeRunner{help: "old helm\n  --reuse-values …\n"}
	install(t, f)
	if _, err := Upgrade(context.Background(), baseParams()); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	call, _ := f.upgradeCall()
	joined := strings.Join(call, " ")
	if strings.Contains(joined, "--reset-then-reuse-values") {
		t.Errorf("should have fallen back to --reuse-values on old helm:\n%s", joined)
	}
	if !strings.Contains(joined, "--reuse-values") {
		t.Errorf("missing --reuse-values fallback:\n%s", joined)
	}
}

func TestUpgrade_DryRunRunsNothing(t *testing.T) {
	f := &fakeRunner{help: "  --reset-then-reuse-values"}
	install(t, f)
	p := baseParams()
	p.DryRun = true

	plan, err := Upgrade(context.Background(), p)
	if err != nil {
		t.Fatalf("Upgrade dry-run: %v", err)
	}
	// The whole point: zero shell-outs.
	if len(f.calls) != 0 {
		t.Fatalf("dry-run must run NOTHING, got calls=%v", f.calls)
	}
	// But it still returns the exact command + values it WOULD run.
	if !strings.Contains(plan.Command, "helm upgrade client-123") || !strings.Contains(plan.Command, "--reset-then-reuse-values") {
		t.Errorf("dry-run plan command wrong: %s", plan.Command)
	}
	if !strings.Contains(plan.ValuesYAML, "RESOURCE_LIMITS") {
		t.Errorf("dry-run plan values missing env: %s", plan.ValuesYAML)
	}
}

func TestUpgrade_DevChartPathSkipsRepo(t *testing.T) {
	f := &fakeRunner{help: "  --reset-then-reuse-values"}
	install(t, f)
	p := baseParams()
	p.ChartPath = "/dev/chart"

	if _, err := Upgrade(context.Background(), p); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	for _, c := range f.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "repo add") || strings.Contains(joined, "repo update") {
			t.Errorf("dev chart path must skip repo add/update, saw: %s", joined)
		}
	}
	call, _ := f.upgradeCall()
	if !strings.Contains(strings.Join(call, " "), "/dev/chart") {
		t.Errorf("dev chart path not used as chart ref: %v", call)
	}
}

func TestUpgrade_AddsRepoWhenMissing(t *testing.T) {
	f := &fakeRunner{help: "  --reset-then-reuse-values"} // repo list returns "" → repo absent
	install(t, f)
	if _, err := Upgrade(context.Background(), baseParams()); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	var sawAdd, sawUpdate bool
	for _, c := range f.calls {
		joined := strings.Join(c, " ")
		sawAdd = sawAdd || strings.Contains(joined, "repo add tracebloc")
		sawUpdate = sawUpdate || strings.Contains(joined, "repo update")
	}
	if !sawAdd || !sawUpdate {
		t.Errorf("expected repo add + update when repo missing; add=%v update=%v", sawAdd, sawUpdate)
	}
}

func TestUpgrade_HelmFailureSurfaces(t *testing.T) {
	f := &fakeRunner{help: "  --reset-then-reuse-values", failOn: "upgrade client-123"}
	install(t, f)
	if _, err := Upgrade(context.Background(), baseParams()); err == nil {
		t.Fatalf("expected an error when helm upgrade fails")
	}
}

// TestUpgrade_GenuineFailureIsNotInterrupt pins the "real failure" half of the
// backend#2255 contrast: a plain helm failure on a live context (no cancel, a
// non-130 error) stays a "helm upgrade failed" error and is NEVER misclassified
// as an interrupt — the interrupt handling must not swallow real failures.
func TestUpgrade_GenuineFailureIsNotInterrupt(t *testing.T) {
	f := &fakeRunner{help: "  --reset-then-reuse-values", failOn: "upgrade client-123"}
	install(t, f)
	_, err := Upgrade(context.Background(), baseParams())
	if err == nil {
		t.Fatal("expected an error when helm upgrade fails")
	}
	if errors.Is(err, ErrInterrupted) {
		t.Errorf("a genuine failure must NOT be classified as an interrupt: %v", err)
	}
	if !strings.Contains(err.Error(), "helm upgrade failed") {
		t.Errorf("a genuine failure should read as a helm failure: %v", err)
	}
}

// TestUpgrade_CancelledContextIsInterrupt is the interrupt half of the contrast:
// a Ctrl-C / cancelled context while the mutating `helm upgrade` is in flight must
// surface as ErrInterrupted — not "helm upgrade failed" — because helm may have
// ALREADY applied the values before it was killed. The resolved Plan comes back
// too (not Plan{}) so the caller can show what was in flight (backend#2255).
func TestUpgrade_CancelledContextIsInterrupt(t *testing.T) {
	f := &fakeRunner{help: "  --reset-then-reuse-values", failOn: "upgrade client-123"}
	install(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the signal handler already propagated the cancel

	plan, err := Upgrade(ctx, baseParams())
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("a cancelled context must yield ErrInterrupted, got %v", err)
	}
	if strings.Contains(err.Error(), "helm upgrade failed") {
		t.Errorf("an interrupt must not be framed as a helm failure: %v", err)
	}
	if _, ok := f.upgradeCall(); !ok {
		t.Errorf("the upgrade should still have been attempted; calls=%v", f.calls)
	}
	// Plan is populated so the caller can surface what was in flight.
	if !strings.Contains(plan.Command, "upgrade client-123") {
		t.Errorf("interrupt must return the resolved Plan for progress display, got %+v", plan)
	}
}

// TestUpgrade_HelmExit130IsInterrupt covers the race the classifier guards: a
// terminal Ctrl-C can kill the helm child (exit 130 = 128+SIGINT) and
// CombinedOutput can return BEFORE NotifyContext flips ctx.Err(). On a LIVE
// context, only that exit code marks the interrupt.
func TestUpgrade_HelmExit130IsInterrupt(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available to synthesize an exit-130 ExitError")
	}
	// A real *exec.ExitError with code 130 — exactly what helm.Runner surfaces
	// when the helm child is killed by SIGINT.
	exit130 := exec.Command("sh", "-c", "exit 130").Run()
	if exit130 == nil {
		t.Fatal("expected a non-nil error from exit 130")
	}
	f := &fakeRunner{help: "  --reset-then-reuse-values", failOn: "upgrade client-123", failErr: exit130}
	install(t, f)

	plan, err := Upgrade(context.Background(), baseParams()) // live ctx: only the exit code says interrupt
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("helm exit 130 on a live context must be an interrupt, got %v", err)
	}
	if strings.Contains(err.Error(), "helm upgrade failed") {
		t.Errorf("an interrupt must not be framed as a helm failure: %v", err)
	}
	// Plan comes back populated so the caller can surface what was in flight.
	if !strings.Contains(plan.Command, "upgrade client-123") {
		t.Errorf("interrupt must return the resolved Plan, got %+v", plan)
	}
}

// TestUpgrade_RefusesUnpinnedRemoteChart: a remote chart ref with no
// ChartVersion must be REFUSED before any shell-out — an unpinned upgrade would
// pull the latest chart and silently change the release (FIX 1).
func TestUpgrade_RefusesUnpinnedRemoteChart(t *testing.T) {
	f := &fakeRunner{help: "  --reset-then-reuse-values"}
	install(t, f)
	p := baseParams()
	p.ChartVersion = "" // remote ref (no ChartPath) + no version = unpinned
	if _, err := Upgrade(context.Background(), p); err == nil {
		t.Fatalf("expected a refusal for a remote chart with no pinned --version")
	} else if !strings.Contains(err.Error(), "without a pinned") {
		t.Errorf("error should explain the missing pin: %v", err)
	}
	// Refuse BEFORE anything runs — not even the reuse-flag probe or repo add.
	if len(f.calls) != 0 {
		t.Errorf("must refuse before any shell-out, got calls=%v", f.calls)
	}
}

// TestUpgrade_RefusesUnpinnedEvenInDryRun: the refusal fires in --dry-run too —
// its printed plan would otherwise be the exact unsafe (unpinned) command.
func TestUpgrade_RefusesUnpinnedEvenInDryRun(t *testing.T) {
	f := &fakeRunner{help: "  --reset-then-reuse-values"}
	install(t, f)
	p := baseParams()
	p.ChartVersion = ""
	p.DryRun = true
	if _, err := Upgrade(context.Background(), p); err == nil {
		t.Fatalf("expected a refusal even in dry-run for an unpinned remote chart")
	}
}

// TestUpgrade_DevChartPathAllowsEmptyVersion: a local ChartPath (dev override)
// needs no --version pin — helm uses the on-disk chart, so the guard exempts it.
func TestUpgrade_DevChartPathAllowsEmptyVersion(t *testing.T) {
	f := &fakeRunner{help: "  --reset-then-reuse-values"}
	install(t, f)
	p := baseParams()
	p.ChartVersion = ""
	p.ChartPath = "/dev/chart"
	if _, err := Upgrade(context.Background(), p); err != nil {
		t.Fatalf("a local chart path needs no --version pin: %v", err)
	}
	if _, ok := f.upgradeCall(); !ok {
		t.Fatalf("expected the upgrade to proceed for a local chart path; calls=%v", f.calls)
	}
}

// TestRenderValues_MatchesChartEnvShape pins the rendered values to the chart's
// actual structure. tracebloc/client's jobs-manager-deployment.yaml reads each
// override at `.Values.env.<KEY>` (`hasKey .Values.env "RESOURCE_LIMITS"`, and
// likewise RESOURCE_REQUESTS / GPU_LIMITS / GPU_REQUESTS). If renderValues ever
// emitted these anywhere but under a top-level `env:` map, a `helm upgrade -f`
// would merge them to the wrong place and SILENTLY no-op on the cluster — so
// this test fails loudly on that drift. It also confirms an explicit empty
// GPU value round-trips as "" (the FIX 2 no-GPU representation), not null.
func TestRenderValues_MatchesChartEnvShape(t *testing.T) {
	in := map[string]string{
		"RESOURCE_LIMITS":   "cpu=4,memory=16Gi",
		"RESOURCE_REQUESTS": "cpu=4,memory=16Gi",
		"GPU_LIMITS":        "", // FIX 2 no-GPU value
		"GPU_REQUESTS":      "",
	}
	yml := renderValues(in)

	var nested struct {
		Env map[string]string `yaml:"env"`
	}
	if err := yaml.Unmarshal([]byte(yml), &nested); err != nil {
		t.Fatalf("rendered values is not valid YAML: %v\n%s", err, yml)
	}
	if nested.Env == nil {
		t.Fatalf("rendered values has no top-level `env:` map (chart reads .Values.env.*):\n%s", yml)
	}
	for k, want := range in {
		got, ok := nested.Env[k]
		if !ok {
			t.Errorf("env.%s missing — the chart looks it up at .Values.env.%s:\n%s", k, k, yml)
			continue
		}
		if got != want {
			t.Errorf("env.%s = %q, want %q", k, got, want)
		}
	}

	// Guard against a flattened shape (keys at the top level, no `env:` nesting),
	// which would merge to the wrong place and silently no-op.
	var flat map[string]interface{}
	if err := yaml.Unmarshal([]byte(yml), &flat); err != nil {
		t.Fatalf("re-parse failed: %v", err)
	}
	for _, k := range []string{"RESOURCE_LIMITS", "GPU_LIMITS"} {
		if _, leaked := flat[k]; leaked {
			t.Errorf("%s must live under env:, not at the top level:\n%s", k, yml)
		}
	}
}
