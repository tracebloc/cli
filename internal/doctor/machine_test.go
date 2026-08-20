package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The numbers this ticket was measured on: k3d v5.9.0 / k3s v1.35.5 / Docker
// 29.5.2, macOS aarch64. `docker info` reported NCPU=10 MemTotal=8321712128
// and BOTH uncapped node containers reported 8126672Ki — byte-identical to the
// VM's MemTotal (8126672 * 1024 == 8321712128).
const (
	measuredVMMem   = int64(8321712128)
	measuredVMCores = int64(10)
	measuredNodeMem = "8126672Ki"
)

// k3dNode builds a Ready k3d node container with CAPACITY set (the field the
// chain compares against the VM), mirroring what kubelet actually reports.
func k3dNode(name, cpu, mem string) *corev1.Node {
	rl := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Capacity:    rl,
			Allocatable: rl.DeepCopy(),
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func fixedVM(cores, mem int64) VMProbe {
	return func(context.Context) (int64, int64, error) { return cores, mem, nil }
}

func failingVM(msg string) VMProbe {
	return func(context.Context) (int64, int64, error) { return 0, 0, errors.New(msg) }
}

func fixedHost(cores, mem int64) HostProbe {
	return func() (int64, int64, error) { return cores, mem, nil }
}

func noHost() HostProbe {
	return func() (int64, int64, error) { return 0, 0, errors.New("unsupported") }
}

// ── the bug, at the numbers it was measured at ───────────────────────────────

func TestMachineChain_TwoUncappedNodesDoubleCountTheVM(t *testing.T) {
	// The shipped default topology: SERVERS=1 AGENTS=1, both uncapped.
	cs := fake.NewClientset(
		k3dNode("k3d-tracebloc-server-0", "10", measuredNodeMem),
		k3dNode("k3d-tracebloc-agent-0", "10", measuredNodeMem),
	)
	got := checkMachineChain(bg(), cs, fixedVM(measuredVMCores, measuredVMMem), fixedHost(10, 17179869184))

	if got.Status != StatusWarn {
		t.Fatalf("two uncapped nodes on one VM => %v (%q), want warn", got.Status, got.Detail)
	}
	// The ratio is the number that makes the lie legible, so pin it.
	if !strings.Contains(got.Detail, "2.00×") {
		t.Errorf("detail should name the 2.00x over-count, got %q", got.Detail)
	}
	// All four levels present.
	for _, want := range []string{"host 16.00 GiB", "Docker VM 7.75 GiB", "2 nodes claiming 15.50 GiB", "unrequested"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("chain missing %q, got %q", want, got.Detail)
		}
	}
	if got.Remedy == "" {
		t.Error("a warn must carry a remedy")
	}
}

func TestMachineChain_SingleNodeIsHonest(t *testing.T) {
	// One node container reporting the whole VM is not a lie — it IS the VM.
	cs := fake.NewClientset(k3dNode("k3d-tracebloc-server-0", "10", measuredNodeMem))
	got := checkMachineChain(bg(), cs, fixedVM(measuredVMCores, measuredVMMem), fixedHost(10, 17179869184))
	if got.Status != StatusOK {
		t.Fatalf("single node claiming exactly the VM => %v (%q), want ok", got.Status, got.Detail)
	}
	if strings.Contains(got.Detail, "×") {
		t.Errorf("an honest cluster should not report a ratio, got %q", got.Detail)
	}
}

func TestMachineChain_CappedNodesPassDespiteRounding(t *testing.T) {
	// MEASURED: a node capped at 3 GiB (3221225472 B) reported capacity
	// 3221225Ki, which is 2.4% ABOVE the limit it came from. Two of those sum
	// to 0.79x of this VM, so the invariant holds — but a strict `sum > vm`
	// would still have to survive that rounding, which is what the tolerance
	// is for. This is the regression guard for capping correctly.
	cs := fake.NewClientset(
		k3dNode("k3d-cap-server-0", "10", "3221225Ki"),
		k3dNode("k3d-cap-agent-0", "10", "3221225Ki"),
	)
	got := checkMachineChain(bg(), cs, fixedVM(measuredVMCores, measuredVMMem), noHost())
	if got.Status != StatusOK {
		t.Fatalf("correctly capped nodes => %v (%q), want ok", got.Status, got.Detail)
	}
}

func TestMachineChain_RoundingAloneNeverWarns(t *testing.T) {
	// A single node whose reported capacity sits just above the VM by the same
	// rounding must not be called a double-count.
	cs := fake.NewClientset(k3dNode("k3d-x-server-0", "10", fmt.Sprintf("%d", measuredVMMem+measuredVMMem/50)))
	got := checkMachineChain(bg(), cs, fixedVM(measuredVMCores, measuredVMMem), noHost())
	if got.Status != StatusOK {
		t.Fatalf("2%% over the VM is rounding, not over-commit: got %v (%q)", got.Status, got.Detail)
	}
}

func TestMachineChain_ThreeNodesReportTheirRatio(t *testing.T) {
	cs := fake.NewClientset(
		k3dNode("k3d-t-server-0", "10", measuredNodeMem),
		k3dNode("k3d-t-agent-0", "10", measuredNodeMem),
		k3dNode("k3d-t-agent-1", "10", measuredNodeMem),
	)
	got := checkMachineChain(bg(), cs, fixedVM(measuredVMCores, measuredVMMem), noHost())
	if got.Status != StatusWarn {
		t.Fatalf("three uncapped nodes => %v, want warn", got.Status)
	}
	if !strings.Contains(got.Detail, "3.00×") {
		t.Errorf("want 3.00x for three nodes, got %q", got.Detail)
	}
}

// ── the fourth level ────────────────────────────────────────────────────────

func TestMachineChain_UnrequestedSubtractsLiveRequests(t *testing.T) {
	pod := func(name string, mem string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tracebloc"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(mem)},
				},
			}}},
			Status: corev1.PodStatus{Phase: phase},
		}
	}
	cs := fake.NewClientset(
		k3dNode("k3d-t-server-0", "10", "8Gi"),
		pod("live", "2Gi", corev1.PodRunning),
		// Terminal pods hold nothing; counting them would understate free.
		pod("done", "4Gi", corev1.PodSucceeded),
		pod("dead", "4Gi", corev1.PodFailed),
	)
	got := checkMachineChain(bg(), cs, fixedVM(10, 8*(1<<30)), noHost())
	if !strings.Contains(got.Detail, "6.00 GiB unrequested") {
		t.Fatalf("8Gi capacity - 2Gi live request should leave 6.00 GiB, got %q", got.Detail)
	}
}

// ── refusing to assert what it cannot know ──────────────────────────────────

func TestMachineChain_NonK3dClusterGetsNoVerdict(t *testing.T) {
	// On EKS the nodes ARE machines and `docker info` on this laptop describes
	// something unrelated. Asserting from it would be worse than silence.
	cs := fake.NewClientset(
		k3dNode("ip-10-0-1-23.eu-central-1.compute.internal", "8", "32Gi"),
		k3dNode("ip-10-0-1-24.eu-central-1.compute.internal", "8", "32Gi"),
	)
	got := checkMachineChain(bg(), cs, fixedVM(2, 4*(1<<30)), noHost())
	if got.Status != StatusUnknown {
		t.Fatalf("non-k3d cluster => %v (%q), want unknown", got.Status, got.Detail)
	}
}

func TestMachineChain_MixedClusterGetsNoVerdict(t *testing.T) {
	cs := fake.NewClientset(
		k3dNode("k3d-t-server-0", "10", measuredNodeMem),
		k3dNode("ip-10-0-1-24.eu-central-1.compute.internal", "8", "32Gi"),
	)
	if got := checkMachineChain(bg(), cs, fixedVM(measuredVMCores, measuredVMMem), noHost()); got.Status != StatusUnknown {
		t.Fatalf("mixed cluster => %v, want unknown", got.Status)
	}
}

func TestMachineChain_UnreadableVMGetsNoVerdict(t *testing.T) {
	cs := fake.NewClientset(k3dNode("k3d-t-server-0", "10", measuredNodeMem))
	got := checkMachineChain(bg(), cs, failingVM("docker daemon not running"), noHost())
	if got.Status != StatusUnknown {
		t.Fatalf("unreadable VM => %v, want unknown (the VM is the level this check adds)", got.Status)
	}
	if got.Remedy == "" {
		t.Error("an unreadable VM should say how to make it readable")
	}
}

func TestMachineChain_NoCapacityReportedGetsNoVerdict(t *testing.T) {
	// Allocatable set, Capacity absent — "0 GiB claimed, all good" would be a
	// green with nothing behind it.
	n := k3dNode("k3d-t-server-0", "10", measuredNodeMem)
	n.Status.Capacity = nil
	cs := fake.NewClientset(n)
	if got := checkMachineChain(bg(), cs, fixedVM(measuredVMCores, measuredVMMem), noHost()); got.Status != StatusUnknown {
		t.Fatalf("no capacity => %v (%q), want unknown", got.Status, got.Detail)
	}
}

func TestMachineChain_NotReadyNodesAreNotCounted(t *testing.T) {
	notReady := k3dNode("k3d-t-agent-0", "10", measuredNodeMem)
	notReady.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	cs := fake.NewClientset(k3dNode("k3d-t-server-0", "10", measuredNodeMem), notReady)
	got := checkMachineChain(bg(), cs, fixedVM(measuredVMCores, measuredVMMem), fixedHost(10, 17179869184))
	// One Ready node claiming the VM is honest; the not-Ready one must not add
	// a phantom second claim.
	if got.Status != StatusOK {
		t.Fatalf("one Ready + one NotReady => %v (%q), want ok", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "1 node claiming") {
		t.Errorf("want singular '1 node claiming', got %q", got.Detail)
	}
}

func TestMachineChain_NoReadyNodesGetsNoVerdict(t *testing.T) {
	n := k3dNode("k3d-t-server-0", "10", measuredNodeMem)
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	cs := fake.NewClientset(n)
	if got := checkMachineChain(bg(), cs, fixedVM(measuredVMCores, measuredVMMem), noHost()); got.Status != StatusUnknown {
		t.Fatalf("no Ready node => %v, want unknown", got.Status)
	}
}

func TestMachineChain_MissingHostLevelStillReportsTheRest(t *testing.T) {
	// The host is context; the VM is the constraint. Losing the host level
	// must not cost the chain.
	cs := fake.NewClientset(k3dNode("k3d-t-server-0", "10", measuredNodeMem))
	got := checkMachineChain(bg(), cs, fixedVM(measuredVMCores, measuredVMMem), noHost())
	if got.Status != StatusOK {
		t.Fatalf("unreadable host => %v, want ok", got.Status)
	}
	if strings.Contains(got.Detail, "host ") {
		t.Errorf("host level should be omitted, not zero-filled: %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "Docker VM") {
		t.Errorf("VM level must survive: %q", got.Detail)
	}
}

// ── the probes' own parsing ─────────────────────────────────────────────────

func TestGib(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 GiB"},
		{-1, "0 GiB"},
		{1 << 30, "1.00 GiB"},
		{measuredVMMem, "7.75 GiB"},
		{17179869184, "16.00 GiB"},
	} {
		if got := gib(tc.in); got != tc.want {
			t.Errorf("gib(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPlural(t *testing.T) {
	if plural(1) != "" || plural(0) != "s" || plural(2) != "s" {
		t.Error("plural should be empty only for 1")
	}
}

func TestMachineChain_UnrequestedIsMeasuredAgainstTheVMNotTheInflatedSum(t *testing.T) {
	// Regression guard for the fourth level inheriting the third's error.
	// Two uncapped nodes claim 15.50 GiB on a 7.75 GiB VM; with 3 GiB
	// requested, the honest remainder is 4.75 GiB (VM - requests), NOT
	// 12.50 GiB (sum - requests). Caught by running this against a live edge.
	cs := fake.NewClientset(
		k3dNode("k3d-t-server-0", "10", measuredNodeMem),
		k3dNode("k3d-t-agent-0", "10", measuredNodeMem),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "hog", Namespace: "tracebloc"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3Gi")},
				},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)
	got := checkMachineChain(bg(), cs, fixedVM(measuredVMCores, measuredVMMem), noHost())
	if !strings.Contains(got.Detail, "4.75 GiB unrequested") {
		t.Fatalf("unrequested must be VM-bounded (7.75 - 3 = 4.75 GiB), got %q", got.Detail)
	}
	if strings.Contains(got.Detail, "12.50 GiB unrequested") {
		t.Error("unrequested was computed from the inflated node sum")
	}
}

func TestMachineChain_OverRequestedNeverGoesNegative(t *testing.T) {
	cs := fake.NewClientset(
		k3dNode("k3d-t-server-0", "10", "8Gi"),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "hog", Namespace: "tracebloc"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("20Gi")},
				},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)
	got := checkMachineChain(bg(), cs, fixedVM(10, 8*(1<<30)), noHost())
	if !strings.Contains(got.Detail, "0 GiB unrequested") {
		t.Fatalf("over-requested should floor at 0, got %q", got.Detail)
	}
}
