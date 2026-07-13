package cli

import (
	"errors"
	"testing"

	"github.com/AlecAivazis/survey/v2/terminal"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tracebloc/cli/internal/doctor"
	"github.com/tracebloc/cli/internal/resources"
)

// TestMapErr pins the interactive-cancel seam contract (interactive.go:82,
// previously 0%): a survey Ctrl-C (terminal.InterruptErr) becomes
// errInteractiveCancelled so callers can treat cancel as a clean exit 0; any
// other error passes through unchanged.
func TestMapErr(t *testing.T) {
	if got := mapErr(terminal.InterruptErr); !errors.Is(got, errInteractiveCancelled) {
		t.Errorf("Ctrl-C must map to errInteractiveCancelled, got %v", got)
	}
	other := errors.New("boom")
	if got := mapErr(other); got != other {
		t.Errorf("a non-interrupt error must pass through unchanged, got %v", got)
	}
	if mapErr(nil) != nil {
		t.Error("nil must stay nil")
	}
}

// TestMapClientErr pins client.go:1015 (0%): a cancelled prompt is a clean exit
// (nil); anything else becomes an exit-1 *exitError.
func TestMapClientErr(t *testing.T) {
	if err := mapClientErr(errInteractiveCancelled); err != nil {
		t.Errorf("cancel must map to a clean nil, got %v", err)
	}
	err := mapClientErr(errors.New("nope"))
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 1 {
		t.Errorf("a real error must become exit 1, got %v", err)
	}
}

// TestWorseStatus pins the doctor verdict truth-table (doctor.go:229, was 40% —
// only the Fail arm was hit). Fail dominates Warn dominates OK, order-independent.
func TestWorseStatus(t *testing.T) {
	F, W, O := doctor.StatusFail, doctor.StatusWarn, doctor.StatusOK
	cases := []struct{ a, b, want doctor.Status }{
		{O, O, O},
		{O, W, W}, {W, O, W},
		{W, W, W},
		{O, F, F}, {F, O, F},
		{W, F, F}, {F, W, F},
		{F, F, F},
	}
	for _, c := range cases {
		if got := worseStatus(c.a, c.b); got != c.want {
			t.Errorf("worseStatus(%v,%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

const gpuName = corev1.ResourceName("nvidia.com/gpu")

func gpuTraining(count string) resources.Training {
	return resources.Training{GPUName: gpuName, GPU: resource.MustParse(count), HasGPU: true}
}

// TestMachineGPUShort pins resources_set.go:494 (0%): true when the machine
// lacks the requested GPU name, or has fewer of them than the run wants.
func TestMachineGPUShort(t *testing.T) {
	has4 := resources.Machine{GPU: map[corev1.ResourceName]resource.Quantity{gpuName: resource.MustParse("4")}}
	noGPU := resources.Machine{}

	if machineGPUShort(has4, gpuTraining("2")) {
		t.Error("4 available vs 2 requested must NOT be short")
	}
	if !machineGPUShort(has4, gpuTraining("8")) {
		t.Error("4 available vs 8 requested must be short")
	}
	if !machineGPUShort(noGPU, gpuTraining("1")) {
		t.Error("a machine without the GPU name must be short")
	}
}

// TestCurrentGPUCount pins resources_set.go:603 (0%).
func TestCurrentGPUCount(t *testing.T) {
	if got := currentGPUCount(gpuTraining("3")); got != 3 {
		t.Errorf("HasGPU run must report its count, got %d", got)
	}
	if got := currentGPUCount(resources.Training{HasGPU: false}); got != 0 {
		t.Errorf("a CPU-only run must report 0 GPUs, got %d", got)
	}
}

// TestDefaultGPUChoice pins resources_set.go:612 (0%): pre-fill the "how many
// GPUs" prompt with the current in-range count, else fall back to 1.
func TestDefaultGPUChoice(t *testing.T) {
	cases := []struct {
		name    string
		current resources.Training
		machine int64
		want    int64
	}{
		{"in-range current count", gpuTraining("2"), 4, 2},
		{"CPU-only run -> 1", resources.Training{HasGPU: false}, 4, 1},
		{"current exceeds machine -> 1", gpuTraining("8"), 4, 1},
		{"current below 1 -> 1", gpuTraining("0"), 4, 1},
	}
	for _, c := range cases {
		if got := defaultGPUChoice(c.current, c.machine); got != c.want {
			t.Errorf("%s: defaultGPUChoice = %d, want %d", c.name, got, c.want)
		}
	}
}
