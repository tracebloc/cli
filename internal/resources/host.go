package resources

import (
	"runtime"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Host is the physical machine the CLI runs on — best-effort. CPU is always
// available (the CLI runs natively on the host, not in the cluster VM); Mem is
// OS-specific (hostmem_*.go) and 0 when it can't be read.
//
// Used for the "Your machine has:" line on a LOCAL install, where the cluster
// runs on this machine and its capacity is a slice of the host. On a remote
// cluster the host is the operator's laptop and irrelevant to training, so the
// caller omits the line entirely.
type Host struct {
	CPU int   // logical CPUs
	Mem int64 // total physical memory in bytes; 0 = undetected
}

// DetectHost reports the physical host's compute (best-effort).
func DetectHost() Host {
	return Host{CPU: runtime.NumCPU(), Mem: hostMemBytes()}
}

// FormatHostCPU renders the host CPU count like FormatCPU ("16 CPU").
func FormatHostCPU(cpu int) string {
	return trimFloat(float64(cpu)) + " CPU"
}

// FormatHostMem renders host memory in GiB like FormatMem ("32 GiB"), or "" when
// it couldn't be detected (so the caller drops the memory dimension rather than
// printing a fabricated 0).
func FormatHostMem(bytes int64) string {
	if bytes <= 0 {
		return ""
	}
	return trimFloat(float64(bytes)/float64(1<<30)) + " GiB"
}

// Line renders the host as "CPU · GPU · GiB" (memory omitted when undetected).
// gpu is the cluster's GPU map — on a local install the machine's GPUs are the
// ones the cluster sees, so we reuse that rather than probing the host for them.
func (h Host) Line(gpu map[corev1.ResourceName]resource.Quantity) string {
	s := FormatHostCPU(h.CPU)
	for name, qty := range gpu {
		if !qty.IsZero() {
			s += " · " + FormatGPU(name, qty)
		}
	}
	if mem := FormatHostMem(h.Mem); mem != "" {
		s += " · " + mem
	}
	return s
}
