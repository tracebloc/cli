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
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/tracebloc/cli/internal/cluster"
)

// Status is a single check's severity. Ordered so the numerically-greatest
// status is the worst — see Worst.
type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
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
}

// Worst returns the most severe status across results (StatusOK for none).
// The doctor command maps a non-OK worst to a non-zero exit code.
func Worst(results []Result) Status {
	worst := StatusOK
	for _, r := range results {
		if r.Status > worst {
			worst = r.Status
		}
	}
	return worst
}

// Conservative tunables, kept as package consts to avoid false positives on a
// busy cluster. If a check ever needs runtime tuning, thread it through Options
// (like HTTPProbe) rather than making these package vars.
const (
	pendingGrace     = 5 * time.Minute // a pod Pending longer than this is flagged
	httpProbeTimeout = 8 * time.Second
)

// Options configures a diagnosis run. The zero value is usable: Namespace
// defaults are the caller's concern (it passes the resolved namespace), and
// HTTPProbe falls back to the real proxy-aware prober.
type Options struct {
	Namespace string

	// HTTPProbe reports whether a URL is reachable from where the CLI runs.
	// nil => httpProbe (proxy-aware, short timeout). Injected in tests.
	HTTPProbe func(ctx context.Context, url string) error
}

// Run executes every check in display order and returns their results. It
// never returns an error: an unreachable cluster or a failing probe is a
// Result, not a Go error — the command renders all of them and derives the
// exit code from Worst.
func Run(ctx context.Context, cs kubernetes.Interface, opts Options) []Result {
	if opts.HTTPProbe == nil {
		opts.HTTPProbe = httpProbe
	}
	ns := opts.Namespace

	// Discovered once and shared: the parent release gates nothing (every
	// check still runs and reports), but the later checks reuse it.
	release, relErr := cluster.DiscoverParentRelease(ctx, cs, ns)
	jmEnv := jobsManagerEnv(ctx, cs, ns, release)

	return []Result{
		checkReachable(release, relErr, ns),
		checkPods(ctx, cs, ns),
		checkPVC(ctx, cs, ns),
		checkProxy(jmEnv),
		checkBackendEgress(ctx, jmEnv, opts.HTTPProbe),
		checkRequestsProxy(ctx, cs, ns, release),
	}
}

// checkReachable confirms the API answered and the parent client chart is
// installed here. It's the gate the customer reads first; the rest still run.
func checkReachable(release *cluster.ParentRelease, err error, ns string) Result {
	const name = "Cluster reachable"
	if err != nil {
		return Result{
			Name:   name,
			Status: StatusFail,
			Detail: err.Error(),
			Remedy: "Check your kubeconfig/context and that the tracebloc client chart is installed here: kubectl get deploy -n " + ns,
		}
	}
	return Result{
		Name:   name,
		Status: StatusOK,
		Detail: fmt.Sprintf("release %q, chart %s, appVersion %s (namespace %s)",
			release.ReleaseName, release.ChartVersion, release.AppVersion, ns),
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
func backendHost(clientEnv string) string {
	switch clientEnv {
	case "dev":
		return "dev-api.tracebloc.io"
	case "stg":
		return "stg-api.tracebloc.io"
	default:
		return "api.tracebloc.io"
	}
}

// checkRequestsProxy verifies the requests-proxy deployment — the in-cluster
// broker for experiment egress (the Service Bus "experiments" queue) — is
// present and Ready. While it's down, experiments egress fails and the
// experiment silently stays Pending, the exact class this epic targets.
func checkRequestsProxy(ctx context.Context, cs kubernetes.Interface, ns string, release *cluster.ParentRelease) Result {
	const name = "Service Bus egress (requests-proxy)"
	dep := findDeployment(ctx, cs, ns, requestsProxyNames(release), "requests-proxy")
	if dep == nil {
		return Result{
			Name:   name,
			Status: StatusFail,
			Detail: "requests-proxy deployment not found",
			Remedy: "The requests-proxy brokers experiment (Service Bus) egress; without it experiments stay Pending. Reinstall/upgrade the client chart.",
		}
	}
	if dep.Status.ReadyReplicas < 1 {
		return Result{
			Name:   name,
			Status: StatusFail,
			Detail: fmt.Sprintf("requests-proxy not ready (%d/%d replicas)", dep.Status.ReadyReplicas, dep.Status.Replicas),
			Remedy: "Experiments egress flows through requests-proxy; while it's down they stay Pending. kubectl describe deploy " + dep.Name + " -n " + ns,
		}
	}
	return Result{Name: name, Status: StatusOK, Detail: "requests-proxy ready (brokers the 'experiments' queue)"}
}

// jobsManagerEnv reads jobs-manager's first-container plain env into a map
// (valueFrom entries have no literal value and are skipped). Best-effort:
// returns an empty map when the deployment can't be fetched.
func jobsManagerEnv(ctx context.Context, cs kubernetes.Interface, ns string, release *cluster.ParentRelease) map[string]string {
	env := map[string]string{}
	dep := findDeployment(ctx, cs, ns, jobsManagerNames(release), "jobs-manager")
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

// findDeployment returns the first existing deployment among candidates (exact
// Get), falling back to a namespace-wide list matched by name suffix. The
// fallback matters when the parent release couldn't be discovered (release nil
// => only the unprefixed candidate name), yet the chart installed a
// release-prefixed deployment like "<release>-requests-proxy" — e.g. when
// multiple parent releases were detected (Bugbot on PR #89).
func findDeployment(ctx context.Context, cs kubernetes.Interface, ns string, candidates []string, suffix string) *appsv1.Deployment {
	if d := getDeployment(ctx, cs, ns, candidates); d != nil {
		return d
	}
	deps, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	for i := range deps.Items {
		if n := deps.Items[i].Name; n == suffix || strings.HasSuffix(n, "-"+suffix) {
			return &deps.Items[i]
		}
	}
	return nil
}

// jobsManagerNames / requestsProxyNames mirror the chart's naming: the
// release-prefixed form first, then the bare form for older/unprefixed charts.
func jobsManagerNames(release *cluster.ParentRelease) []string {
	names := []string{"jobs-manager"}
	if release != nil && release.ReleaseName != "" {
		names = append([]string{release.ReleaseName + "-jobs-manager"}, names...)
	}
	return names
}

func requestsProxyNames(release *cluster.ParentRelease) []string {
	names := []string{"requests-proxy"}
	if release != nil && release.ReleaseName != "" {
		names = append([]string{release.ReleaseName + "-requests-proxy"}, names...)
	}
	return names
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
