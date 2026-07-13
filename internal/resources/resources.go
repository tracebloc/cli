// Package resources backs `tracebloc resources` — the one-knob view of "how
// much of this machine tracebloc may use" (cli#143). It is deliberately
// cluster-free and pure: it reads already-fetched Kubernetes objects (nodes,
// the jobs-manager env) and turns them into the two numbers the command shows —
// the machine's capacity and the ceiling a single tracebloc training run may
// use — plus the user-language formatting for both.
//
// Grounding (verified against tracebloc/client + client-runtime, 2026-07):
//   - The machine's capacity is the sum of Ready nodes' Status.Allocatable
//     (the installer path is single-node, so this is normally one node).
//   - A training run's ceiling is the jobs-manager env RESOURCE_LIMITS
//     ("cpu=2,memory=8Gi", requests==limits for Guaranteed QoS). This is the
//     exact value client-runtime's jobs_manager.py stamps on spawned jobs and
//     the same value `cluster doctor`'s checkNodeFit already parses — so the two
//     read it identically (di#358 lesson: a reader must mirror the writer).
//
// No Kubernetes vocabulary leaks into what this package formats: the command
// renders CPU cores and GiB, never "requests", "limits", or "allocatable".
package resources

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// DefaultTraining is the chart's default spawned-job size when the operator set
// no override — mirrors tracebloc/client's jobs-manager-deployment.yaml default
// ("cpu=2,memory=8Gi") and client-runtime's own fallback. Kept here so a `show`
// against an older chart that omits the literal env still reports the real
// effective ceiling rather than "unknown".
const DefaultTraining = "cpu=2,memory=8Gi"

// Machine is a cluster's schedulable capacity: the sum of Ready nodes'
// allocatable CPU and memory, plus any GPU capacity keyed by its resource name
// (e.g. "nvidia.com/gpu"). The zero value is a machine with nothing Ready.
type Machine struct {
	CPU resource.Quantity
	Mem resource.Quantity
	GPU map[corev1.ResourceName]resource.Quantity
}

// Training is the ceiling one tracebloc training run may use, parsed from the
// jobs-manager resource env. HasCPUMem is false when neither RESOURCE_LIMITS nor
// RESOURCE_REQUESTS carried a parseable cpu+memory pair (so the command can say
// so honestly instead of printing a fabricated number).
type Training struct {
	CPU       resource.Quantity
	Mem       resource.Quantity
	HasCPUMem bool

	// GPU, when requested, is the per-run GPU ceiling. Name is the k8s device
	// resource name; HasGPU is false for a CPU-only run.
	GPUName corev1.ResourceName
	GPU     resource.Quantity
	HasGPU  bool
}

// MachineCapacity sums the allocatable CPU/memory (and GPU) across every Ready
// node. A pod is scheduled onto ONE node, but the machine-capacity headline is a
// whole-machine figure the user recognizes ("this machine has 8 CPU"); the
// single-node fit question is `cluster doctor`'s job (checkNodeFit), which
// deliberately never ORs capacity across nodes. On the installer's single-node
// path the sum is just that node.
func MachineCapacity(nodes []corev1.Node) Machine {
	m := Machine{GPU: map[corev1.ResourceName]resource.Quantity{}}
	for i := range nodes {
		n := nodes[i]
		if !nodeReady(n) {
			continue
		}
		for name, qty := range n.Status.Allocatable {
			switch name {
			case corev1.ResourceCPU:
				addInto(&m.CPU, qty)
			case corev1.ResourceMemory:
				addInto(&m.Mem, qty)
			default:
				// Surface GPUs (and any other extended device) so the machine
				// line can note "· 1 GPU" — but only non-zero quantities.
				if isGPUResource(name) && !qty.IsZero() {
					cur := m.GPU[name]
					cur.Add(qty)
					m.GPU[name] = cur
				}
			}
		}
	}
	return m
}

// ParseTraining reads the per-run ceiling from a jobs-manager env map. It
// prefers RESOURCE_LIMITS (the true ceiling) and falls back to RESOURCE_REQUESTS
// (requests==limits by chart contract), then to the chart default so an older
// chart without the literal env still reports the effective size. GPU is read
// from GPU_LIMITS, then GPU_REQUESTS.
func ParseTraining(env map[string]string) Training {
	spec := firstNonEmpty(env["RESOURCE_LIMITS"], env["RESOURCE_REQUESTS"])
	cpu, mem, ok := parseCPUMem(spec)
	if !ok {
		// Fall back to the chart default rather than reporting nothing: the
		// chart injects this exact value when the operator set no override.
		cpu, mem, ok = parseCPUMem(DefaultTraining)
	}
	t := Training{CPU: cpu, Mem: mem, HasCPUMem: ok}

	gpuName, gpuQty, gpuOK := parseGPU(firstNonEmpty(env["GPU_LIMITS"], env["GPU_REQUESTS"]))
	if gpuOK {
		t.GPUName, t.GPU, t.HasGPU = gpuName, gpuQty, true
	}
	return t
}

// FormatCPU renders a CPU quantity as the user sees it: whole cores with no
// decimal ("4 CPU"), fractional cores to one decimal ("1.2 CPU"). Never a
// milli-suffixed Kubernetes string.
func FormatCPU(q resource.Quantity) string {
	cores := float64(q.MilliValue()) / 1000.0
	return trimFloat(cores) + " CPU"
}

// FormatMem renders a memory quantity in GiB — whole when it divides evenly
// ("16 GiB"), else one decimal ("11.5 GiB"). GiB (1024^3), matching the Gi
// suffix the chart uses, so a "16Gi" limit reads back as "16 GiB".
func FormatMem(q resource.Quantity) string {
	gib := float64(q.Value()) / float64(1<<30)
	return trimFloat(gib) + " GiB"
}

// FormatGPU renders a GPU count with its short device label ("1 GPU"). The
// vendor prefix (nvidia.com/, amd.com/) is dropped — the user cares "does it
// have a GPU", not the device-plugin name.
func FormatGPU(name corev1.ResourceName, q resource.Quantity) string {
	n := q.Value()
	unit := "GPU"
	if n != 1 {
		return fmt.Sprintf("%d %ss", n, unit)
	}
	return fmt.Sprintf("%d %s", n, unit)
}

// --- internal helpers -------------------------------------------------------

// addInto adds src into dst in place. resource.Quantity.Add is a pointer
// method; dst starts as a zero quantity (value 0), so the first add seeds it.
func addInto(dst *resource.Quantity, src resource.Quantity) { dst.Add(src) }

// nodeReady reports whether a node's Ready condition is True. Mirrors
// doctor.nodeReady — kept local so this package stays leaf/pure and doesn't
// import the doctor command package.
func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// isGPUResource reports whether an extended resource name is a GPU device. Covers
// the common vendor plugins; anything else (hugepages, ephemeral-storage, …) is
// intentionally not surfaced as a GPU.
func isGPUResource(name corev1.ResourceName) bool {
	s := string(name)
	return strings.Contains(s, "gpu") || strings.HasPrefix(s, "nvidia.com/") || strings.HasPrefix(s, "amd.com/")
}

// parseResourceSpec parses jobs-manager's "k1=v1,k2=v2" env into a map. Same
// shape as doctor.parseResourceSpec — the format is a stable chart contract.
func parseResourceSpec(spec string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(spec, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && strings.TrimSpace(kv[0]) != "" {
			out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return out
}

// parseCPUMem extracts cpu+memory quantities; ok is false unless both parse.
func parseCPUMem(spec string) (cpu, mem resource.Quantity, ok bool) {
	m := parseResourceSpec(spec)
	c, cOK := m["cpu"]
	mm, mOK := m["memory"]
	if !cOK || !mOK {
		return resource.Quantity{}, resource.Quantity{}, false
	}
	cpu, errC := resource.ParseQuantity(c)
	mem, errM := resource.ParseQuantity(mm)
	if errC != nil || errM != nil {
		return resource.Quantity{}, resource.Quantity{}, false
	}
	return cpu, mem, true
}

// parseGPU extracts the GPU device name + count; requested is false when absent,
// unparseable, or zero.
func parseGPU(spec string) (name corev1.ResourceName, qty resource.Quantity, requested bool) {
	for k, v := range parseResourceSpec(spec) {
		q, err := resource.ParseQuantity(v)
		if err == nil && !q.IsZero() {
			return corev1.ResourceName(k), q, true
		}
	}
	return "", resource.Quantity{}, false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// trimFloat formats a float with at most one decimal, dropping a trailing ".0"
// so whole numbers read as "16" not "16.0".
func trimFloat(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	s = strings.TrimSuffix(s, ".0")
	return s
}
