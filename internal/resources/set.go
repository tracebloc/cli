package resources

// set.go backs `tracebloc resources set` (cli#143 P2) — RAISING how much of this
// machine a single training run may use. It is the write-side companion to the
// read-only view in resources.go and stays just as cluster-free and pure: it
// turns the numbers the user picks into the chart env the run will be stamped
// with, and answers the "does it fit on a node" question — all against
// already-fetched Kubernetes objects, no client calls of its own.
//
// LOCKED DESIGN (issue #143, do not deviate):
//   - Decision A: the number the user sets IS the per-run ceiling written to
//     RESOURCE_* verbatim. tracebloc's fixed overhead (Overhead) is a fit-check
//     SAFETY MARGIN only — it is NEVER subtracted from what the user asked for.
//     DeriveTraining is therefore the identity on the user's numbers.
//   - GPU is first-class and whole-unit: no fractional GPUs, no overhead math on
//     the GPU dimension. A machine with zero GPUs has no GPU dimension at all.
//   - Fit-check runs against the LARGEST single Ready node (a run lands on ONE
//     node, never OR'd across nodes — mirrors doctor.checkNodeFit), with the
//     overhead added on top so a set can never leave no room for the platform.

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// overheadCPUMilli / overheadMemBytes are tracebloc's fixed platform reservation
// — the single documented constant the design calls for (~1 core / 3 GiB). It is
// used ONLY as the fit-check safety margin (see FitsNode / MaxRunCores /
// MaxRunGiB); it is never subtracted from the per-run ceiling the user sets.
const (
	overheadCPUMilli = 1000           // ~1 CPU core
	overheadMemBytes = 3 * (1 << 30)  // 3 GiB
	minRunCPUMilli   = 1000           // per-run floor: ~1 core
	minRunMemBytes   = 2 * (1 << 30)  // per-run floor: 2 GiB
	gib              = int64(1) << 30 // bytes per GiB
)

// Overhead returns tracebloc's fixed platform reservation as quantities. Kept a
// function (not exported vars) so callers can't mutate the shared value — a
// resource.Quantity's Add mutates its receiver.
func Overhead() (cpu, mem resource.Quantity) {
	return *resource.NewMilliQuantity(overheadCPUMilli, resource.DecimalSI),
		*resource.NewQuantity(overheadMemBytes, resource.BinarySI)
}

// DeriveTraining turns a chosen per-run ceiling into the Training spec written to
// the chart. Under Decision A it is the IDENTITY on the user's numbers: the
// overhead is a fit margin only and is NEVER subtracted here. (Its existence as a
// named function is deliberate — it's the one place the "we don't secretly shrink
// what you asked for" invariant lives, and the test pins it.)
func DeriveTraining(cpu, mem resource.Quantity, gpuName corev1.ResourceName, gpu resource.Quantity, wantGPU bool) Training {
	t := Training{CPU: cpu, Mem: mem, HasCPUMem: true}
	if wantGPU {
		t.GPUName, t.GPU, t.HasGPU = gpuName, gpu, true
	}
	return t
}

// LargestReadyNode returns the single biggest Ready node's capacity (CPU-major,
// then memory), and ok=false when no node is Ready. The fit question is
// single-node by nature — a training pod gets ALL its resources from ONE node —
// so `set` sizes and checks against this one node, never a sum across nodes
// (mirrors doctor.checkNodeFit; MachineCapacity's whole-machine sum is only for
// the friendly headline). On the installer's single-node path this IS that node.
func LargestReadyNode(nodes []corev1.Node) (Machine, bool) {
	var best Machine
	found := false
	for i := range nodes {
		n := nodes[i]
		if !nodeReady(n) {
			continue
		}
		m := Machine{GPU: map[corev1.ResourceName]resource.Quantity{}}
		for name, qty := range n.Status.Allocatable {
			switch name {
			case corev1.ResourceCPU:
				m.CPU = qty.DeepCopy()
			case corev1.ResourceMemory:
				m.Mem = qty.DeepCopy()
			default:
				if isGPUResource(name) && !qty.IsZero() {
					m.GPU[name] = qty.DeepCopy()
				}
			}
		}
		if !found || nodeLarger(m, best) {
			best, found = m, true
		}
	}
	return best, found
}

// nodeLarger orders two nodes CPU-major, memory as the tie-break — a total order
// so LargestReadyNode is deterministic regardless of list order.
func nodeLarger(a, b Machine) bool {
	if c := a.CPU.Cmp(b.CPU); c != 0 {
		return c > 0
	}
	return a.Mem.Cmp(b.Mem) > 0
}

// FitsNode reports whether a per-run ceiling PLUS the platform overhead fits on a
// single node. CPU and memory both carry the overhead margin; the GPU dimension
// does not (whole-unit, no overhead math). A run must satisfy every dimension on
// the SAME node — this never ORs cpu/mem against GPU across nodes.
func FitsNode(node Machine, cpu, mem resource.Quantity, gpuName corev1.ResourceName, gpu resource.Quantity, wantGPU bool) bool {
	oc, om := Overhead()
	needCPU := cpu.DeepCopy()
	needCPU.Add(oc)
	needMem := mem.DeepCopy()
	needMem.Add(om)
	if node.CPU.Cmp(needCPU) < 0 || node.Mem.Cmp(needMem) < 0 {
		return false
	}
	if wantGPU {
		have, ok := node.GPU[gpuName]
		if !ok || have.Cmp(gpu) < 0 {
			return false
		}
	}
	return true
}

// MaxRunCores is the largest whole-core per-run ceiling that still leaves room for
// the overhead on this node: floor(nodeCPU - overheadCPU). Never negative. This is
// the bound the wizard clamps the "cores" prompt to, so over-asking is impossible.
func MaxRunCores(node Machine) int {
	milli := node.CPU.MilliValue() - overheadCPUMilli
	if milli < minRunCPUMilli {
		return 0
	}
	return int(milli / 1000)
}

// MaxRunGiB is the largest whole-GiB per-run memory ceiling that still leaves room
// for the overhead: floor(nodeMem - overheadMem), in GiB. Never negative.
func MaxRunGiB(node Machine) int {
	b := node.Mem.Value() - overheadMemBytes
	if b < minRunMemBytes {
		return 0
	}
	return int(b / gib)
}

// MachineGPU returns the machine's GPU device name and whole count, and ok=false
// when the machine exposes none (so the caller omits the GPU dimension entirely —
// no prompt, no flag echo). When several GPU device names are present the one with
// the highest count wins (deterministic; real single-vendor hosts have exactly one).
func MachineGPU(m Machine) (name corev1.ResourceName, count int64, ok bool) {
	var best int64 = -1
	for n, q := range m.GPU {
		if v := q.Value(); v > best {
			best, name, count, ok = v, n, v, true
		}
	}
	return name, count, ok
}

// BelowCoreFloor / BelowMemFloor enforce the per-run minimum (~1 core / 2 GiB):
// a run smaller than this can't hold a training job and is almost always a typo.
func BelowCoreFloor(cpu resource.Quantity) bool { return cpu.MilliValue() < minRunCPUMilli }
func BelowMemFloor(mem resource.Quantity) bool  { return mem.Value() < minRunMemBytes }

// Cores / MemGiB are the floor values as user-facing strings, for error messages.
func CoreFloorText() string { return "1 core" }
func MemFloorText() string  { return "2 GiB" }

// NoGPUEnvValue is the canonical "this run uses no GPU" representation for the
// GPU_REQUESTS / GPU_LIMITS chart env: an EXPLICIT empty string.
//
// It is grounded in how the chart's jobs-manager reads the value
// (client-runtime jobs_manager._gpu_available_from_env, client-runtime#80): the
// chart ALWAYS injects GPU_REQUESTS/GPU_LIMITS (defaulting to "nvidia.com/gpu=1"
// when unset in values), so an explicit empty string is the ONLY value the
// jobs-manager treats as "no GPU on this run" — any non-empty value, INCLUDING
// "nvidia.com/gpu=0", is read as GPU-present and forced back to a count of 1.
// It also round-trips through this package's own parseGPU (which reads "" as
// HasGPU=false), so a write here reads back consistently (di#358: the reader
// must mirror the writer).
const NoGPUEnvValue = ""

// BuildEnvSpec renders a per-run ceiling into the exact chart env the run is
// stamped with. RESOURCE_REQUESTS == RESOURCE_LIMITS (Guaranteed QoS, the chart
// contract), both "cpu=X,memory=Y".
//
// Every dimension is ALWAYS written — never omitted. The apply uses
// `helm upgrade --reset-then-reuse-values`, which re-applies the release's
// previously-stored values on top of chart defaults; a key we omit is therefore
// NOT cleared but silently re-inherited from the prior release (or the chart's
// "nvidia.com/gpu=1" default). So to actually REMOVE the GPU (wantGPU=false) we
// must write the explicit no-GPU value (NoGPUEnvValue) that OVERRIDES the stored
// one — omitting the keys would leave the old GPU in place while the success
// echo claimed it was gone. RESOURCE_* have no "unset" state and are likewise
// always written.
func BuildEnvSpec(cpu, mem resource.Quantity, gpuName corev1.ResourceName, gpu resource.Quantity, wantGPU bool) map[string]string {
	spec := fmt.Sprintf("cpu=%s,memory=%s", cpu.String(), mem.String())
	env := map[string]string{"RESOURCE_REQUESTS": spec, "RESOURCE_LIMITS": spec}
	if wantGPU {
		g := fmt.Sprintf("%s=%d", gpuName, gpu.Value())
		env["GPU_LIMITS"], env["GPU_REQUESTS"] = g, g
	} else {
		env["GPU_LIMITS"], env["GPU_REQUESTS"] = NoGPUEnvValue, NoGPUEnvValue
	}
	return env
}

// ParseCores parses a plain CPU-core count into a quantity. It deliberately
// REJECTS anything carrying a unit suffix — "6GB", "500m", "4Gi" — because on the
// `set` command "cores" means whole/fractional CPU cores, and letting
// resource.ParseQuantity swallow "6GB" as 6 gigabytes is the exact footgun the
// human wording is meant to avoid. Zero/negative is rejected too.
func ParseCores(s string) (resource.Quantity, error) {
	t := strings.TrimSpace(s)
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf(
			"set cores as a plain number of CPU cores, e.g. --cores 4 (got %q)", s)
	}
	if f <= 0 {
		return resource.Quantity{}, fmt.Errorf("cores must be greater than zero (got %q)", s)
	}
	milli := int64(f*1000 + 0.5)
	return *resource.NewMilliQuantity(milli, resource.DecimalSI), nil
}

// ParseMemoryGiB parses a memory size, interpreting the NUMBER as GiB regardless
// of a GB/G/Gi/GiB suffix (the locked design: all forms mean GiB; a bare number
// means GiB). bare reports whether the user gave no suffix, so the caller can echo
// the GiB interpretation back. Any other unit (Mi, Ti, m, …) or garbage is a clear
// wrong-unit error, never a silent misread.
func ParseMemoryGiB(s string) (q resource.Quantity, bare bool, err error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return resource.Quantity{}, false, fmt.Errorf("a memory size is required, e.g. --memory 16")
	}
	num := t
	bare = true
	// Case-insensitive suffix strip; longest first so "gib" wins over "gi"/"g".
	low := strings.ToLower(t)
	for _, suf := range []string{"gib", "gb", "gi", "g"} {
		if strings.HasSuffix(low, suf) {
			num = strings.TrimSpace(t[:len(t)-len(suf)])
			bare = false
			break
		}
	}
	f, perr := strconv.ParseFloat(num, 64)
	if perr != nil {
		return resource.Quantity{}, false, fmt.Errorf(
			"set memory as a number of GiB, e.g. --memory 16 or --memory 16Gi (got %q)", s)
	}
	if f <= 0 {
		return resource.Quantity{}, false, fmt.Errorf("memory must be greater than zero (got %q)", s)
	}
	return gibToQuantity(f), bare, nil
}

// GiBToQuantity builds a canonical binary-SI memory quantity from a GiB count:
// whole GiB render as "NGi", fractional fall back to the nearest "NMi", so the
// chart env and `FormatMem` echo stay clean.
func GiBToQuantity(gibCount float64) resource.Quantity { return gibToQuantity(gibCount) }

func gibToQuantity(gibCount float64) resource.Quantity {
	mib := int64(gibCount*1024 + 0.5)
	if mib%1024 == 0 {
		return resource.MustParse(fmt.Sprintf("%dGi", mib/1024))
	}
	return resource.MustParse(fmt.Sprintf("%dMi", mib))
}
