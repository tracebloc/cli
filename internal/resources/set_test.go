package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// nodes wraps readyNode pointers (defined in resources_test.go) into the value
// slice LargestReadyNode / MachineCapacity take.
func nodeList(ns ...*corev1.Node) []corev1.Node {
	out := make([]corev1.Node, 0, len(ns))
	for _, n := range ns {
		out = append(out, *n)
	}
	return out
}

// TestDeriveTraining_IdentityUnderDecisionA: the number the user sets IS the
// per-run ceiling — DeriveTraining never subtracts the overhead.
func TestDeriveTraining_IdentityUnderDecisionA(t *testing.T) {
	cpu := resource.MustParse("4")
	mem := resource.MustParse("16Gi")
	gpu := resource.MustParse("2")
	got := DeriveTraining(cpu, mem, "nvidia.com/gpu", gpu, true)
	if got.CPU.Cmp(cpu) != 0 {
		t.Errorf("CPU = %s, want identity %s (overhead must NOT be subtracted)", got.CPU.String(), cpu.String())
	}
	if got.Mem.Cmp(mem) != 0 {
		t.Errorf("Mem = %s, want identity %s", got.Mem.String(), mem.String())
	}
	if !got.HasGPU || got.GPU.Cmp(gpu) != 0 || got.GPUName != "nvidia.com/gpu" {
		t.Errorf("GPU = %+v, want identity nvidia.com/gpu=2", got)
	}

	// CPU-only: no GPU carried.
	cpuOnly := DeriveTraining(cpu, mem, "", resource.Quantity{}, false)
	if cpuOnly.HasGPU {
		t.Errorf("HasGPU = true for a CPU-only derive")
	}
}

func TestOverhead_IsOneCoreThreeGiB(t *testing.T) {
	cpu, mem := Overhead()
	if cpu.MilliValue() != 1000 {
		t.Errorf("overhead CPU = %dm, want 1000m", cpu.MilliValue())
	}
	if mem.Value() != 3*(1<<30) {
		t.Errorf("overhead mem = %d bytes, want 3 GiB", mem.Value())
	}
}

func TestLargestReadyNode(t *testing.T) {
	// Skips the NotReady node; picks the CPU-largest among Ready ones.
	notReady := readyNode("down", "64", "256Gi")
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	small := readyNode("s", "4", "16Gi")
	big := readyNode("b", "16", "64Gi", "nvidia.com/gpu", "2")

	got, ok := LargestReadyNode(nodeList(small, notReady, big))
	if !ok {
		t.Fatalf("ok = false, want a Ready node")
	}
	if got.CPU.Cmp(resource.MustParse("16")) != 0 {
		t.Errorf("picked CPU = %s, want 16 (the biggest Ready node)", got.CPU.String())
	}
	if q := got.GPU["nvidia.com/gpu"]; q.Value() != 2 {
		t.Errorf("GPU carried = %v, want 2", got.GPU)
	}

	// No Ready node at all → ok=false.
	only := readyNode("x", "8", "32Gi")
	only.Status.Conditions[0].Status = corev1.ConditionFalse
	if _, ok := LargestReadyNode(nodeList(only)); ok {
		t.Errorf("ok = true with no Ready node")
	}
}

func TestFitsNode_IncludesOverheadAndGPU(t *testing.T) {
	node, _ := LargestReadyNode(nodeList(readyNode("n", "8", "32Gi", "nvidia.com/gpu", "1")))

	// 7 cores + 1 overhead = 8 == node → fits. 8 cores + overhead = 9 > 8 → no.
	if !FitsNode(node, resource.MustParse("7"), resource.MustParse("16Gi"), "", resource.Quantity{}, false) {
		t.Errorf("7 CPU + overhead should fit an 8-core node")
	}
	if FitsNode(node, resource.MustParse("8"), resource.MustParse("16Gi"), "", resource.Quantity{}, false) {
		t.Errorf("8 CPU + 1 overhead must NOT fit an 8-core node")
	}
	// Memory: 29Gi + 3Gi overhead = 32 == node → fits; 30Gi + 3 = 33 > 32 → no.
	if !FitsNode(node, resource.MustParse("4"), resource.MustParse("29Gi"), "", resource.Quantity{}, false) {
		t.Errorf("29Gi + overhead should fit a 32Gi node")
	}
	if FitsNode(node, resource.MustParse("4"), resource.MustParse("30Gi"), "", resource.Quantity{}, false) {
		t.Errorf("30Gi + 3Gi overhead must NOT fit a 32Gi node")
	}
	// GPU: 1 requested, 1 present → fits; 2 requested → no (no overhead on GPU).
	if !FitsNode(node, resource.MustParse("4"), resource.MustParse("16Gi"), "nvidia.com/gpu", resource.MustParse("1"), true) {
		t.Errorf("1 GPU should fit a 1-GPU node")
	}
	if FitsNode(node, resource.MustParse("4"), resource.MustParse("16Gi"), "nvidia.com/gpu", resource.MustParse("2"), true) {
		t.Errorf("2 GPU must NOT fit a 1-GPU node")
	}
}

func TestMaxRunCoresAndGiB(t *testing.T) {
	node, _ := LargestReadyNode(nodeList(readyNode("n", "8", "32Gi")))
	if c := MaxRunCores(node); c != 7 {
		t.Errorf("MaxRunCores = %d, want 7 (8 − 1 overhead)", c)
	}
	if g := MaxRunGiB(node); g != 29 {
		t.Errorf("MaxRunGiB = %d, want 29 (32 − 3 overhead)", g)
	}
	// A tiny node leaves no room → 0.
	tiny, _ := LargestReadyNode(nodeList(readyNode("t", "1", "2Gi")))
	if MaxRunCores(tiny) != 0 || MaxRunGiB(tiny) != 0 {
		t.Errorf("tiny node should yield 0/0, got %d/%d", MaxRunCores(tiny), MaxRunGiB(tiny))
	}
}

func TestMachineGPU(t *testing.T) {
	node, _ := LargestReadyNode(nodeList(readyNode("n", "8", "32Gi", "nvidia.com/gpu", "4")))
	name, count, ok := MachineGPU(node)
	if !ok || count != 4 || name != "nvidia.com/gpu" {
		t.Errorf("MachineGPU = (%s,%d,%v), want (nvidia.com/gpu,4,true)", name, count, ok)
	}
	cpuOnly, _ := LargestReadyNode(nodeList(readyNode("c", "8", "32Gi")))
	if _, _, ok := MachineGPU(cpuOnly); ok {
		t.Errorf("MachineGPU ok=true on a GPU-less node")
	}
}

func TestBuildEnvSpec(t *testing.T) {
	// CPU-only: RESOURCE_* set, and GPU keys WRITTEN as the explicit no-GPU value
	// (not omitted) so --reset-then-reuse-values can't re-inherit a stale GPU.
	env := BuildEnvSpec(resource.MustParse("4"), resource.MustParse("16Gi"), "", resource.Quantity{}, false)
	if env["RESOURCE_REQUESTS"] != "cpu=4,memory=16Gi" || env["RESOURCE_LIMITS"] != "cpu=4,memory=16Gi" {
		t.Errorf("RESOURCE_* = %q / %q", env["RESOURCE_REQUESTS"], env["RESOURCE_LIMITS"])
	}
	gl, glOK := env["GPU_LIMITS"]
	gr, grOK := env["GPU_REQUESTS"]
	if !glOK || !grOK {
		t.Fatalf("GPU keys must be WRITTEN (not omitted) for a CPU-only spec so reset-then-reuse can't re-inherit a stale GPU: %v", env)
	}
	if gl != NoGPUEnvValue || gr != NoGPUEnvValue {
		t.Errorf("GPU_* = %q / %q, want the no-GPU value %q", gl, gr, NoGPUEnvValue)
	}
	// The no-GPU value must be the empty string — the ONLY value the chart's
	// jobs-manager reads as "no GPU" (client-runtime#80); "nvidia.com/gpu=0" is
	// read as GPU-present. And it must round-trip through this package's own
	// parseGPU back to HasGPU=false, so a write reads back consistently.
	if NoGPUEnvValue != "" {
		t.Errorf("NoGPUEnvValue = %q, want the empty string (chart's only no-GPU representation)", NoGPUEnvValue)
	}
	if _, _, requested := parseGPU(NoGPUEnvValue); requested {
		t.Errorf("parseGPU(%q) reported a GPU; the writer's no-GPU value must read back as no-GPU", NoGPUEnvValue)
	}

	// GPU: GPU_LIMITS/REQUESTS carried.
	genv := BuildEnvSpec(resource.MustParse("4"), resource.MustParse("16Gi"), "nvidia.com/gpu", resource.MustParse("2"), true)
	if genv["GPU_LIMITS"] != "nvidia.com/gpu=2" || genv["GPU_REQUESTS"] != "nvidia.com/gpu=2" {
		t.Errorf("GPU_* = %q / %q, want nvidia.com/gpu=2", genv["GPU_LIMITS"], genv["GPU_REQUESTS"])
	}
}

func TestParseCores(t *testing.T) {
	ok := map[string]int64{"4": 4000, "0.5": 500, "2.5": 2500}
	for in, wantMilli := range ok {
		q, err := ParseCores(in)
		if err != nil {
			t.Errorf("ParseCores(%q) err = %v", in, err)
			continue
		}
		if q.MilliValue() != wantMilli {
			t.Errorf("ParseCores(%q) = %dm, want %dm", in, q.MilliValue(), wantMilli)
		}
	}
	for _, bad := range []string{"6GB", "4Gi", "500m", "", "abc", "0", "-2"} {
		if _, err := ParseCores(bad); err == nil {
			t.Errorf("ParseCores(%q) err = nil, want a rejection", bad)
		}
	}
}

func TestParseMemoryGiB(t *testing.T) {
	// All these forms mean N GiB.
	for _, in := range []string{"16", "16Gi", "16GiB", "16G", "16GB", "16gb", "16gib"} {
		q, bare, err := ParseMemoryGiB(in)
		if err != nil {
			t.Errorf("ParseMemoryGiB(%q) err = %v", in, err)
			continue
		}
		if q.Value() != 16*(1<<30) {
			t.Errorf("ParseMemoryGiB(%q) = %d bytes, want 16 GiB", in, q.Value())
		}
		if wantBare := in == "16"; bare != wantBare {
			t.Errorf("ParseMemoryGiB(%q) bare = %v, want %v", in, bare, wantBare)
		}
	}
	// Wrong units / garbage rejected.
	for _, bad := range []string{"16Mi", "16Ti", "16m", "", "xyz", "0", "-4"} {
		if _, _, err := ParseMemoryGiB(bad); err == nil {
			t.Errorf("ParseMemoryGiB(%q) err = nil, want a rejection", bad)
		}
	}
}

func TestFloors(t *testing.T) {
	if !BelowCoreFloor(resource.MustParse("500m")) {
		t.Errorf("500m should be below the ~1 core floor")
	}
	if BelowCoreFloor(resource.MustParse("1")) {
		t.Errorf("1 core should NOT be below the floor")
	}
	if !BelowMemFloor(resource.MustParse("1Gi")) {
		t.Errorf("1Gi should be below the 2 GiB floor")
	}
	if BelowMemFloor(resource.MustParse("2Gi")) {
		t.Errorf("2Gi should NOT be below the floor")
	}
}

func TestGiBToQuantity_CanonicalRendering(t *testing.T) {
	q16 := GiBToQuantity(16)
	if s := q16.String(); s != "16Gi" {
		t.Errorf("GiBToQuantity(16).String() = %q, want 16Gi", s)
	}
	q15 := GiBToQuantity(1.5)
	if s := q15.String(); s != "1536Mi" {
		t.Errorf("GiBToQuantity(1.5).String() = %q, want 1536Mi", s)
	}
}
