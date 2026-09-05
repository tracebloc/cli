// Package doctor implements the checks behind `tracebloc cluster doctor`:
// a read-only, best-effort health sweep of a running tracebloc client
// cluster. Each check reports ✔/⚠/✖ plus a one-line remedy, so a customer
// can answer "why isn't my experiment running?" without tracebloc shelling
// into their cluster (epic tracebloc/client-runtime#116, WS3).
//
// Design mirrors the installer's preflight.sh: every check is independent
// and returns a Result instead of aborting, so one failure never hides the
// others. Network probes are injectable (Options) so the package is fully
// exercisable with client-go's fake clientset — no real cluster or egress.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/resources"
)

// Status is a single check's severity. Ordered so the numerically-greatest
// status is the worst; the cli layer rolls the checks up into the verdict.
type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
)

// StatusUnknown marks a check with no trustworthy signal either way. Two cases
// produce it:
//
//  1. The check could not run because a prerequisite was unavailable — today,
//     the cluster API being unreachable. A single root cause (a stopped cluster)
//     then renders as one honest ✖ plus neutral "couldn't check" lines, instead
//     of every downstream check inventing a false cause.
//  2. The check ran fine, but deliberately declines to assert a green it cannot
//     back — e.g. requests-proxy is present and Ready, yet readiness does not
//     prove Service Bus egress actually works and there is no probe for it yet
//     (backend#1143). A neutral line is more honest than a false ✔.
//
// Either way it carries NO signal: the verdict rollup ignores it, so it never
// affects the overall result or exit code.
const StatusUnknown Status = -1

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	case StatusUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Result is one check's outcome. Remedy is shown to the customer only when
// Status is not OK.
type Result struct {
	Name   string
	Status Status
	Detail string
	Remedy string

	// Reach classifies WHY the "Cluster reachable" check landed where it did, so
	// the cli summary can word the remedy correctly — an unreachable API (stopped
	// container runtime / network) needs a different fix than a reachable cluster
	// with no tracebloc installed. Zero (ReachOK) on every other check.
	Reach ReachState
}

// ReachState classifies the "Cluster reachable" outcome so the cli summary can
// word its remedy correctly (see Result.Reach). A stopped API, a reachable
// cluster with no tracebloc installed, and an RBAC/other error each need a
// different plain-language fix — never the same "isn't answering" line.
type ReachState int

const (
	ReachOK          ReachState = iota // API answered and the tracebloc chart is here
	ReachUnreachable                   // API never answered (container runtime/network down)
	ReachNoEnv                         // API answered, but no tracebloc secure environment here
	ReachError                         // API answered with some other error (RBAC/NotFound/…)
)

// Conservative tunables, kept as package consts to avoid false positives on a
// busy cluster. If a check ever needs runtime tuning, thread it through Options
// (like HTTPProbe) rather than making these package vars.
const (
	pendingGrace     = 5 * time.Minute // a pod Pending longer than this is flagged
	httpProbeTimeout = 8 * time.Second

	// restartWarnThreshold is the container RestartCount at/above which the
	// doctor surfaces a pod's restart *history* as a ⚠. checkPods reads only the
	// current waiting reason, so a container that crashed several times and then
	// recovered — or a job that flapped before Succeeding — leaves no live trace
	// there and reads as OK (the gap backend#1028 flagged). We pick 3, not 1: a
	// single restart is routine (a node drain, an image warm-up, a dependency not
	// ready on first boot, a one-off liveness-probe miss), so 1–2 would be noisy.
	// 3+ distinct restarts is a genuine flap worth a log look, while staying a
	// history hint that never escalates past ⚠ or overrides checkPods'
	// crash-loop-now failure.
	restartWarnThreshold = 3
)

// Options configures a diagnosis run. The zero value is usable: Namespace
// defaults are the caller's concern (it passes the resolved namespace), and
// HTTPProbe falls back to the real proxy-aware prober.
type Options struct {
	Namespace string

	// ServerURL is the cluster API endpoint from the resolved kubeconfig. Used
	// only to name the endpoint (and detect a local/loopback cluster) in the
	// "Cluster reachable" remedy when the API is unreachable. Empty is fine — the
	// remedy just omits the address.
	ServerURL string

	// HTTPProbe reports whether a URL is reachable from where the CLI runs.
	// nil => httpProbe (proxy-aware, short timeout). Injected in tests.
	HTTPProbe func(ctx context.Context, url string) error

	// VMProbe reports the container runtime's VM size, and HostProbe the
	// physical machine's. nil => dockerVMProbe / hostProbe. Both injected in
	// tests so the four-level chain is exercisable without Docker
	// (backend#2221).
	VMProbe   VMProbe
	HostProbe HostProbe
}

// Run executes every check in display order and returns their results. It
// never returns an error: an unreachable cluster or a failing probe is a
// Result, not a Go error — the cli layer rolls them up into the two health
// lines the owner reads and derives the exit code from that verdict.
func Run(ctx context.Context, cs kubernetes.Interface, opts Options) []Result {
	if opts.HTTPProbe == nil {
		opts.HTTPProbe = httpProbe
	}
	if opts.VMProbe == nil {
		opts.VMProbe = dockerVMProbe
	}
	if opts.HostProbe == nil {
		opts.HostProbe = hostProbe
	}
	ns := opts.Namespace

	// Discovered once: the first API call. Its error is the reachability signal
	// the rest of the sweep keys on.
	release, relErr := cluster.DiscoverParentRelease(ctx, cs, ns)

	// If the cluster API itself is unreachable (Docker/k3d stopped, wrong server,
	// network down), every cluster-dependent check would fail its own API call
	// and invent a domain-specific cause — "PVC unbound", "requests-proxy not
	// found → reinstall the chart", "chart too old" — burying the one true cause
	// under a wall of false ✖/⚠. Report checkReachable as the single honest ✖ and
	// mark the cluster checks StatusUnknown ("couldn't check"). checkBackendEgress
	// still runs: it probes the backend from THIS machine and never touches the
	// cluster API, so its verdict stays truthful. Display order is preserved.
	if isUnreachable(relErr) {
		return []Result{
			checkReachable(release, relErr, ns, opts.ServerURL),
			unknownCheck("Pod health"),
			unknownCheck("Restart history"),
			unknownCheck("Dataset volume (PVC)"),
			unknownCheck("Node capacity"),
			unknownCheck("Machine capacity"),
			unknownCheck("Image pull secret"),
			unknownCheck("Proxy configuration"),
			checkBackendEgress(ctx, nil, opts.HTTPProbe),
			unknownCheck("Service Bus egress (requests-proxy)"),
		}
	}

	// API reachable: every check runs and reports independently, as before.
	jmEnv := jobsManagerEnv(ctx, cs, ns, release)

	return []Result{
		checkReachable(release, relErr, ns, opts.ServerURL),
		checkPods(ctx, cs, ns),
		checkRestartHistory(ctx, cs, ns),
		checkPVC(ctx, cs, ns),
		checkNodeFit(ctx, cs, jmEnv),
		checkMachineChain(ctx, cs, opts.ServerURL, opts.VMProbe, opts.HostProbe),
		checkImagePull(ctx, cs, ns, release),
		checkProxy(jmEnv),
		checkBackendEgress(ctx, jmEnv, opts.HTTPProbe),
		checkRequestsProxy(ctx, cs, ns, release),
	}
}

// checkReachable confirms the API answered and the parent client chart is
// installed here. It's the gate the customer reads first; the rest still run.
func checkReachable(release *cluster.ParentRelease, err error, ns, serverURL string) Result {
	const name = "Cluster reachable"
	if err != nil {
		// Transport error: the API server never answered. Name the endpoint and,
		// for a local (loopback) cluster, give the start-it remedy — the common
		// "Docker/k3d stopped" case, not a kubeconfig or chart problem. This is the
		// only branch reached when Run() takes its unreachable short-circuit.
		if isUnreachable(err) {
			detail := "the cluster API server isn't answering"
			if serverURL != "" {
				detail = fmt.Sprintf("the cluster API server at %s isn't answering — is the cluster running?", serverURL)
			}
			remedy := "Check your secure environment is running."
			if isLoopback(serverURL) {
				remedy = "Start Docker Desktop (`open -a Docker`) — your secure environment restarts with it — then run this again."
			}
			return Result{Name: name, Status: StatusFail, Detail: detail, Remedy: remedy, Reach: ReachUnreachable}
		}
		// API answered but no chart here (ErrNoParentRelease) or another error.
		// The discovery error's remediation tail points at doctor — which is
		// what's running. Strip it so doctor never tells the user to run doctor.
		// (Must match the exact suffix cluster.discover appends.)
		detail := strings.TrimSuffix(strings.TrimSpace(err.Error()), "Diagnose with `tracebloc doctor`.")
		// The API answered: a missing chart means "no environment installed here"
		// (fix: reinstall), anything else is an RBAC/NotFound-class error (fix:
		// support). The cli summary words each differently — never a kubectl.
		reach := ReachError
		if errors.Is(err, cluster.ErrNoParentRelease) {
			reach = ReachNoEnv
		}
		return Result{
			Name:   name,
			Status: StatusFail,
			Detail: strings.TrimSpace(detail),
			Remedy: "Check your kubeconfig/context and that the tracebloc client chart is installed here: kubectl get deploy -n " + ns,
			Reach:  reach,
		}
	}
	return Result{
		Name:   name,
		Status: StatusOK,
		Detail: fmt.Sprintf("release %q, chart %s, appVersion %s (namespace %s)",
			release.ReleaseName, release.ChartVersion, release.AppVersion, ns),
	}
}

// isUnreachable reports whether err is a transport/connectivity failure talking
// to the cluster API server (connection refused, timeout, DNS, TLS) — as opposed
// to the API answering with "no chart here" (ErrNoParentRelease) or an
// RBAC/NotFound status. Those latter cases mean the API IS reachable and the
// per-check verdicts are trustworthy; a transport error means they are not, so
// Run() short-circuits to a single honest "Cluster reachable" ✖.
func isUnreachable(err error) bool {
	if err == nil {
		return false
	}
	// The API answered — these are real verdicts, not connectivity failures.
	if errors.Is(err, cluster.ErrNoParentRelease) ||
		apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsNotFound(err) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	// client-go often wraps the transport error as a plain string by the time it
	// reaches us; match the connectivity signatures as a fallback.
	msg := err.Error()
	for _, sig := range []string{
		"connection refused", "no such host", "i/o timeout", "dial tcp",
		"TLS handshake", "network is unreachable", "connection reset",
		"server misbehaving", "no route to host",
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// isLoopback reports whether serverURL points at the local machine — a
// k3d/kind/Docker-Desktop cluster, whose "isn't answering" almost always means
// the container runtime or the cluster is simply stopped, so the remedy is
// "start Docker Desktop". It covers the loopback addresses (127.0.0.0/8, ::1,
// localhost), the unspecified/wildcard bind addresses k3d writes into a
// kubeconfig when no explicit host is pinned (0.0.0.0, ::), and Docker Desktop's
// host alias (host.docker.internal). No genuinely-remote cluster is ever reached
// through any of these, so treating them as local carries no false-positive risk.
func isLoopback(serverURL string) bool {
	if serverURL == "" {
		return false
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "host.docker.internal":
		return true
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

// unknownCheck is the placeholder for a cluster check that could not run because
// the API is unreachable — no signal, so the verdict rollup ignores it and the
// verdict comes from "Cluster reachable" alone.
func unknownCheck(name string) Result {
	return Result{
		Name:   name,
		Status: StatusUnknown,
		Detail: "could not check — cluster API unreachable (see 'Cluster reachable' above)",
	}
}

// checkPods flags crash-looping or long-Pending pods — the local complement
// to the controller's crash-loop detection (client-runtime#117). Conservative
// thresholds keep a transient restart or a briefly-Pending job from tripping it.
func checkPods(ctx context.Context, cs kubernetes.Interface, ns string) Result {
	const name = "Pod health"
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Result{
			Name:   name,
			Status: StatusWarn,
			Detail: "could not list pods: " + err.Error(),
			Remedy: "Ensure your kubeconfig user can list pods in " + ns + ".",
		}
	}

	var crashing, pending []string
	for _, p := range pods.Items {
		if podCrashLooping(p) {
			crashing = append(crashing, p.Name)
			continue
		}
		if p.Status.Phase == corev1.PodPending &&
			time.Since(p.CreationTimestamp.Time) > pendingGrace {
			pending = append(pending, p.Name)
		}
	}
	sort.Strings(crashing)
	sort.Strings(pending)

	switch {
	case len(crashing) > 0:
		return Result{
			Name:   name,
			Status: StatusFail,
			Detail: fmt.Sprintf("crash-looping: %v", crashing),
			Remedy: "Inspect the container's own logs: kubectl logs -n " + ns + " <pod> --previous",
		}
	case len(pending) > 0:
		return Result{
			Name:   name,
			Status: StatusWarn,
			Detail: fmt.Sprintf("Pending > %s: %v", pendingGrace, pending),
			Remedy: "kubectl describe pod -n " + ns + " <pod> — usually unschedulable (resources) or an image pull issue.",
		}
	default:
		return Result{Name: name, Status: StatusOK, Detail: fmt.Sprintf("%d pod(s), none crash-looping or stuck Pending", len(pods.Items))}
	}
}

// podCrashLooping reports whether a pod has a container actively stuck in
// CrashLoopBackOff — the state Kubernetes sets for a container that keeps
// crashing. Both init AND app containers are checked: an init container in
// CrashLoopBackOff keeps the pod Pending and blocks startup silently (Bugbot
// on PR #89).
//
// We deliberately do NOT infer crash-looping from RestartCount: a high count
// is equally produced by a pod that recovered on retry, a job that retried
// before Succeeding, or a completed init container — all healthy (Bugbot on
// PR #89; cf. the controller's recovered-container fix, client-runtime#117).
// The terminal-phase guard is belt-and-suspenders: a Succeeded/Failed pod has
// no waiting containers anyway.
func podCrashLooping(p corev1.Pod) bool {
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return false
	}
	for _, group := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, c := range group {
			if c.State.Waiting != nil && c.State.Waiting.Reason == "CrashLoopBackOff" {
				return true
			}
		}
	}
	return false
}

// checkRestartHistory surfaces containers that have restarted repeatedly even
// though they are not crash-looping right now — the restart-*history* signal
// backend#1028 asked for. checkPods reads only the current waiting reason, so a
// container that crashed several times and then came up healthy (or a job that
// flapped before Succeeding) reads as OK there, hiding a real problem. This is
// deliberately an ADDITIONAL check and never touches podCrashLooping: it caps
// at ⚠ (history, not liveness) and both init AND app container statuses are
// scanned, since an init container that keeps dying blocks startup just as much.
func checkRestartHistory(ctx context.Context, cs kubernetes.Interface, ns string) Result {
	const name = "Restart history"
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Result{
			Name:   name,
			Status: StatusWarn,
			Detail: "could not list pods: " + err.Error(),
			Remedy: "Ensure your kubeconfig user can list pods in " + ns + ".",
		}
	}

	var flapped []string
	for _, p := range pods.Items {
		for _, group := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
			for _, c := range group {
				if c.RestartCount >= restartWarnThreshold {
					flapped = append(flapped, fmt.Sprintf("pod %s container %s restarted %d times", p.Name, c.Name, c.RestartCount))
				}
			}
		}
	}
	sort.Strings(flapped)

	if len(flapped) > 0 {
		return Result{
			Name:   name,
			Status: StatusWarn,
			Detail: fmt.Sprintf("restarted ≥%d times — check logs: %v", restartWarnThreshold, flapped),
			Remedy: "A container that restarted repeatedly may be flapping even if it's up now. Check its logs: kubectl logs -n " + ns + " <pod> -c <container> --previous",
		}
	}
	return Result{Name: name, Status: StatusOK, Detail: fmt.Sprintf("%d pod(s), none restarted ≥%d times", len(pods.Items), restartWarnThreshold)}
}

// checkPVC reuses cluster.DiscoverSharedPVC — which already verifies the
// shared-data PVC exists and is Bound, with actionable errors.
func checkPVC(ctx context.Context, cs kubernetes.Interface, ns string) Result {
	const name = "Dataset volume (PVC)"
	pvc, err := cluster.DiscoverSharedPVC(ctx, cs, ns)
	if err != nil {
		return Result{
			Name:   name,
			Status: StatusFail,
			Detail: err.Error(),
			Remedy: "Check the cluster has a usable StorageClass (kubectl get sc) and the PVC is Bound (kubectl get pvc -n " + ns + ").",
		}
	}
	return Result{
		Name:   name,
		Status: StatusOK,
		Detail: fmt.Sprintf("%s Bound, mounted at %s", pvc.ClaimName, pvc.MountPath),
	}
}

// checkProxy surfaces the in-cluster proxy wiring read from jobs-manager's
// env — the corporate-proxy propagation that, when missing, silently strands
// egress. Informational: present config is ✔; a missing requests-proxy URL is
// the only genuine anomaly.
func checkProxy(env map[string]string) Result {
	const name = "Proxy configuration"
	rp := env["REQUESTS_PROXY_URL"]
	if rp == "" {
		return Result{
			Name:   name,
			Status: StatusWarn,
			// jobsManagerEnv reads only literal env values, so a chart that sets
			// REQUESTS_PROXY_URL via a configMap/secret ref reads as empty here —
			// called out in the remedy so a ref-based install isn't mistaken for
			// missing wiring.
			Detail: "jobs-manager has no literal REQUESTS_PROXY_URL (chart too old, or it's set via a configMap/secret ref)",
			Remedy: "Verify the requests-proxy is wired: kubectl set env deploy/<release>-jobs-manager --list | grep PROXY",
		}
	}
	detail := "requests-proxy=" + rp
	if eg := env["EGRESS_PROXY_URL"]; eg != "" {
		detail += ", egress-proxy=" + eg
	}
	if env["HTTPS_PROXY"] != "" || env["HTTP_PROXY"] != "" {
		detail += ", corporate HTTP(S)_PROXY set"
	} else {
		detail += ", no corporate HTTP(S)_PROXY"
	}
	return Result{Name: name, Status: StatusOK, Detail: detail}
}

// checkBackendEgress probes the tracebloc backend API. Honest scope: this
// runs from the machine the CLI is on, NOT from inside the cluster — the
// cluster egresses via its egress-proxy. An in-cluster probe is the WS3
// follow-up; this still catches a customer network/proxy that can't reach
// the backend at all.
func checkBackendEgress(ctx context.Context, env map[string]string, probe func(context.Context, string) error) Result {
	const name = "Backend egress (from this machine)"
	host := backendHost(env["CLIENT_ENV"])
	url := "https://" + host + "/"
	if err := probe(ctx, url); err != nil {
		return Result{
			Name:   name,
			Status: StatusFail,
			Detail: fmt.Sprintf("%s unreachable: %v", host, err),
			Remedy: "Check this machine's network/proxy to " + host + ". The cluster egresses via its egress-proxy, so this is indicative, not definitive.",
		}
	}
	return Result{Name: name, Status: StatusOK, Detail: host + " reachable"}
}

// backendHost maps CLIENT_ENV to the backend API host, mirroring the edge
// runtime's own mapping (controller.py). Unset/unknown defaults to prod, the
// chart's CLIENT_ENV default.
//
// DERIVED FROM api.BaseURL, not restated. The env→host mapping used to be a
// second copy of BaseURL's switch living in this package, which is how the two
// drift: the same three hosts written down twice, with nothing that fails when
// only one of them is edited. api.BaseURL already lower-cases, so TrimSpace is
// the only normalisation this adds — a CLIENT_ENV read off a container spec can
// carry surrounding whitespace that a --env flag cannot.
//
// The input is the CLUSTER's CLIENT_ENV (read off the jobs-manager Deployment),
// not this CLI's session env — a deliberately different question, which is why
// this takes a string rather than calling into the session resolution.
func backendHost(clientEnv string) string {
	u, err := url.Parse(api.BaseURL(strings.TrimSpace(clientEnv)))
	if err != nil || u.Host == "" {
		// Unreachable for BaseURL's closed set of return values; the prod default
		// keeps this total rather than returning an empty host into a probe URL.
		return "api.tracebloc.io"
	}
	return u.Host
}

// checkRequestsProxy verifies the requests-proxy deployment is present and
// Ready. requests-proxy is the OUTBOUND relay: training pods POST epoch
// results/FLOPs to it and it forwards them to the Service Bus queues. It is NOT
// on the scheduling path — jobs-manager consumes the experiment subscription
// directly with its own credentials — so a down requests-proxy stalls result/
// weights egress MID-RUN; it does not block scheduling (experiments do not
// "stay Pending" for this). Ready here is the Deployment's ReadyReplicas, which
// (absent a readiness probe — backend#1143) only means the container started,
// not that Service Bus egress actually works. So a Ready relay returns a neutral
// StatusUnknown ("running, egress not actively verified"), never a ✔ (cli#351);
// the real ✔ arrives once the chart ships that probe and doctor consumes it.
func checkRequestsProxy(ctx context.Context, cs kubernetes.Interface, ns string, release *cluster.ParentRelease) Result {
	const name = "Service Bus egress (requests-proxy)"
	dep := findDeployment(ctx, cs, ns, release, "requests-proxy")
	if dep == nil {
		return Result{
			Name:   name,
			Status: StatusFail,
			Detail: "requests-proxy deployment not found",
			Remedy: "requests-proxy relays training results/metrics to Service Bus; without it, running experiments can't send results back and training stalls mid-run (scheduling is unaffected). Reinstall/upgrade the client chart.",
		}
	}
	if dep.Status.ReadyReplicas < 1 {
		return Result{
			Name:   name,
			Status: StatusFail,
			Detail: fmt.Sprintf("requests-proxy not ready (%d/%d replicas)", dep.Status.ReadyReplicas, dep.Status.Replicas),
			Remedy: "While requests-proxy is down, in-flight training can't relay epoch results/FLOPs to Service Bus — result egress stalls (scheduling is unaffected). kubectl describe deploy " + dep.Name + " -n " + ns,
		}
	}
	// Present and Ready — but readiness ≠ egress works (see the doc comment). Do
	// not green this off readiness; report a neutral, honest "unknown" so doctor
	// never claims Service Bus egress is verified when it hasn't been (cli#351).
	return Result{
		Name:   name,
		Status: StatusUnknown,
		Detail: "requests-proxy is running, but egress to Service Bus is not actively verified — readiness only confirms the relay started, not that it can reach Service Bus",
	}
}

// checkNodeFit verifies at least one Ready node can satisfy the resource
// requests the jobs-manager stamps on spawned training jobs (RESOURCE_REQUESTS
// / GPU_REQUESTS env) — the "Pending forever, no node big enough" class. The
// fit is against what is FREE on a node, not its allocatable, and it tells two
// shortages apart (backend#2870):
//
//   - PERMANENT (Fail, prefix OverCommitted): the envelope does not fit beside
//     the platform's own steady-state pods. No run can ever schedule here until
//     the envelope shrinks or the machine grows.
//   - TRANSIENT (Warn, prefix HeldByRunningJob): it fits beside the platform,
//     but a running batch Job holds the room right now. The next run waits;
//     nothing on the machine needs changing.
//
// GPU is soft: when a GPU is requested but no node exposes it, that's a ⚠
// (jobs-manager has a GPU→CPU fallback), not a hard failure.
func checkNodeFit(ctx context.Context, cs kubernetes.Interface, env map[string]string) Result {
	const name = "Node capacity"
	cpuReq, memReq, ok := parseCPUMem(env["RESOURCE_REQUESTS"])
	if !ok {
		return Result{
			Name:   name,
			Status: StatusWarn,
			Detail: "couldn't read RESOURCE_REQUESTS from jobs-manager — skipping node-fit",
			Remedy: "kubectl set env deploy/<release>-jobs-manager --list | grep RESOURCE_REQUESTS",
		}
	}
	gpuName, gpuReq, gpuRequested := parseGPU(env["GPU_REQUESTS"])
	diskReq, diskRequested := parseDisk(env["RESOURCE_REQUESTS"])

	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Result{
			Name:   name,
			Status: StatusWarn,
			Detail: "could not list nodes: " + err.Error(),
			Remedy: "Ensure your kubeconfig user can list nodes.",
		}
	}

	req := fmt.Sprintf("cpu=%s, memory=%s", cpuReq.String(), memReq.String())
	if diskRequested {
		req += fmt.Sprintf(", ephemeral-storage=%s", diskReq.String())
	}
	if gpuRequested {
		req += fmt.Sprintf(", %s=%s", gpuName, gpuReq.String())
	}

	// A pod gets ALL its requested resources from ONE node, so evaluate each
	// node as a whole — never OR cpu/mem and GPU across different nodes, which
	// would pass even when no single node can run the job (Bugbot on PR #91).
	// FREE, not allocatable (backend#2870). A node whose control plane already
	// requests memory has less room than its allocatable advertises, and fitting
	// the training envelope against allocatable is the blind spot that let every
	// install go Pending unnoticed: the installer writes `allocatable − overhead`,
	// and if the real control plane exceeds `overhead` the pod cannot schedule even
	// though allocatable "fits". Sum the requests already on each node and evaluate
	// the job against what is FREE. If the pod list cannot be read, free is UNKNOWN:
	// say so and fall back to allocatable, never silently pass it off as free.
	reqCPU := map[string]int64{} // node -> already-requested millicores
	reqMem := map[string]int64{} // node -> already-requested bytes
	// Requests held by RUNNING batch Jobs, per node, kept APART from the
	// steady-state sums above. They answer a different question: "beside the
	// platform" is about whether a run can ever schedule here, "beside the
	// platform and the job that is running" is about whether one can schedule
	// NOW. Folding them together produced a false Fail during healthy training
	// (Bugbot High on #628); dropping them produced a green over a machine whose
	// next run was going to wait. Two sums, two verdicts.
	jobCPU := map[string]int64{} // node -> millicores held by running Jobs
	jobMem := map[string]int64{} // node -> bytes held by running Jobs
	freeKnown := true
	if pods, perr := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{}); perr == nil {
		for i := range pods.Items {
			p := pods.Items[i]
			// A pod with no node holds no node's memory yet; a terminal pod holds
			// none at all -- counting either would understate free (mirrors
			// requestedMemory's Succeeded/Failed skip).
			if p.Spec.NodeName == "" ||
				p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			// Batch-Job pods carry the `job-name` label the batch/v1 controller
			// stamps (see internal/submit/watch.go) -- that label, not a name
			// pattern, is how the CLI already recognises a training or ingestion
			// pod, so it is the one used here. They go into the JOB sums, never the
			// steady-state ones: a running job holds the envelope itself, so
			// counting it as platform would make doctor Fail "no room for a training
			// job" on the exact healthy state it exists to bless (Bugbot High). The
			// steady state is the control plane (Deployments/DaemonSets), which has
			// no `job-name`.
			cpu, mem := reqCPU, reqMem
			if _, isJob := p.Labels["job-name"]; isJob {
				cpu, mem = jobCPU, jobMem
			}
			for j := range p.Spec.Containers {
				r := p.Spec.Containers[j].Resources.Requests
				if q, ok := r[corev1.ResourceCPU]; ok {
					cpu[p.Spec.NodeName] += q.MilliValue()
				}
				if q, ok := r[corev1.ResourceMemory]; ok {
					mem[p.Spec.NodeName] += q.Value()
				}
			}
		}
	} else {
		freeKnown = false
	}

	var cpuMemFits, fullFits, allocOnlyFit, overCPU, overMem bool
	// The first node that is big enough by allocatable but not by FREE, with the
	// free figures it had -- so the permanent Fail can print the numbers the
	// rollup's remedy promises ("--verbose shows the exact numbers") instead of
	// only the request. Bounded to the node the verdict is about; the CPU-major
	// bestFree* below may belong to a different node.
	var overNode string
	var overFreeCPUm, overFreeMemB int64
	// Whether some node fits the envelope beside the platform AND the Jobs
	// running on it -- i.e. can a run schedule NOW, not just ever. When it fits
	// the steady state but not this, the first such node and what its Jobs hold
	// are recorded for the transient Warn.
	var nowFits bool
	var heldNode string
	var heldCPUm, heldMemB int64
	// Largest Ready node for the drift nudge — CPU-major with memory as the
	// tie-break, EXACTLY like resources.nodeLarger, so the advertised ceiling
	// always matches what `resources set max` will actually apply (Bugbot).
	var bestCPU, bestMem resource.Quantity
	// Largest ephemeral-storage seen on a Ready node, tracked SEPARATELY from
	// the CPU-major "best node" above: it is reported for visibility, not used
	// to pick a node, so it must not perturb the drift nudge's tie-break.
	var bestDisk resource.Quantity
	var sawDisk bool
	// The most cpu/memory actually FREE on any one Ready node. Tracked next to
	// the allocatable-derived best above because the drift nudge must not
	// advertise a ceiling the fit would refuse: the fit moved to free, the nudge
	// did not, so on a large-but-claimed node it advised sizing UP to a figure
	// that recreates the over-commit this very check now fails (Bugbot Medium,
	// confirmed by @saqlainsyed007).
	var bestFreeCPUm, bestFreeMemB int64
	for i := range nodes.Items {
		n := nodes.Items[i]
		if !nodeReady(n) {
			continue
		}
		alloc := n.Status.Allocatable
		if alloc.Cpu().Cmp(bestCPU) > 0 ||
			(alloc.Cpu().Cmp(bestCPU) == 0 && alloc.Memory().Cmp(bestMem) > 0) {
			bestCPU = *alloc.Cpu()
			bestMem = *alloc.Memory()
		}
		if d, present := alloc[corev1.ResourceEphemeralStorage]; present {
			sawDisk = true
			if d.Cmp(bestDisk) > 0 {
				bestDisk = d
			}
		}
		// Allocatable minus what is already requested on THIS node (never below 0).
		// When free is unknown, this is allocatable and the caveat is reported below.
		freeCPUm := alloc.Cpu().MilliValue()
		freeMemB := alloc.Memory().Value()
		if freeKnown {
			if freeCPUm -= reqCPU[n.Name]; freeCPUm < 0 {
				freeCPUm = 0
			}
			if freeMemB -= reqMem[n.Name]; freeMemB < 0 {
				freeMemB = 0
			}
		}
		if freeCPUm > bestFreeCPUm ||
			(freeCPUm == bestFreeCPUm && freeMemB > bestFreeMemB) {
			bestFreeCPUm, bestFreeMemB = freeCPUm, freeMemB
		}
		nodeCPUMem := freeCPUm >= cpuReq.MilliValue() && freeMemB >= memReq.Value()
		// Whether the node is big enough IGNORING neighbours -- the old, blind
		// verdict. Kept only to tell "no node is big enough at all" apart from
		// "a node is big enough but not beside its control plane" in the message.
		if alloc.Cpu().Cmp(cpuReq) >= 0 && alloc.Memory().Cmp(memReq) >= 0 {
			allocOnlyFit = true
			// Which dimension the FREE fit falls short on, so the over-commit
			// message names cpu vs memory instead of always blaming memory (Bugbot).
			if freeCPUm < cpuReq.MilliValue() {
				overCPU = true
			}
			if freeMemB < memReq.Value() {
				overMem = true
			}
			if (freeCPUm < cpuReq.MilliValue() || freeMemB < memReq.Value()) && overNode == "" {
				overNode, overFreeCPUm, overFreeMemB = n.Name, freeCPUm, freeMemB
			}
		}
		// Disk joins cpu+memory as a WHOLE-NODE condition. A pod gets every
		// resource it requests from ONE node, so this must be AND-ed into the
		// same per-node verdict and never OR-ed across nodes (Bugbot on PR #91
		// made exactly that point about GPU). A node that does not report
		// ephemeral-storage is not failed on it -- an unreadable field must not
		// be turned into "no node fits".
		if diskRequested {
			if d, present := alloc[corev1.ResourceEphemeralStorage]; present &&
				d.Cmp(diskReq) < 0 {
				nodeCPUMem = false
			}
		}
		nodeGPU := !gpuRequested
		if gpuRequested {
			if q, present := alloc[gpuName]; present && q.Cmp(gpuReq) >= 0 {
				nodeGPU = true
			}
		}
		if nodeCPUMem {
			cpuMemFits = true
		}
		if nodeCPUMem && nodeGPU {
			fullFits = true
		}
		// The same whole-node fit with the running Jobs subtracted as well: can a
		// run schedule NOW? Asked only of a node that passed the steady-state fit
		// -- on one that did not, the permanent shortage is the finding and the
		// Job on it is not what is in the way. With free unknown there is no Job
		// sum either, so this collapses to the steady-state answer and the
		// `!freeKnown` arms below carry the caveat.
		nodeNow := nodeCPUMem
		if nodeCPUMem && freeKnown {
			nodeNow = freeCPUm-jobCPU[n.Name] >= cpuReq.MilliValue() &&
				freeMemB-jobMem[n.Name] >= memReq.Value()
			if !nodeNow && heldNode == "" {
				heldNode, heldCPUm, heldMemB = n.Name, jobCPU[n.Name], jobMem[n.Name]
			}
		}
		if nodeNow {
			nowFits = true
		}
	}

	switch {
	case !cpuMemFits && allocOnlyFit && freeKnown && (overCPU || overMem):
		// The #2870 case: a node IS big enough, but not beside the requests its own
		// control plane already holds. Allocatable said yes; free says no. This is
		// the shape that goes Pending/Insufficient memory after a clean install.
		short := "memory"
		switch {
		case overCPU && overMem:
			short = "cpu and memory"
		case overCPU:
			short = "cpu"
		}
		return Result{
			Name:   name,
			Status: StatusFail,
			Detail: fmt.Sprintf("%s for a training job (%s) but not beside what is already running on it — the envelope over-asks the node's FREE %s (%s has %s free beside the platform's own pods), so the pod schedules Pending", OverCommitted, req, short, overNode, cpuMemString(overFreeCPUm, overFreeMemB)),
			Remedy: "Lower RESOURCE_REQUESTS on jobs-manager to leave room for the platform's own pods, or move the control plane / add a node. The installer sizes the envelope from allocatable, not free, so a machine that is 'big enough' can still be over-committed (backend#2870).",
		}
	case !cpuMemFits:
		detail := fmt.Sprintf("no Ready node can fit a training job (needs %s)", req)
		if !freeKnown {
			detail += " — checked against allocatable only; the pod list could not be read, so an over-committed control plane would be invisible here"
		}
		return Result{
			Name:   name,
			Status: StatusFail,
			Detail: detail,
			Remedy: "Add/resize a node to meet the job's requests, or lower RESOURCE_REQUESTS on jobs-manager.",
		}
	case freeKnown && !nowFits:
		// The TRANSIENT shortage (backend#2870): every dimension fits beside the
		// platform's own pods, so this machine CAN run the envelope -- but a batch
		// Job holds the room at this moment, and the next run sits Pending until
		// it finishes. Before this arm the running Job was dropped from the sum and
		// this state read as an unqualified green, which is the doctor-side half of
		// the ticket's "waiting_for_capacity forever looks identical to a permanent
		// shortage".
		//
		// Warn, not Fail: training is genuinely running here, and failing the
		// command on the healthy state it exists to bless was the Bugbot High on
		// #628. Warn, not OK: the operator asking why a second run is waiting
		// needs the answer on this line, not a ✔.
		//
		// Its remedy is the OPPOSITE of the permanent arm's -- lowering the
		// envelope or resizing changes nothing about a Job that is already
		// running -- which is why the prefix is a distinct constant that the
		// rollup (summarizeDoctor) classifies on. It sits above the soft GPU Warn
		// because it is the stronger statement about scheduling; the GPU fact is
		// folded in rather than lost, as #628 did for the can't-check.
		detail := fmt.Sprintf("%s: a Ready node fits a training job (%s) beside the platform's own pods, but running job(s) on %s hold %s right now, so the next run waits Pending until they finish", HeldByRunningJob, req, heldNode, cpuMemString(heldCPUm, heldMemB))
		if gpuRequested && !fullFits {
			detail += fmt.Sprintf(". Also, no single Ready node satisfies cpu+memory AND %s, so GPU jobs would rely on the CPU fallback", gpuName)
		}
		return Result{
			Name:   name,
			Status: StatusWarn,
			Detail: detail,
			Remedy: "Nothing on the machine needs changing: let the running job finish, or stop it if it is not needed (kubectl get jobs -A). Lowering RESOURCE_REQUESTS or resizing does not free room a running job holds.",
		}
	case gpuRequested && !fullFits:
		// UNKNOWN FREE OUTRANKS THE SOFT GPU WARN (Bugbot High, #628).
		//
		// This case sits ABOVE the `!freeKnown` branch in the default arm, and its
		// detail carries no `CantVerifyFreeCompute` prefix -- so when the pod list
		// could not be read AND a GPU is requested, this Warn won, the can't-check
		// was never emitted, and `summarizeDoctor` fell through to "Ready to run
		// training" at exit 0. Doctor printed a clean bill over a cluster whose
		// free compute it had not looked at.
		//
		// It is not an exotic path: the chart stamps `nvidia.com/gpu` on CPU-only
		// installs too, so `gpuRequested` is commonly true where no node exposes a
		// GPU -- which is exactly this case.
		//
		// The GPU fallback stays SOFT and stays reported; it just no longer
		// suppresses the stronger statement. Both facts go in one Warn, with the
		// can't-check FIRST because the rollup matches on the prefix.
		if !freeKnown {
			return Result{
				Name:   name,
				Status: StatusWarn,
				Detail: fmt.Sprintf("%s, so free compute could not be verified — checked against allocatable only; an over-committed control plane would be invisible here. Also, no single Ready node satisfies cpu+memory AND %s, so GPU jobs would rely on the CPU fallback (needs %s)", CantVerifyFreeCompute, gpuName, req),
				Remedy: "Ensure your kubeconfig user can list pods cluster-wide, then re-run doctor to verify free capacity. If GPU training is expected, also ensure one node has both the compute and the GPU capacity, with its device plugin.",
			}
		}
		return Result{
			Name:   name,
			Status: StatusWarn,
			Detail: fmt.Sprintf("no single Ready node satisfies cpu+memory AND %s — GPU jobs rely on the CPU fallback (needs %s)", gpuName, req),
			Remedy: "If GPU training is expected, ensure one node has both the compute and the GPU capacity, with its device plugin.",
		}
	default:
		detail := fmt.Sprintf("a Ready node can schedule a training job (%s)", req)
		// backend#2223: report disk even when nothing requested it. Before this,
		// doctor measured no disk AT ALL -- and an ephemeral-storage eviction
		// was once reported to a user as "CPU Overload" (backend#2053), so the
		// dimension being invisible here has already cost a misdiagnosis.
		if sawDisk {
			detail += fmt.Sprintf(" — largest node offers ephemeral-storage=%s", bestDisk.String())
		}
		// Drift nudge (#400 / backend#1236): the install-time auto-size goes
		// stale when a machine GROWS. When the configured budget uses no more
		// than half of what this machine could give one run (largest node −
		// platform overhead), say so.
		//
		// BOUNDED BY FREE, NOT ALLOCATABLE. The ceiling is derived from the
		// largest node, but the verdict above is derived from what is FREE on
		// it. On a node that is big enough and already partly claimed those two
		// disagree, and the nudge won: an operator who ran `resources set max`
		// recreated the exact over-commit this check fails on, and the next
		// `doctor` told them not to size to their own machine. Advice that
		// contradicts the verdict printed beside it is worse than no advice.
		//
		// So the machine handed to `MaxRunCores`/`MaxRunGiB` is the smaller of
		// allocatable and free, and when free is UNKNOWN the nudge is suppressed
		// entirely -- there is nothing to bound it with, and this arm is already
		// a Warn that says free could not be verified.
		m := resources.Machine{CPU: bestCPU, Mem: bestMem}
		if freeKnown {
			freeCPU := *resource.NewMilliQuantity(bestFreeCPUm, resource.DecimalSI)
			freeMem := *resource.NewQuantity(bestFreeMemB, resource.BinarySI)
			if freeCPU.Cmp(m.CPU) < 0 {
				m.CPU = freeCPU
			}
			if freeMem.Cmp(m.Mem) < 0 {
				m.Mem = freeMem
			}
		}
		maxCores, maxGiB := resources.MaxRunCores(m), resources.MaxRunGiB(m)
		if freeKnown && maxCores >= 1 && maxGiB >= 2 &&
			cpuReq.MilliValue()*2 <= int64(maxCores)*1000 &&
			memReq.Value()*2 <= int64(maxGiB)<<30 {
			// NAME THE COMMAND THAT APPLIES THESE NUMBERS (Bugbot Medium).
			// Bounding the ceiling by free fixed the figure and left the
			// attribution behind: `resources set max` sizes from ALLOCATABLE via
			// `LargestReadyNode`, so on a claimed node it applies more than the
			// figure printed beside it -- and reapplies the over-commit this
			// check just started refusing. The printed numbers and the suggested
			// command have to be the same thing.
			//
			// So `set max` is named only when it would genuinely land on these
			// numbers -- i.e. nothing meaningful is claimed and the free-bounded
			// ceiling equals the allocatable one. Otherwise the explicit form is
			// named, which applies exactly what is printed.
			allocM := resources.Machine{CPU: bestCPU, Mem: bestMem}
			how := fmt.Sprintf("'tracebloc resources set --cores %d --memory %dGi'", maxCores, maxGiB)
			if maxCores == resources.MaxRunCores(allocM) && maxGiB == resources.MaxRunGiB(allocM) {
				how = "'tracebloc resources set max'"
			}
			detail += fmt.Sprintf(" — this machine could give a run up to cpu=%d,memory=%dGi (%s)", maxCores, maxGiB, how)
		}
		// UNKNOWN free is not a clean pass (Bugbot High). When the pod list could
		// not be read, this fit was against allocatable, not free -- so it cannot
		// assert schedulability, and an over-committed control plane would be
		// invisible. Warn and say so rather than greening node capacity.
		if !freeKnown {
			return Result{
				Name: name,
				// DISTINCT can't-check PREFIX so the rollup (summarizeDoctor in
				// cli/doctor.go) classifies this as "couldn't check free compute"
				// rather than greening it: it matches Node-capacity can't-checks by
				// prefix, and "a Ready node can schedule..." would fall through to
				// "Ready to run training" at exit 0 (Bugbot High). Keep the
				// "allocatable only" phrase the caveat and its test rely on.
				Status: StatusWarn,
				Detail: CantVerifyFreeCompute + ", so free compute could not be verified — checked against allocatable only; an over-committed control plane would be invisible here (the node fits the envelope on allocatable: " + req + ")",
				Remedy: "Ensure your kubeconfig user can list pods cluster-wide, then re-run doctor to verify free capacity.",
			}
		}
		return Result{Name: name, Status: StatusOK, Detail: detail}
	}
}

// OverCommitted is the prefix of the #2870 Fail: a node IS big enough, but not
// beside what its own control plane already holds. ONE definition, same reason
// as CantVerifyFreeCompute below.
//
// The rollup needs to tell this Fail apart from the generic "no node is big
// enough" one because the REMEDIES ARE OPPOSITE. `computeRemedy` ends every
// variant with "size runs to this machine with `tracebloc resources set max`",
// and `set max` sizes from ALLOCATABLE -- the exact figure this Fail rejected.
// Following it raises the envelope and the pod stays Pending (Bugbot Medium,
// #628). The generic arm is right for a too-small machine and wrong here.
const OverCommitted = "a Ready node is large enough"

// HeldByRunningJob is the prefix of the #2870 TRANSIENT Warn: the envelope fits
// this machine beside the platform, but a running batch Job holds the room now.
// ONE definition, for the same reason as OverCommitted -- the rollup classifies
// on it, and its remedy ("wait, or stop the job") is the opposite of both
// capacity Fails' ("shrink the envelope" / "grow the machine"). A retyped copy
// that drifted would send this state down the generic path again.
const HeldByRunningJob = "a running job holds the room"

// cpuMemString renders a millicore/byte pair the way RESOURCE_REQUESTS reads
// ("cpu=2, memory=8Gi"), so the free/held figures in a Node-capacity verdict
// line up with the request printed beside them.
func cpuMemString(cpuMilli, memBytes int64) string {
	return fmt.Sprintf("cpu=%s, memory=%s",
		resource.NewMilliQuantity(cpuMilli, resource.DecimalSI).String(),
		resource.NewQuantity(memBytes, resource.BinarySI).String())
}

// CantVerifyFreeCompute is the prefix every "we could not check free compute"
// Node-capacity Warn must start with, and the ONE definition of it.
//
// `summarizeDoctor` in internal/cli/doctor.go classifies a Node-capacity Warn as
// a can't-check by PREFIX, so the producer's wording is load-bearing: a Warn that
// does not start with this string falls through to "Ready to run training" at
// exit 0. That string was written out three times -- here, in the classifier, and
// in the classifier's test -- which is a rule the checks held their own copy of.
//
// It is exported rather than duplicated because the two live in different
// packages and internal/cli already imports this one. A third caller that needs
// the same classification must reference this, not retype it.
const CantVerifyFreeCompute = "could not read the pod list"

// checkImagePull verifies that any registry pull secret the jobs-manager
// references exists and is a well-formed dockerconfigjson — so private-image
// pulls don't ImagePullBackOff. (Bad-but-well-formed credentials can't be
// detected without an actual pull; this catches a missing/empty/malformed
// secret, the common misconfig.)
func checkImagePull(ctx context.Context, cs kubernetes.Interface, ns string, release *cluster.ParentRelease) Result {
	const name = "Image pull secret"
	dep := findDeployment(ctx, cs, ns, release, "jobs-manager")
	if dep == nil {
		return Result{
			Name:   name,
			Status: StatusWarn,
			Detail: "couldn't read jobs-manager to resolve image pull secrets — skipping",
			Remedy: "Check a tracebloc client is installed in " + ns + ".",
		}
	}
	secrets := dep.Spec.Template.Spec.ImagePullSecrets
	if len(secrets) == 0 {
		return Result{Name: name, Status: StatusOK, Detail: "no image pull secret in use (public/digest-pinned images)"}
	}
	for _, ref := range secrets {
		sec, err := cs.CoreV1().Secrets(ns).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return Result{
				Name:   name,
				Status: StatusFail,
				Detail: fmt.Sprintf("image pull secret %q not found", ref.Name),
				Remedy: "Private image pulls will ImagePullBackOff. Reinstall the chart with valid registry credentials.",
			}
		}
		if sec.Type != corev1.SecretTypeDockerConfigJson {
			return Result{
				Name:   name,
				Status: StatusFail,
				Detail: fmt.Sprintf("secret %q is type %q, not %s", ref.Name, sec.Type, corev1.SecretTypeDockerConfigJson),
				Remedy: "Recreate it as a docker-registry secret (kubectl create secret docker-registry).",
			}
		}
		if data := sec.Data[corev1.DockerConfigJsonKey]; len(data) == 0 || !json.Valid(data) {
			return Result{
				Name:   name,
				Status: StatusFail,
				Detail: fmt.Sprintf("secret %q has an empty or malformed %s", ref.Name, corev1.DockerConfigJsonKey),
				Remedy: "Recreate the registry secret; its .dockerconfigjson isn't valid JSON.",
			}
		}
	}
	return Result{Name: name, Status: StatusOK, Detail: fmt.Sprintf("%d image pull secret(s) present and well-formed", len(secrets))}
}

// parseResourceSpec parses jobs-manager's "k1=v1,k2=v2" resource env into a map.
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

// parseCPUMem extracts the cpu + memory quantities from a RESOURCE_REQUESTS
// spec; ok is false unless both are present and parseable.
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

// parseDisk extracts an ephemeral-storage request from a RESOURCE_REQUESTS spec.
//
// OPTIONAL, deliberately — shaped like parseGPU rather than parseCPUMem.
// backend#2223 widened the chart grammar to admit ephemeral-storage, but the
// installer does not write it and most edges will not carry it, so a missing
// disk request must leave node-fit behaving exactly as before rather than
// becoming a can't-read Warn.
//
// LIMIT OF WHAT doctor CAN SEE, stated because it is not obvious: this reads
// the jobs-manager Deployment's env, which is the DECLARED envelope. Since
// client-runtime#380, jobs-manager also applies a built-in ephemeral-storage
// default that never appears in that env — so an undeclared disk request is
// unknown here, not zero. Hence the node's own capacity is surfaced regardless
// (below), so disk is at least visible even when nothing declares it.
func parseDisk(spec string) (qty resource.Quantity, requested bool) {
	v, present := parseResourceSpec(spec)["ephemeral-storage"]
	if !present {
		return resource.Quantity{}, false
	}
	q, err := resource.ParseQuantity(v)
	if err != nil || q.IsZero() {
		return resource.Quantity{}, false
	}
	return q, true
}

// parseGPU extracts the GPU resource name + quantity from a GPU_REQUESTS spec
// (e.g. "nvidia.com/gpu=1"). requested is false when absent, unparseable, or 0.
func parseGPU(spec string) (name corev1.ResourceName, qty resource.Quantity, requested bool) {
	for k, v := range parseResourceSpec(spec) {
		q, err := resource.ParseQuantity(v)
		if err == nil && !q.IsZero() {
			return corev1.ResourceName(k), q, true
		}
	}
	return "", resource.Quantity{}, false
}

// nodeReady reports whether a node's Ready condition is True.
func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// jobsManagerEnv reads jobs-manager's first-container plain env into a map
// (valueFrom entries have no literal value and are skipped). Best-effort:
// returns an empty map when the deployment can't be fetched.
func jobsManagerEnv(ctx context.Context, cs kubernetes.Interface, ns string, release *cluster.ParentRelease) map[string]string {
	env := map[string]string{}
	dep := findDeployment(ctx, cs, ns, release, "jobs-manager")
	if dep == nil || len(dep.Spec.Template.Spec.Containers) == 0 {
		return env
	}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Value != "" {
			env[e.Name] = e.Value
		}
	}
	return env
}

// getDeployment returns the first of candidates that exists, or nil.
func getDeployment(ctx context.Context, cs kubernetes.Interface, ns string, candidates []string) *appsv1.Deployment {
	for _, n := range candidates {
		if n == "" {
			continue
		}
		d, err := cs.AppsV1().Deployments(ns).Get(ctx, n, metav1.GetOptions{})
		if err == nil {
			return d
		}
	}
	return nil
}

// findDeployment locates a chart component's Deployment ("<suffix>", e.g.
// "requests-proxy"), tied to the discovered release so a check can never be
// satisfied by a DIFFERENT release's component or a stray bare one (Bugbot on
// PR #89).
//
// Release known: take the chart's standard "<release>-<suffix>" name, or a bare
// "<suffix>" ONLY when its app.kubernetes.io/instance label ties it to this
// release (older unprefixed charts). A deployment belonging to another release
// is never accepted — if this release's component is missing, return nil and let
// the check report it.
//
// Release unknown (discovery failed): match by name suffix, but only when
// EXACTLY ONE deployment carries it. With several (multiple releases, which
// DiscoverParentRelease refuses to disambiguate) there's no safe attribution, so
// return nil rather than guess — which would let different checks describe
// different releases in one run.
func findDeployment(ctx context.Context, cs kubernetes.Interface, ns string, release *cluster.ParentRelease, suffix string) *appsv1.Deployment {
	if release != nil && release.ReleaseName != "" {
		if d := getDeployment(ctx, cs, ns, []string{release.ReleaseName + "-" + suffix}); d != nil {
			return d
		}
		if d := getDeployment(ctx, cs, ns, []string{suffix}); d != nil &&
			d.Labels["app.kubernetes.io/instance"] == release.ReleaseName {
			return d
		}
		return nil
	}

	deps, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var match *appsv1.Deployment
	for i := range deps.Items {
		if n := deps.Items[i].Name; n == suffix || strings.HasSuffix(n, "-"+suffix) {
			if match != nil {
				return nil // ambiguous across releases — don't guess
			}
			match = &deps.Items[i]
		}
	}
	return match
}

// httpProbe is the default backend prober: a GET with a short timeout over
// Go's default transport, which honors HTTP(S)_PROXY/NO_PROXY via
// http.ProxyFromEnvironment. Any HTTP response (even 4xx/5xx) means the host
// is reachable — we're testing connectivity, not the endpoint's health.
func httpProbe(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: httpProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	// Connected — the host is reachable regardless of status code. Discard the
	// close error so a rare post-connect close failure isn't reported as "down".
	_ = resp.Body.Close()
	return nil
}
