package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The four-level chain: host RAM -> VM RAM -> node allocatable -> free
// (backend#2221, RFC-BACKEND-664 §P4).
//
// Every other check in this package reads the cluster and believes it. On a
// local k3d install that belief is misplaced, because the node containers are
// created with `NanoCpus=0 CpuQuota=0 Memory=0`: each one honestly reports the
// WHOLE Docker VM, so a default server+agent cluster tells Kubernetes the
// machine is twice its real size. Measured on k3d v5.9.0 / k3s v1.35.5 /
// Docker 29.5.2:
//
//	host             10 cpu / 16.00 GiB
//	Docker VM        10 cpu /  7.75 GiB   <- the real machine
//	2 node containers        15.50 GiB    <- what Kubernetes believes
//
// The node memory was byte-identical to the VM's MemTotal on BOTH nodes. So
// checkNodeFit can truthfully say "a Ready node can schedule this job" for two
// jobs that cannot both exist, and its own drift nudge ("this machine could
// give a run up to cpu=9,memory=4Gi") advertises half the VM twice.
//
// The gap between the levels IS the customer's problem, and today no check
// shows it: doctor reads level 3 only, the installer's preflight gates on
// level 1, and nothing looks at level 2 at all. This check shows all four and
// names the one invariant that matters:
//
//	sum(node capacity) <= VM capacity
//
// Uncapped k3d violates it by exactly the node count.

// VMProbe reports the container runtime's VM size — `docker info` NCPU and
// MemTotal. Injected in tests; nil means dockerVMProbe.
//
// This is deliberately NOT the host and NOT the sum of node allocatable. On
// macOS and Windows the host is much larger than the VM (measured: a 16 GiB
// host behind a 7.75 GiB VM), which is why gating on host RAM passes machines
// whose cluster cannot hold what it was sized for.
type VMProbe func(ctx context.Context) (cores int64, memBytes int64, err error)

// HostProbe reports the physical machine's cores and RAM. Injected in tests;
// nil means hostProbe. Best-effort: the chain drops this level rather than
// failing when it cannot be read, because the VM is the binding constraint and
// the host is context.
type HostProbe func() (cores int64, memBytes int64, err error)

// dockerVMProbe asks the container runtime how big its VM is.
func dockerVMProbe(ctx context.Context) (int64, int64, error) {
	// #nosec G204 -- argv is compile-time constant: literal "docker" with a
	// fixed --format template. No user input reaches this command line.
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.NCPU}} {{.MemTotal}}").Output()
	if err != nil {
		return 0, 0, fmt.Errorf("docker info: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("docker info returned %q, want two fields", strings.TrimSpace(string(out)))
	}
	cores, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("unparseable NCPU %q: %w", fields[0], err)
	}
	mem, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("unparseable MemTotal %q: %w", fields[1], err)
	}
	if cores <= 0 || mem <= 0 {
		return 0, 0, fmt.Errorf("docker info reported cores=%d mem=%d", cores, mem)
	}
	return cores, mem, nil
}

// hostProbe reads physical RAM for the platforms the local install supports.
// runtime.NumCPU() is the host's logical CPU count on both — the CLI runs on
// the host, not inside the VM.
func hostProbe() (int64, int64, error) {
	cores := int64(runtime.NumCPU())
	switch runtime.GOOS {
	case "darwin":
		// #nosec G204 -- argv is compile-time constant.
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return cores, 0, fmt.Errorf("sysctl hw.memsize: %w", err)
		}
		mem, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return cores, 0, fmt.Errorf("unparseable hw.memsize: %w", err)
		}
		return cores, mem, nil
	case "linux":
		raw, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return cores, 0, fmt.Errorf("read /proc/meminfo: %w", err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(line, "MemTotal:") {
				continue
			}
			f := strings.Fields(line)
			if len(f) < 2 {
				break
			}
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				break
			}
			return cores, kb * 1024, nil
		}
		return cores, 0, fmt.Errorf("no MemTotal in /proc/meminfo")
	default:
		// Windows: the VM level is what binds there too, and .wslconfig is the
		// thing a user actually changes. Skip the host level rather than shell
		// out to WMI for a number this check only prints.
		return cores, 0, fmt.Errorf("host memory unsupported on %s", runtime.GOOS)
	}
}

// overCommitTolerance is how far sum(node capacity) may exceed the VM before
// this check calls it a double-count.
//
// Not slack for the real bug — that is 2.00x, and anything above ~1.05 is
// already unambiguous. It exists because a CORRECTLY capped k3d node reports
// slightly MORE than its own limit, which was measured:
//
//	k3d --servers-memory 3g  ->  cgroup memory.max = 3221225472
//	k3d's fake /proc/meminfo ->  MemTotal: 3221225 kB
//	kubelet capacity         ->  3221225Ki  == 3298534400 B, +2.4%
//
// k3d caps a node by bind-mounting a synthetic /proc/meminfo into the node
// container (a "fakeowner" mount) — NOT by the cgroup, which kubelet never
// reads. It writes MemTotal by dividing the byte limit by 1000 and labelling
// the result kB, but kB there means 1024 bytes, so the advertised capacity
// overstates the real limit by 2.4%. A strict `sum > vm` would therefore
// report correct capping as over-commit.
//
// The same measurement is why capping is a CREATE-TIME operation: `docker
// update --memory` on a running node container moves the cgroup and leaves
// /proc/meminfo alone, so node capacity does not budge even across a restart.
const overCommitTolerance = 1.05

// checkMachineChain surfaces host -> VM -> node allocatable -> free, and warns
// when the nodes claim more of the machine than the VM has.
func checkMachineChain(ctx context.Context, cs kubernetes.Interface, vm VMProbe, host HostProbe) Result {
	const name = "Machine capacity"

	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Result{
			Name:   name,
			Status: StatusUnknown,
			Detail: "could not list nodes: " + err.Error(),
		}
	}

	var ready []corev1.Node
	for i := range nodes.Items {
		if nodeReady(nodes.Items[i]) {
			ready = append(ready, nodes.Items[i])
		}
	}
	if len(ready) == 0 {
		return Result{Name: name, Status: StatusUnknown, Detail: "no Ready node to measure"}
	}

	// Only a k3d cluster has a Docker VM beneath it. On EKS the nodes ARE
	// machines and `docker info` on this laptop describes something unrelated,
	// so asserting anything from it would be worse than staying quiet.
	if !allK3d(ready) {
		return Result{
			Name:   name,
			Status: StatusUnknown,
			Detail: "not a local k3d cluster — the host/VM chain applies only to a cluster running on this machine's Docker VM",
		}
	}

	// CAPACITY, not allocatable: the invariant this check exists to state is
	// about what the nodes claim the machine IS. Making `allocatable` honest is
	// a separate question (kubelet reservations, RFC §P3) and k3s sets none, so
	// on the clusters this check applies to the two are byte-identical anyway.
	var sumCPU, sumMem int64
	for i := range ready {
		capacity := ready[i].Status.Capacity
		sumCPU += capacity.Cpu().MilliValue()
		sumMem += capacity.Memory().Value()
	}
	if sumMem <= 0 {
		// No capacity reported at all. Saying "0 GiB claimed, all good" would be
		// a green this check cannot back.
		return Result{Name: name, Status: StatusUnknown, Detail: "nodes report no memory capacity to compare against the VM"}
	}

	vmCores, vmMem, err := vm(ctx)
	if err != nil {
		// The VM is the level this check exists to add. Without it there is no
		// chain, and guessing is what the bug already does.
		return Result{
			Name:   name,
			Status: StatusUnknown,
			Detail: "could not read the Docker VM's size: " + err.Error(),
			Remedy: "Check Docker is running: docker info",
		}
	}

	// Level 4: what is actually left to request.
	//
	// Measured against min(sum, VM), NOT against the sum. On a double-counted
	// cluster the sum is a fiction, and "12.30 GiB unrequested" on a 7.75 GiB
	// VM would be this check repeating the very lie it is here to expose — the
	// fourth level inheriting the error from the third. min() is a no-op on an
	// honest cluster, where the sum never exceeds the VM.
	trueCeiling := sumMem
	if vmMem < trueCeiling {
		trueCeiling = vmMem
	}
	freeMem := trueCeiling - requestedMemory(ctx, cs)
	if freeMem < 0 {
		freeMem = 0
	}

	chain := ""
	if _, hostMem, herr := host(); herr == nil && hostMem > 0 {
		chain = fmt.Sprintf("host %s → ", gib(hostMem))
	}
	chain += fmt.Sprintf("Docker VM %s (%d cpu) → %d node%s claiming %s → %s unrequested",
		gib(vmMem), vmCores, len(ready), plural(len(ready)), gib(sumMem), gib(freeMem))

	if float64(sumMem) > float64(vmMem)*overCommitTolerance {
		ratio := float64(sumMem) / float64(vmMem)
		return Result{
			Name:   name,
			Status: StatusWarn,
			Detail: fmt.Sprintf("%s — Kubernetes believes %.2f× the memory this machine has, because the k3d node containers are uncapped and each reports the whole VM", chain, ratio),
			Remedy: fmt.Sprintf("Each node container reports the whole VM, so %d of them double-count it. Run a single-node environment, or cap the nodes (k3d --servers-memory/--agents-memory). Until then a job that fits a node may still OOM the VM.", len(ready)),
		}
	}

	return Result{Name: name, Status: StatusOK, Detail: chain}
}

// allK3d reports whether every Ready node is a k3d node container. k3d names
// them "k3d-<cluster>-server-N" / "-agent-N"; a mixed cluster is not a local
// install and gets no verdict.
func allK3d(nodes []corev1.Node) bool {
	for i := range nodes {
		if !strings.HasPrefix(nodes[i].Name, "k3d-") {
			return false
		}
	}
	return true
}

// requestedMemory sums memory requests across pods that still hold resources.
// Best-effort: on a read failure it returns 0, so the "unrequested" level
// degrades to the full claim rather than reporting a negative remainder.
func requestedMemory(ctx context.Context, cs kubernetes.Interface) int64 {
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0
	}
	var total int64
	for i := range pods.Items {
		p := pods.Items[i]
		// Succeeded/Failed pods hold no resources; counting them would
		// understate what is actually free.
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for j := range p.Spec.Containers {
			if q, ok := p.Spec.Containers[j].Resources.Requests[corev1.ResourceMemory]; ok {
				total += q.Value()
			}
		}
	}
	return total
}

func gib(b int64) string {
	if b <= 0 {
		return "0 GiB"
	}
	return fmt.Sprintf("%.2f GiB", float64(b)/float64(1<<30))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
