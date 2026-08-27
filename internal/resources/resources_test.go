package resources

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func readyNode(name, cpu, mem string, extra ...string) *corev1.Node {
	alloc := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
	if len(extra) == 2 {
		alloc[corev1.ResourceName(extra[0])] = resource.MustParse(extra[1])
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: alloc,
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

// The k3d regression (#399): server-0 + agent-0 are the SAME physical machine,
// so two identical Ready nodes must report 1× that machine, never 2×. The
// not-Ready node is bigger than both — it must not win either.
func TestMachineCapacity_LargestReadyNodeNotSum(t *testing.T) {
	notReady := readyNode("down", "16", "64Gi")
	notReady.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}

	nodes := []corev1.Node{
		*readyNode("a", "4", "16Gi", "nvidia.com/gpu", "1"),
		*readyNode("b", "4", "16Gi"),
		*notReady, // must be excluded entirely
	}
	m := MachineCapacity(nodes)
	if got := FormatCPU(m.CPU); got != "4 CPU" {
		t.Errorf("cpu = %q, want 4 CPU (largest node, not the 8-CPU sum)", got)
	}
	if got := FormatMem(m.Mem); got != "16 GiB" {
		t.Errorf("mem = %q, want 16 GiB (largest node, not the 32-GiB sum)", got)
	}
}

func TestMachineCapacity_NoReadyNodes(t *testing.T) {
	m := MachineCapacity(nil)
	if !m.CPU.IsZero() || !m.Mem.IsZero() {
		t.Errorf("empty machine should be zero, got cpu=%v mem=%v", m.CPU, m.Mem)
	}
}

func TestParseTraining_PrefersLimitsThenRequestsThenDefault(t *testing.T) {
	t.Run("limits win over requests", func(t *testing.T) {
		tr := ParseTraining(map[string]string{
			"RESOURCE_LIMITS":   "cpu=4,memory=16Gi",
			"RESOURCE_REQUESTS": "cpu=2,memory=8Gi",
		})
		if !tr.HasCPUMem || FormatCPU(tr.CPU) != "4 CPU" || FormatMem(tr.Mem) != "16 GiB" {
			t.Fatalf("=> %q %q ok=%v", FormatCPU(tr.CPU), FormatMem(tr.Mem), tr.HasCPUMem)
		}
	})
	t.Run("falls back to requests", func(t *testing.T) {
		tr := ParseTraining(map[string]string{"RESOURCE_REQUESTS": "cpu=2,memory=8Gi"})
		if FormatCPU(tr.CPU) != "2 CPU" || FormatMem(tr.Mem) != "8 GiB" {
			t.Fatalf("=> %q %q", FormatCPU(tr.CPU), FormatMem(tr.Mem))
		}
	})
	t.Run("empty env falls back to the contract floor", func(t *testing.T) {
		// backend#2254: the fallback is the contract floor (1 CPU / 2 GiB), not
		// the old cpu=2,memory=8Gi that could not schedule on a small host.
		tr := ParseTraining(map[string]string{})
		if !tr.HasCPUMem || FormatCPU(tr.CPU) != "1 CPU" || FormatMem(tr.Mem) != "2 GiB" {
			t.Fatalf("default => %q %q ok=%v", FormatCPU(tr.CPU), FormatMem(tr.Mem), tr.HasCPUMem)
		}
	})
	t.Run("unparseable env falls back to the contract floor", func(t *testing.T) {
		tr := ParseTraining(map[string]string{"RESOURCE_LIMITS": "cpu=oops"})
		if !tr.HasCPUMem || FormatCPU(tr.CPU) != "1 CPU" {
			t.Fatalf("=> %q ok=%v, want the contract floor", FormatCPU(tr.CPU), tr.HasCPUMem)
		}
	})
}

func TestParseTraining_GPU(t *testing.T) {
	tr := ParseTraining(map[string]string{
		"RESOURCE_LIMITS": "cpu=4,memory=16Gi",
		"GPU_LIMITS":      "nvidia.com/gpu=1",
	})
	if !tr.HasGPU || string(tr.GPUName) != "nvidia.com/gpu" || tr.GPU.Value() != 1 {
		t.Fatalf("gpu => %v %v has=%v", tr.GPUName, tr.GPU, tr.HasGPU)
	}
	if tr2 := ParseTraining(map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi"}); tr2.HasGPU {
		t.Errorf("cpu-only run must not report a GPU")
	}
}

func TestFormatCPU(t *testing.T) {
	cases := map[string]string{
		"4":     "4 CPU",
		"2000m": "2 CPU",
		"1200m": "1.2 CPU",
		"500m":  "0.5 CPU",
	}
	for in, want := range cases {
		if got := FormatCPU(resource.MustParse(in)); got != want {
			t.Errorf("FormatCPU(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatMem(t *testing.T) {
	cases := map[string]string{
		"8Gi":     "8 GiB",
		"16Gi":    "16 GiB",
		"32Gi":    "32 GiB",
		"1536Mi":  "1.5 GiB",
		"30720Mi": "30 GiB",
	}
	for in, want := range cases {
		if got := FormatMem(resource.MustParse(in)); got != want {
			t.Errorf("FormatMem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatGPU_Pluralization(t *testing.T) {
	if got := FormatGPU("nvidia.com/gpu", resource.MustParse("1")); got != "1 GPU" {
		t.Errorf("one gpu = %q, want 1 GPU", got)
	}
	if got := FormatGPU("nvidia.com/gpu", resource.MustParse("2")); got != "2 GPUs" {
		t.Errorf("two gpus = %q, want 2 GPUs", got)
	}
}

// --- reader (fake clientset) ---

func jmDeploy(name, instance string, env map[string]string) *appsv1.Deployment {
	var vars []corev1.EnvVar
	for k, v := range env {
		vars = append(vars, corev1.EnvVar{Name: k, Value: v})
	}
	// A valueFrom entry (no literal value) must be skipped by the reader.
	vars = append(vars, corev1.EnvVar{Name: "SECRET_REF", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{Key: "k"},
	}})
	labels := map[string]string{}
	if instance != "" {
		labels["app.kubernetes.io/instance"] = instance
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tracebloc", Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "jobs-manager", Env: vars}}},
			},
		},
	}
}

func TestJobsManagerEnv_ReleasePrefixedName(t *testing.T) {
	cs := fake.NewClientset(jmDeploy("tb-jobs-manager", "tb", map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi"}))
	env := JobsManagerEnv(context.Background(), cs, "tracebloc", "tb")
	if env["RESOURCE_LIMITS"] != "cpu=4,memory=16Gi" {
		t.Errorf("env = %v", env)
	}
	if _, leaked := env["SECRET_REF"]; leaked {
		t.Errorf("valueFrom env must be skipped, got %v", env)
	}
}

func TestJobsManagerEnv_BareNameRequiresMatchingInstanceLabel(t *testing.T) {
	// Bare "jobs-manager" belonging to a DIFFERENT release must not be read.
	cs := fake.NewClientset(jmDeploy("jobs-manager", "other", map[string]string{"RESOURCE_LIMITS": "cpu=99,memory=99Gi"}))
	if env := JobsManagerEnv(context.Background(), cs, "tracebloc", "tb"); len(env) != 0 {
		t.Errorf("must not read another release's bare deployment, got %v", env)
	}
	// Bare "jobs-manager" whose instance label matches IS read.
	cs2 := fake.NewClientset(jmDeploy("jobs-manager", "tb", map[string]string{"RESOURCE_LIMITS": "cpu=8,memory=32Gi"}))
	if env := JobsManagerEnv(context.Background(), cs2, "tracebloc", "tb"); env["RESOURCE_LIMITS"] != "cpu=8,memory=32Gi" {
		t.Errorf("matching-instance bare deployment should be read, got %v", env)
	}
}

func TestJobsManagerEnv_AbsentIsEmpty(t *testing.T) {
	cs := fake.NewClientset()
	if env := JobsManagerEnv(context.Background(), cs, "tracebloc", "tb"); len(env) != 0 {
		t.Errorf("absent deployment should yield empty env, got %v", env)
	}
}

// TestJobsManagerEnv_ReleaseUnknownUniqueMatch: with the release undiscovered
// (releaseName==""), a single "<x>-jobs-manager" is unambiguous and IS read —
// matching doctor.findDeployment's unknown-release branch (the OLD reader only
// Get()'d a bare "jobs-manager" and so missed a release-prefixed one).
func TestJobsManagerEnv_ReleaseUnknownUniqueMatch(t *testing.T) {
	cs := fake.NewClientset(jmDeploy("tb-jobs-manager", "tb", map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi"}))
	if env := JobsManagerEnv(context.Background(), cs, "tracebloc", ""); env["RESOURCE_LIMITS"] != "cpu=4,memory=16Gi" {
		t.Errorf("unique suffix match should be read when release is unknown, got %v", env)
	}
}

// TestJobsManagerEnv_ReleaseUnknownAmbiguousIsEmpty: with the release
// undiscovered and TWO releases' jobs-managers present, there's no safe
// attribution — the read must return nothing rather than pick another release's
// ceiling (the attribution guard finding #2 restores).
func TestJobsManagerEnv_ReleaseUnknownAmbiguousIsEmpty(t *testing.T) {
	cs := fake.NewClientset(
		jmDeploy("tb-jobs-manager", "tb", map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi"}),
		jmDeploy("prod-jobs-manager", "prod", map[string]string{"RESOURCE_LIMITS": "cpu=99,memory=99Gi"}),
	)
	if env := JobsManagerEnv(context.Background(), cs, "tracebloc", ""); len(env) != 0 {
		t.Errorf("ambiguous multi-release match must yield empty env, got %v", env)
	}
}

// ── the default fallback fits the smallest host we support (backend#2254) ────
//
// DefaultTraining() is what `show` reports, and mirrors the size a spawned job
// requests, when a release carries no RESOURCE_* env. It was cpu=2,memory=8Gi,
// which EXCEEDS a default Docker Desktop VM (8 GiB out of ~7.7 GiB allocatable,
// unschedulable once the 1c/3Gi overhead is reserved). It is now the contract
// floor: the largest default that still fits the smallest host preflight admits.

func TestDefaultTraining_IsTheContractFloor(t *testing.T) {
	// Drift gate: the default IS the embedded contract floor, so it and
	// client-runtime's DEFAULT_JOB_RESOURCES (the same floor, from the same
	// byte-identical contract) cannot disagree without a human keeping them in
	// sync. A floor change in envelope_contract.json flows here automatically.
	f := mustContract().Floor
	cpu, mem, ok := parseCPUMem(DefaultTraining())
	if !ok {
		t.Fatalf("DefaultTraining() did not parse: %q", DefaultTraining())
	}
	if cpu.MilliValue() != f.CPUMilli {
		t.Errorf("default cpu = %dm, contract floor = %dm", cpu.MilliValue(), f.CPUMilli)
	}
	if mem.Value() != f.MemoryBytes {
		t.Errorf("default mem = %d bytes, contract floor = %d bytes", mem.Value(), f.MemoryBytes)
	}
}

func TestDefaultTraining_FitsADefaultDockerDesktop(t *testing.T) {
	// 11 CPU / 7.7 GiB — the exact environment in backend#2254. The old
	// cpu=2,memory=8Gi did NOT fit (8Gi + 3Gi overhead > 7.7Gi); the floor fits
	// with room to spare. This is the assertion that was red before the fix.
	ddMemBytes := 7.7 * float64(1<<30) // ~7.7 GiB, as a runtime float so int64() may truncate
	dockerDesktop := Machine{
		CPU: resource.MustParse("11"),
		Mem: *resource.NewQuantity(int64(ddMemBytes), resource.BinarySI),
	}
	cpu, mem, ok := parseCPUMem(DefaultTraining())
	if !ok {
		t.Fatalf("DefaultTraining() did not parse: %q", DefaultTraining())
	}
	if !FitsNode(dockerDesktop, cpu, mem, "", resource.Quantity{}, false) {
		t.Errorf("default %q must fit a 7.7 GiB environment after the platform overhead", DefaultTraining())
	}
}

func TestDefaultTraining_FitsTheSmallestSupportedHost(t *testing.T) {
	// preflight's PF_MIN_MEM_GB floor is 5 GB; 2Gi default + 3Gi overhead == 5Gi,
	// the tight case the default must still clear.
	host := Machine{CPU: resource.MustParse("2"), Mem: resource.MustParse("5Gi")}
	cpu, mem, _ := parseCPUMem(DefaultTraining())
	if !FitsNode(host, cpu, mem, "", resource.Quantity{}, false) {
		t.Errorf("default %q must fit the smallest supported host (5 GiB)", DefaultTraining())
	}
}

func TestDefaultTraining_ReddensBelowDefaultPlusOverhead(t *testing.T) {
	// Mutation: a machine below default + overhead cannot host the default — the
	// check the 8Gi literal could never satisfy on a small box, and the guard
	// that a future default-bump past the floor reddens here.
	tiny := Machine{CPU: resource.MustParse("2"), Mem: resource.MustParse("4Gi")}
	cpu, mem, _ := parseCPUMem(DefaultTraining())
	if FitsNode(tiny, cpu, mem, "", resource.Quantity{}, false) {
		t.Errorf("default %q must NOT fit a 4 GiB machine (2Gi default + 3Gi overhead = 5Gi > 4Gi)", DefaultTraining())
	}
}
