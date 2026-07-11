package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/helm"
	"github.com/tracebloc/cli/internal/ui"
	"k8s.io/client-go/kubernetes/fake"
)

// setTarget builds a resolved cluster target from a fake clientset, with a chart
// version so the apply path can pin --version.
func setTarget(cs *fake.Clientset) *clusterTarget {
	return &clusterTarget{
		Resolved:  &cluster.ResolvedConfig{Namespace: "tracebloc", Context: "my-ctx"},
		Clientset: cs,
		Release:   &cluster.ParentRelease{ReleaseName: "tb", ChartVersion: "1.3.5"},
	}
}

// fakeHelm installs a recording helm.Runner (reset-then-reuse capable) and
// returns a pointer to the captured calls.
func fakeHelm(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	orig := helm.Runner
	helm.Runner = func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) >= 2 && args[0] == "upgrade" && args[1] == "--help" {
			return "  --reset-then-reuse-values", nil
		}
		return "", nil
	}
	t.Cleanup(func() { helm.Runner = orig })
	return &calls
}

// runSet drives applyResourcesSet against a fake cluster + prompter, returning the
// error and captured stdout.
func runSet(t *testing.T, cs *fake.Clientset, pr prompter, req setReq) (error, string) {
	t.Helper()
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), pr,
		setTarget(cs), cluster.KubeconfigOptions{Path: "/tmp/kc"}, req)
	return err, buf.String()
}

func csWith(nodeCPU, nodeMem string, env map[string]string, gpu ...string) *fake.Clientset {
	return fake.NewClientset(resNode("n1", nodeCPU, nodeMem, gpu...), resJMDeploy("tb", env))
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, not an *exitError", err)
	}
	return ee.Code()
}

// --- validation matrix ------------------------------------------------------

func TestSet_ValidationMatrix(t *testing.T) {
	cur := map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"}
	cases := []struct {
		name string
		req  setReq
		want int
	}{
		{"cores too high", setReq{cores: "20", coresSet: true, yes: true}, 2},
		{"memory too high", setReq{memory: "200", memSet: true, yes: true}, 2},
		{"below core floor", setReq{cores: "0.5", coresSet: true, yes: true}, 2},
		{"below mem floor", setReq{memory: "1", memSet: true, yes: true}, 2},
		{"zero cores", setReq{cores: "0", coresSet: true, yes: true}, 2},
		{"negative cores", setReq{cores: "-2", coresSet: true, yes: true}, 2},
		{"cores wrong unit", setReq{cores: "6GB", coresSet: true, yes: true}, 2},
		{"memory wrong unit", setReq{memory: "16Mi", memSet: true, yes: true}, 2},
		{"memory unparseable", setReq{memory: "lots", memSet: true, yes: true}, 2},
		{"gpus on no-gpu machine", setReq{gpus: 1, gpusSet: true, yes: true}, 2},
		{"max plus explicit", setReq{max: true, cores: "4", coresSet: true, yes: true}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs := csWith("8", "32Gi", cur)
			err, _ := runSet(t, cs, nil, c.req)
			if got := exitCode(t, err); got != c.want {
				t.Errorf("exit = %d, want %d", got, c.want)
			}
		})
	}
}

// TestSet_TooHighStatesRealMaxAndFix: the too-high message names the machine's
// real max and the exact fix flag.
func TestSet_TooHighStatesRealMaxAndFix(t *testing.T) {
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	err, out := runSet(t, cs, nil, setReq{cores: "20", coresSet: true, yes: true})
	if exitCode(t, err) != 2 {
		t.Fatalf("want exit 2")
	}
	// The message is on the error (non-silent), so main() prints it — assert its text.
	msg := err.Error()
	for _, want := range []string{"7 cores", "--cores 7"} {
		if !strings.Contains(msg, want) {
			t.Errorf("too-high message missing %q: %s\n%s", want, msg, out)
		}
	}
}

// TestSet_OffTTYWithoutFlags: no flags, no max, no terminal → exit 2 with a guide,
// not a usage dump.
func TestSet_OffTTYWithoutFlags(t *testing.T) {
	err := validateRequestShape(setReq{}, false)
	if exitCode(t, err) != 2 {
		t.Fatalf("want exit 2 for empty off-TTY set")
	}
	if !strings.Contains(err.Error(), "run this on a terminal") {
		t.Errorf("message should point at a terminal / flags: %s", err.Error())
	}
}

// TestSet_OffTTYWithFlagsNeedsYes: flags off a terminal without --yes → exit 1
// (mutating command needs confirmation), mirroring `delete`.
func TestSet_OffTTYWithFlagsNeedsYes(t *testing.T) {
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	err, _ := runSet(t, cs, nil, setReq{cores: "4", coresSet: true}) // yes:false, pr:nil
	if exitCode(t, err) != 1 {
		t.Fatalf("want exit 1 without --yes off a terminal, got %v", err)
	}
}

// --- apply path -------------------------------------------------------------

// TestSet_ApplyBuildsHelmArgsAndValues: an explicit CPU-only set reaches helm with
// the right args (reset-then-reuse, -f, --version, resolved ctx/ns) and the temp
// values carry RESOURCE_*.
func TestSet_ApplyBuildsHelmArgsAndValues(t *testing.T) {
	calls := fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	err, out := runSet(t, cs, nil, setReq{cores: "4", memory: "16", coresSet: true, memSet: true, yes: true})
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	var upgrade []string
	for _, c := range *calls {
		if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
			upgrade = c
		}
	}
	if upgrade == nil {
		t.Fatalf("no helm upgrade call; calls=%v", *calls)
	}
	joined := strings.Join(upgrade, " ")
	for _, want := range []string{
		"upgrade tb", "--namespace tracebloc", "--kube-context my-ctx",
		"--kubeconfig /tmp/kc", "--version 1.3.5", "--reset-then-reuse-values", "-f", "--wait",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("helm args missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(out, "up to 4 CPU · 16 GiB") {
		t.Errorf("success echo missing the new size:\n%s", out)
	}
}

// TestSet_KeepsUnsetDimension: `set --cores 4` changes CPU only and KEEPS the
// current 8Gi memory (proven via the dry-run plan's resulting values).
func TestSet_KeepsUnsetDimension(t *testing.T) {
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	err, out := runSet(t, cs, nil, setReq{cores: "4", coresSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out, "cpu=4,memory=8Gi") {
		t.Errorf("unset memory should be kept at 8Gi:\n%s", out)
	}
}

// TestSet_MaxUsesWholeMachineMinusOverhead: `set max` writes cores/mem =
// machine − overhead (7 / 29 on an 8-core/32Gi node) and includes the GPU.
func TestSet_MaxUsesWholeMachineMinusOverhead(t *testing.T) {
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), nil,
		setTarget(csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"}, "nvidia.com/gpu", "1")),
		cluster.KubeconfigOptions{}, setReq{max: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("dry-run max: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "cpu=7,memory=29Gi") {
		t.Errorf("max should be 7 CPU / 29 GiB (machine − overhead):\n%s", out)
	}
	if !strings.Contains(out, "nvidia.com/gpu=1") {
		t.Errorf("max should include the machine GPU:\n%s", out)
	}
}

// TestSet_NoOpSkipsApply: setting the ceiling to what it already is skips the
// helm upgrade entirely.
func TestSet_NoOpSkipsApply(t *testing.T) {
	calls := fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi"})
	err, out := runSet(t, cs, nil, setReq{cores: "4", memory: "16", coresSet: true, memSet: true, yes: true})
	if err != nil {
		t.Fatalf("no-op: %v", err)
	}
	for _, c := range *calls {
		if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
			t.Errorf("no-op must NOT call helm upgrade: %v", c)
		}
	}
	if !strings.Contains(out, "nothing to change") {
		t.Errorf("no-op should say nothing changed:\n%s", out)
	}
}

// TestSet_DryRunAppliesNothing: --dry-run prints the plan and never shells helm.
func TestSet_DryRunAppliesNothing(t *testing.T) {
	calls := fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	err, out := runSet(t, cs, nil, setReq{cores: "4", coresSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("dry-run must not shell helm, got %v", *calls)
	}
	for _, want := range []string{"Dry run", "helm upgrade tb", "cpu=4,memory=8Gi"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

// --- wizard -----------------------------------------------------------------

// TestWizard_PreselectedMax: on a terminal with no flags, accepting the default
// Select choice ("use as much as possible") sizes the run to machine − overhead.
func TestWizard_PreselectedMax(t *testing.T) {
	pr := &fakePrompter{confirm: boolPtr(true)} // Select returns the default (optMax)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), pr,
		setTarget(cs), cluster.KubeconfigOptions{}, setReq{dryRun: true})
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	if !strings.Contains(buf.String(), "cpu=7,memory=29Gi") {
		t.Errorf("preselected max should size to 7/29:\n%s", buf.String())
	}
}

// TestWizard_ChooseAnAmount: picking "Choose an amount" runs the bounded prompts
// and applies the typed cores/memory.
func TestWizard_ChooseAnAmount(t *testing.T) {
	pr := &fakePrompter{
		answers: map[string]string{
			"How much may one training run use?": "Choose an amount",
			"CPU cores for one run (1–7)":        "3",
			"Memory for one run in GiB (2–29)":   "12",
		},
		confirm: boolPtr(true),
	}
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), pr,
		setTarget(cs), cluster.KubeconfigOptions{}, setReq{dryRun: true})
	if err != nil {
		t.Fatalf("wizard choose: %v", err)
	}
	if !strings.Contains(buf.String(), "cpu=3,memory=12Gi") {
		t.Errorf("chosen amount should be 3/12:\n%s", buf.String())
	}
}

// TestWizard_GPURowOmittedWhenNoGPU: the wizard header shows no GPU line on a
// GPU-less machine, and the Select still preselects max.
func TestWizard_GPURowOmittedWhenNoGPU(t *testing.T) {
	pr := &fakePrompter{confirm: boolPtr(true)}
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	var buf bytes.Buffer
	if err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), pr,
		setTarget(cs), cluster.KubeconfigOptions{}, setReq{dryRun: true}); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	out := buf.String()
	// No GPU should surface in the HUMAN-facing output (the wizard header row or
	// the size echo) on a GPU-less machine. The raw values dump below legitimately
	// carries the explicit no-GPU keys (FIX 2), so scope this to the text before
	// the `values:` block.
	human := out
	if i := strings.Index(out, "values:"); i >= 0 {
		human = out[:i]
	}
	if strings.Contains(human, "GPU") {
		t.Errorf("no GPU row/echo expected in the human-facing output on a GPU-less machine:\n%s", human)
	}
	// And a phantom GPU must never be written, anywhere.
	if strings.Contains(out, "nvidia.com/gpu") {
		t.Errorf("no phantom GPU should be written on a GPU-less machine:\n%s", out)
	}
	// Show block should carry the CPU + Memory "x of N" lines.
	for _, want := range []string{"of 8 cores", "of 32 GiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("wizard header missing %q:\n%s", want, out)
		}
	}
}

// TestWizard_LeaveAsIs: "Leave it as it is" is a no-op (no helm upgrade).
func TestWizard_LeaveAsIs(t *testing.T) {
	calls := fakeHelm(t)
	pr := &fakePrompter{answers: map[string]string{"How much may one training run use?": "Leave it as it is"}}
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi"})
	err, out := runSet(t, cs, pr, setReq{})
	if err != nil {
		t.Fatalf("wizard leave: %v", err)
	}
	for _, c := range *calls {
		if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
			t.Errorf("leave-as-is must not upgrade: %v", c)
		}
	}
	if !strings.Contains(out, "nothing to change") {
		t.Errorf("leave-as-is should be a no-op:\n%s", out)
	}
}

// TestSet_CPUOnlyMachineIgnoresChartDefaultGPU: the chart stamps a default
// GPU_REQUESTS on every install (CPU boxes included); a plain `--cores` change on
// a GPU-less machine must NOT inherit that phantom GPU and fail the GPU fit-check.
// It writes the EXPLICIT no-GPU value (empty string) — never the phantom
// nvidia.com/gpu — and, since no GPU was ever configured here, prints no
// "removed" note.
func TestSet_CPUOnlyMachineIgnoresChartDefaultGPU(t *testing.T) {
	// GPU-less node, but the env carries the chart's default GPU_REQUESTS.
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS": "cpu=2,memory=8Gi",
		"GPU_REQUESTS":    "nvidia.com/gpu=1",
	})
	err, out := runSet(t, cs, nil, setReq{cores: "4", coresSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("a plain --cores change on a GPU-less box must succeed, got: %v", err)
	}
	if strings.Contains(out, "nvidia.com/gpu") {
		t.Errorf("the phantom chart-default GPU must NOT be written on a GPU-less machine:\n%s", out)
	}
	// The GPU keys are written as the explicit no-GPU value so reset-then-reuse
	// can't re-inherit the phantom.
	for _, want := range []string{`GPU_LIMITS: ""`, `GPU_REQUESTS: ""`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected explicit no-GPU value %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "removed") {
		t.Errorf("no GPU was configured, so nothing should be announced as removed:\n%s", out)
	}
	if !strings.Contains(out, "cpu=4,memory=8Gi") {
		t.Errorf("expected CPU-only spec:\n%s", out)
	}
}

// --- FIX 1: never run an unpinned upgrade -----------------------------------

// TestSet_RefusesWhenChartVersionUnknown: when the release carries no
// helm.sh/chart version label (ParentRelease.ChartVersion == ""), `set` must
// REFUSE before mutating — an unpinned `helm upgrade tracebloc/client` would
// pull the latest chart and silently bump the whole client.
func TestSet_RefusesWhenChartVersionUnknown(t *testing.T) {
	calls := fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	// Same target as setTarget, but with the chart version unknown.
	target := &clusterTarget{
		Resolved:  &cluster.ResolvedConfig{Namespace: "tracebloc", Context: "my-ctx"},
		Clientset: cs,
		Release:   &cluster.ParentRelease{ReleaseName: "tb", ChartVersion: ""},
	}
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), nil,
		target, cluster.KubeconfigOptions{Path: "/tmp/kc"}, setReq{cores: "4", coresSet: true, yes: true})
	if exitCode(t, err) != 1 {
		t.Fatalf("want exit 1 when the chart version is unknown, got %v", err)
	}
	for _, want := range []string{"chart version", "installer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message missing %q: %s", want, err.Error())
		}
	}
	// The whole point: refuse BEFORE any helm shell-out (not even the probe).
	for _, c := range *calls {
		if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
			t.Errorf("must not `helm upgrade` when the chart version is unknown: %v", c)
		}
	}
}

// --- FIX 2: removing a GPU actually removes it ------------------------------

// TestSet_RemovingGPUWritesExplicitNoGPUValue: on a GPU machine, `--gpus 0`
// must WRITE the explicit no-GPU value ("") into the plan values so
// --reset-then-reuse-values OVERRIDES the stored nvidia.com/gpu=1 — the fix for
// the silent no-op where the GPU stayed put while the echo claimed it was gone.
func TestSet_RemovingGPUWritesExplicitNoGPUValue(t *testing.T) {
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS": "cpu=2,memory=8Gi",
		"GPU_REQUESTS":    "nvidia.com/gpu=1",
		"GPU_LIMITS":      "nvidia.com/gpu=1",
	}, "nvidia.com/gpu", "1")
	err, out := runSet(t, cs, nil, setReq{gpus: 0, gpusSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("removing the GPU should succeed: %v\n%s", err, out)
	}
	for _, want := range []string{`GPU_LIMITS: ""`, `GPU_REQUESTS: ""`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected explicit no-GPU override %q:\n%s", want, out)
		}
	}
	// The stale count must be OVERRIDDEN, not left behind in the values.
	if strings.Contains(out, "nvidia.com/gpu") {
		t.Errorf("stale nvidia.com/gpu must be overridden, not left in the values:\n%s", out)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("dry-run should state the GPU is removed:\n%s", out)
	}
}

// TestSet_RemovingGPUEchoIsHonest: on the real apply path, removing the GPU
// prints an honest "GPU access removed" note — true only because the written
// values actually remove it.
func TestSet_RemovingGPUEchoIsHonest(t *testing.T) {
	fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS": "cpu=2,memory=8Gi",
		"GPU_REQUESTS":    "nvidia.com/gpu=1",
		"GPU_LIMITS":      "nvidia.com/gpu=1",
	}, "nvidia.com/gpu", "1")
	err, out := runSet(t, cs, nil, setReq{gpus: 0, gpusSet: true, yes: true})
	if err != nil {
		t.Fatalf("removing the GPU should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "GPU access removed") {
		t.Errorf("success echo should honestly announce GPU removal:\n%s", out)
	}
}

// TestSet_UntouchedGPUIsKept: a plain --cores change on a GPU machine leaves the
// GPU dimension untouched — it must be KEPT (re-written as nvidia.com/gpu=1),
// and nothing is announced as removed.
func TestSet_UntouchedGPUIsKept(t *testing.T) {
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS": "cpu=2,memory=8Gi",
		"GPU_REQUESTS":    "nvidia.com/gpu=1",
		"GPU_LIMITS":      "nvidia.com/gpu=1",
	}, "nvidia.com/gpu", "1")
	err, out := runSet(t, cs, nil, setReq{cores: "4", coresSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("cpu-only change on a GPU machine should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, `GPU_LIMITS: "nvidia.com/gpu=1"`) {
		t.Errorf("an untouched GPU must be kept, not dropped:\n%s", out)
	}
	if strings.Contains(out, "removed") {
		t.Errorf("nothing was removed — don't claim it:\n%s", out)
	}
}

func boolPtr(b bool) *bool { return &b }
