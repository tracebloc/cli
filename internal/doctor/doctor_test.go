package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/tracebloc/cli/internal/cluster"
)

const ns = "tracebloc"

func bg() context.Context { return context.Background() }

// jobsManagerDep mirrors the chart labels DiscoverParentRelease keys off
// (see internal/cluster/discover_test.go) so the fake clientset discovers it.
func jobsManagerDep(release string, env ...corev1.EnvVar) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release + "-jobs-manager",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "client",
				"app.kubernetes.io/instance":   release,
				"app.kubernetes.io/managed-by": "Helm",
				"app.kubernetes.io/version":    "1.3.5",
				"helm.sh/chart":                "client-1.3.5",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "api", Env: env}},
				},
			},
		},
	}
}

func requestsProxyDep(release string, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: release + "-requests-proxy", Namespace: ns},
		Status:     appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: ready},
	}
}

func boundPVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.SharedPVCClaimName, Namespace: ns},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func runningPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "c", RestartCount: 0}},
		},
	}
}

func crashPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "c",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
}

// succeededPod is a finished job pod that retried before completing — a high
// RestartCount here is historical, not a current crash-loop (Bugbot on #89).
func succeededPod(name string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{
			Phase:             corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "c", RestartCount: restarts}},
		},
	}
}

// recoveredPod restarted several times but its container is running again now —
// recovered, not crash-looping (cf. controller recovered-container fix, #117).
func recoveredPod(name string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "c",
				RestartCount: restarts,
				State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

// initRestartPod has an init container that restarted repeatedly but is not
// crash-looping now (it terminated Completed). Exercises the
// InitContainerStatuses arm of the restart-history scan.
func initRestartPod(name string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:         "init",
				RestartCount: restarts,
				State:        corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}},
			}},
		},
	}
}

func pendingPod(name string, age time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(age)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

// initCrashPod has an init container stuck in CrashLoopBackOff — the pod stays
// Pending and cannot start, so it must read as a failure, not a Pending warning
// (Bugbot on #89).
func initCrashPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  "init",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
}

// worstStatus mirrors the overall-verdict rollup for the Run() tests below: the
// most severe status across results, StatusUnknown ("couldn't check") ignored.
// Production derives the exit code in the cli layer from the two rolled-up health
// lines; this keeps the package tests' verdict assertions concise.
func worstStatus(results []Result) Status {
	worst := StatusOK
	for _, r := range results {
		if r.Status != StatusUnknown && r.Status > worst {
			worst = r.Status
		}
	}
	return worst
}

func TestCheckReachable(t *testing.T) {
	// A non-transport, non-chart error (e.g. RBAC) keeps the kubeconfig+chart
	// remedy and classifies as ReachError.
	if r := checkReachable(nil, errors.New("boom"), ns, ""); r.Status != StatusFail || r.Reach != ReachError {
		t.Fatalf("other error => status %v reach %v, want fail/ReachError", r.Status, r.Reach)
	}
	// No chart installed (the API answered) classifies as ReachNoEnv so the
	// summary points at the installer, never a kubectl (Bugbot #365).
	if r := checkReachable(nil, cluster.ErrNoParentRelease, ns, ""); r.Reach != ReachNoEnv {
		t.Fatalf("no-chart => reach %v, want ReachNoEnv", r.Reach)
	}
	rel := &cluster.ParentRelease{ReleaseName: "tb", ChartVersion: "1.3.5", AppVersion: "1.3.5"}
	r := checkReachable(rel, nil, ns, "")
	if r.Status != StatusOK || !strings.Contains(r.Detail, "tb") {
		t.Fatalf("release => %v / %q, want ok mentioning the release", r.Status, r.Detail)
	}

	// A transport error against a loopback server names the endpoint and gives
	// the start-the-cluster remedy — not the kubeconfig/chart one.
	tr := checkReachable(nil, errors.New(`Get "https://127.0.0.1:6550/api": dial tcp 127.0.0.1:6550: connect: connection refused`), ns, "https://127.0.0.1:6550")
	if tr.Status != StatusFail {
		t.Fatalf("transport => %v, want fail", tr.Status)
	}
	if !strings.Contains(tr.Detail, "127.0.0.1:6550") || !strings.Contains(tr.Detail, "isn't answering") {
		t.Fatalf("transport detail = %q, want it to name the unreachable endpoint", tr.Detail)
	}
	if !strings.Contains(tr.Remedy, "start") || strings.Contains(tr.Remedy, "kubectl get deploy") {
		t.Fatalf("transport remedy = %q, want a start-the-cluster hint, not the chart remedy", tr.Remedy)
	}

	// A stopped k3d cluster advertised as 0.0.0.0 (the kubeconfig host when the
	// installer pins no explicit --api-port host) must still get the start-Docker
	// remedy, not the generic "check it's running" line (Bugbot #365).
	wc := checkReachable(nil, errors.New(`Get "https://0.0.0.0:6550/api": dial tcp 0.0.0.0:6550: connect: connection refused`), ns, "https://0.0.0.0:6550")
	if wc.Reach != ReachUnreachable {
		t.Fatalf("0.0.0.0 transport => reach %v, want ReachUnreachable", wc.Reach)
	}
	if !strings.Contains(wc.Remedy, "Docker Desktop") {
		t.Fatalf("0.0.0.0 remedy = %q, want the start-Docker-Desktop hint", wc.Remedy)
	}
}

// TestIsLoopback covers every kubeconfig host that means "the cluster is local,
// so the fix is start Docker Desktop": the loopback addresses, the wildcard bind
// addresses k3d writes when no host is pinned (0.0.0.0, ::), and Docker Desktop's
// host alias — but never a genuinely-remote endpoint (Bugbot #365).
func TestIsLoopback(t *testing.T) {
	for _, s := range []string{
		"https://127.0.0.1:6550",
		"https://localhost:6550",
		"https://[::1]:6550",
		"https://0.0.0.0:6550",
		"https://[::]:6550",
		"https://host.docker.internal:6550",
	} {
		if !isLoopback(s) {
			t.Errorf("isLoopback(%q) = false, want true (local cluster)", s)
		}
	}
	for _, s := range []string{
		"https://api.k8s.example.com:6443",
		"https://10.1.2.3:6443",
		"",
	} {
		if isLoopback(s) {
			t.Errorf("isLoopback(%q) = true, want false (not a local cluster)", s)
		}
	}
}

// TestRun_UnreachableCascade mimics the reported failure: the cluster API is
// down (connection refused on every call). Run() must emit ONE honest ✖
// ("Cluster reachable", naming the endpoint) and mark the cluster checks
// StatusUnknown — never inventing "PVC unbound" / "requests-proxy not found →
// reinstall" / "chart too old". Backend egress still runs (it's independent of
// the cluster API), and the exit-code verdict stays Fail from the one real ✖.
func TestRun_UnreachableCascade(t *testing.T) {
	cs := fake.NewClientset()
	connRefused := errors.New(`Get "https://127.0.0.1:6550/apis": dial tcp 127.0.0.1:6550: connect: connection refused`)
	cs.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, connRefused
	})

	results := Run(bg(), cs, Options{
		Namespace: "lukas-test",
		ServerURL: "https://127.0.0.1:6550",
		HTTPProbe: func(context.Context, string) error { return nil }, // backend reachable from this machine
	})

	byName := map[string]Result{}
	for _, r := range results {
		byName[r.Name] = r
	}

	reach := byName["Cluster reachable"]
	if reach.Status != StatusFail || !strings.Contains(reach.Detail, "127.0.0.1:6550") {
		t.Fatalf("Cluster reachable = %+v, want fail naming the endpoint", reach)
	}
	if !strings.Contains(reach.Remedy, "start") {
		t.Errorf("Cluster reachable remedy = %q, want a start-the-cluster hint for a loopback server", reach.Remedy)
	}

	for _, name := range []string{
		"Pod health", "Restart history", "Dataset volume (PVC)", "Node capacity",
		"Image pull secret", "Proxy configuration", "Service Bus egress (requests-proxy)",
	} {
		r, ok := byName[name]
		if !ok {
			t.Fatalf("missing check %q in results", name)
		}
		if r.Status != StatusUnknown {
			t.Errorf("%q = %v, want StatusUnknown (couldn't check) — must not invent a definitive verdict", name, r.Status)
		}
		if r.Remedy != "" {
			t.Errorf("%q emitted remedy %q under an unreachable cluster — should stay silent", name, r.Remedy)
		}
	}

	if r := byName["Backend egress (from this machine)"]; r.Status != StatusOK {
		t.Errorf("Backend egress = %v, want ok (probed from this machine, independent of the cluster API)", r.Status)
	}
	if w := worstStatus(results); w != StatusFail {
		t.Fatalf("worst = %v, want fail (verdict from the one real ✖, StatusUnknown ignored)", w)
	}
}

func TestCheckPods(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want Status
	}{
		{"healthy", runningPod("ok"), StatusOK},
		{"crash-loop", crashPod("bad"), StatusFail},
		{"pending-old", pendingPod("stuck", -10*time.Minute), StatusWarn},
		{"pending-fresh", pendingPod("fresh", -time.Minute), StatusOK},
		{"succeeded-high-restarts", succeededPod("done", 5), StatusOK},
		{"recovered-running", recoveredPod("recovered", 5), StatusOK},
		{"init-crash-loop", initCrashPod("initbad"), StatusFail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset(tc.pod)
			if r := checkPods(bg(), cs, ns); r.Status != tc.want {
				t.Fatalf("checkPods = %v (%q), want %v", r.Status, r.Detail, tc.want)
			}
		})
	}
}

// checkRestartHistory is the backend#1028 restart-*history* signal: it must warn
// on a container whose RestartCount crossed the threshold even though it is not
// crash-looping now (checkPods reads only the current waiting reason and would
// call these OK). It scans both init and app container statuses and caps at ⚠.
func TestCheckRestartHistory(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want Status
	}{
		{"no restarts", runningPod("ok"), StatusOK},
		{"below threshold", recoveredPod("blip", restartWarnThreshold-1), StatusOK},
		{"at threshold", recoveredPod("flap", restartWarnThreshold), StatusWarn},
		{"above threshold", recoveredPod("flap", restartWarnThreshold+2), StatusWarn},
		{"init container restarts", initRestartPod("initflap", restartWarnThreshold), StatusWarn},
		// A recovered pod that flapped is OK to checkPods but ⚠ here — that's the gap.
		{"crash-loop-now stays OK here (not this check's job)", crashPod("bad"), StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset(tc.pod)
			r := checkRestartHistory(bg(), cs, ns)
			if r.Status != tc.want {
				t.Fatalf("checkRestartHistory = %v (%q), want %v", r.Status, r.Detail, tc.want)
			}
			// A warn must name the offending pod and container so the operator
			// knows where to look.
			if tc.want == StatusWarn &&
				(!strings.Contains(r.Detail, tc.pod.Name) || !strings.Contains(r.Detail, "restarted")) {
				t.Fatalf("warn detail %q should name pod %q and its restart count", r.Detail, tc.pod.Name)
			}
		})
	}
}

func TestCheckPVC(t *testing.T) {
	if r := checkPVC(bg(), fake.NewClientset(boundPVC()), ns); r.Status != StatusOK {
		t.Fatalf("bound PVC => %v, want ok", r.Status)
	}
	if r := checkPVC(bg(), fake.NewClientset(), ns); r.Status != StatusFail {
		t.Fatalf("missing PVC => %v, want fail", r.Status)
	}
}

func TestCheckProxy(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		want   Status
		substr string
	}{
		{"requests-proxy set", map[string]string{"REQUESTS_PROXY_URL": "http://requests-proxy-service:8888"}, StatusOK, "requests-proxy="},
		{"corporate proxy", map[string]string{"REQUESTS_PROXY_URL": "http://x", "HTTPS_PROXY": "http://corp:3128"}, StatusOK, "corporate HTTP(S)_PROXY set"},
		{"empty", map[string]string{}, StatusWarn, "REQUESTS_PROXY_URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := checkProxy(tc.env)
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.substr) {
				t.Fatalf("checkProxy = %v / %q, want %v containing %q", r.Status, r.Detail, tc.want, tc.substr)
			}
		})
	}
}

func TestCheckBackendEgress(t *testing.T) {
	okProbe := func(context.Context, string) error { return nil }
	failProbe := func(context.Context, string) error { return errors.New("dns failure") }

	if r := checkBackendEgress(bg(), map[string]string{"CLIENT_ENV": "dev"}, okProbe); r.Status != StatusOK || !strings.Contains(r.Detail, "dev-api.tracebloc.io") {
		t.Fatalf("reachable dev => %v / %q", r.Status, r.Detail)
	}
	if r := checkBackendEgress(bg(), map[string]string{}, failProbe); r.Status != StatusFail || !strings.Contains(r.Detail, "api.tracebloc.io") {
		t.Fatalf("unreachable default => %v / %q", r.Status, r.Detail)
	}
}

func TestBackendHost(t *testing.T) {
	tests := map[string]string{
		"dev":   "dev-api.tracebloc.io",
		"stg":   "stg-api.tracebloc.io",
		"prod":  "api.tracebloc.io",
		"":      "api.tracebloc.io",
		"weird": "api.tracebloc.io",
		// Case/space-insensitive, matching the API client's env resolution — a
		// non-lowercase CLIENT_ENV must not fall through to prod.
		"DEV":   "dev-api.tracebloc.io",
		"Stg":   "stg-api.tracebloc.io",
		" dev ": "dev-api.tracebloc.io",
	}
	for in, want := range tests {
		if got := backendHost(in); got != want {
			t.Errorf("backendHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckRequestsProxy(t *testing.T) {
	rel := &cluster.ParentRelease{ReleaseName: "tb"}
	tests := []struct {
		name string
		dep  *appsv1.Deployment // nil => deployment absent
		want Status
	}{
		// Ready is NOT a green ✔: readiness ≠ Service Bus egress works, so a running
		// relay is an honest StatusUnknown (egress not actively verified), never OK
		// (cli#351). A down / missing relay is still a real ✖.
		{"ready", requestsProxyDep("tb", 1), StatusUnknown},
		{"not-ready", requestsProxyDep("tb", 0), StatusFail},
		{"missing", nil, StatusFail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			if tc.dep != nil {
				cs = fake.NewClientset(tc.dep)
			}
			if r := checkRequestsProxy(bg(), cs, ns, rel); r.Status != tc.want {
				t.Fatalf("checkRequestsProxy = %v (%q), want %v", r.Status, r.Detail, tc.want)
			}
		})
	}
}

// When DiscoverParentRelease failed (release nil) but a release-prefixed
// requests-proxy exists, the suffix fallback must still find it rather than
// falsely report it missing (Bugbot on #89).
// TestCheckRequestsProxy_Wording locks the cli#351 reword + Part-2 downgrade:
// requests-proxy is the OUTBOUND result/FLOPs relay, so none of its lines may
// say experiments "stay Pending" (that's the scheduling path, which it doesn't
// touch); and a Ready relay must read as a neutral StatusUnknown (egress not
// actively verified), never a green ✔.
func TestCheckRequestsProxy_Wording(t *testing.T) {
	rel := &cluster.ParentRelease{ReleaseName: "tb"}

	ready := checkRequestsProxy(bg(), fake.NewClientset(requestsProxyDep("tb", 1)), ns, rel)
	if ready.Status != StatusUnknown {
		t.Errorf("Ready relay = %v, want StatusUnknown — readiness must not green a ✔ egress claim (cli#351)", ready.Status)
	}
	if strings.Contains(ready.Detail, "Pending") {
		t.Errorf("detail says %q — must not mention 'Pending' (that's scheduling, not egress)", ready.Detail)
	}
	if !strings.Contains(ready.Detail, "not actively verified") {
		t.Errorf("detail = %q, want it honest that egress is not actively verified", ready.Detail)
	}

	notReady := checkRequestsProxy(bg(), fake.NewClientset(requestsProxyDep("tb", 0)), ns, rel)
	missing := checkRequestsProxy(bg(), fake.NewClientset(), ns, rel)
	for _, r := range []Result{notReady, missing} {
		if strings.Contains(r.Remedy, "Pending") {
			t.Errorf("remedy %q must not say experiments 'stay Pending' — a down proxy stalls result egress mid-run", r.Remedy)
		}
		if !strings.Contains(r.Remedy, "result") {
			t.Errorf("remedy %q should describe the real failure (result/metrics egress stalls)", r.Remedy)
		}
	}
}

func TestCheckRequestsProxy_NilReleaseFindsPrefixed(t *testing.T) {
	cs := fake.NewClientset(requestsProxyDep("tb", 1)) // "tb-requests-proxy"
	// Found (not falsely reported missing) now reads as StatusUnknown — running,
	// egress not actively verified — rather than a green ✔ (cli#351).
	if r := checkRequestsProxy(bg(), cs, ns, nil); r.Status != StatusUnknown {
		t.Fatalf("nil release with prefixed deploy => %v (%q), want unknown (found, egress unverified)", r.Status, r.Detail)
	}
}

// With multiple parent releases in one namespace (the case DiscoverParentRelease
// refuses) and no discovered release, the suffix fallback must NOT pick one
// arbitrarily — guessing could let different checks describe different releases
// in a single run (Bugbot on #89). It should report can't-determine, not OK.
func TestCheckRequestsProxy_NilReleaseAmbiguous(t *testing.T) {
	cs := fake.NewClientset(
		requestsProxyDep("relA", 1), // "relA-requests-proxy"
		requestsProxyDep("relB", 1), // "relB-requests-proxy"
	)
	if r := checkRequestsProxy(bg(), cs, ns, nil); r.Status == StatusOK {
		t.Fatalf("ambiguous multi-release => %v (%q), want not-OK (no guessing)", r.Status, r.Detail)
	}
}

// With a release discovered, the check must be tied to THAT release: another
// release's requests-proxy must not be accepted as the discovered release's,
// or relA goes green on relB's proxy while relA's is actually missing
// (Bugbot on #89).
func TestCheckRequestsProxy_DiscoveredReleaseIgnoresOtherReleases(t *testing.T) {
	rel := &cluster.ParentRelease{ReleaseName: "relA"} // relA has no requests-proxy
	cs := fake.NewClientset(requestsProxyDep("relB", 1))
	if r := checkRequestsProxy(bg(), cs, ns, rel); r.Status == StatusOK {
		t.Fatalf("relA proxy missing, relB present => %v (%q), want not-OK", r.Status, r.Detail)
	}
}

// A bare (unprefixed) requests-proxy is accepted only when its instance label
// ties it to the discovered release — covering older unprefixed charts.
func TestCheckRequestsProxy_BareNameAcceptedWhenLabelledForRelease(t *testing.T) {
	rel := &cluster.ParentRelease{ReleaseName: "relA"}
	bare := requestsProxyDep("relA", 1)
	bare.Name = "requests-proxy"
	bare.Labels = map[string]string{"app.kubernetes.io/instance": "relA"}
	cs := fake.NewClientset(bare)
	// Accepted (found, not missing) → StatusUnknown, not a green ✔ (cli#351).
	if r := checkRequestsProxy(bg(), cs, ns, rel); r.Status != StatusUnknown {
		t.Fatalf("bare requests-proxy labelled for relA => %v (%q), want unknown (accepted/found, egress unverified)", r.Status, r.Detail)
	}
}

func TestRun_HealthyCluster(t *testing.T) {
	const rel = "tb"
	cs := fake.NewClientset(
		jobsManagerDep(rel,
			corev1.EnvVar{Name: "REQUESTS_PROXY_URL", Value: "http://requests-proxy-service:8888"},
			corev1.EnvVar{Name: "CLIENT_ENV", Value: "dev"},
			corev1.EnvVar{Name: "RESOURCE_REQUESTS", Value: "cpu=2,memory=8Gi"},
		),
		requestsProxyDep(rel, 1),
		boundPVC(),
		runningPod("tb-jobs-manager-abc"),
		node("n1", "4", "16Gi"),
	)

	results := Run(bg(), cs, Options{
		Namespace: ns,
		HTTPProbe: func(context.Context, string) error { return nil },
	})

	// 10 since backend#2221 added "Machine capacity". These nodes are not
	// k3d-named, so that check reports StatusUnknown — which the rollup ignores,
	// so the healthy verdict below is unaffected. That is the intended
	// behaviour on a non-local cluster, not an accident of the fixture.
	if len(results) != 10 {
		t.Fatalf("want 10 checks, got %d", len(results))
	}
	if w := worstStatus(results); w != StatusOK {
		for _, r := range results {
			t.Logf("%-32s %-4s %s", r.Name, r.Status, r.Detail)
		}
		t.Fatalf("healthy cluster worst = %v, want ok", w)
	}
}

func node(name, cpu, mem string, gpu ...string) *corev1.Node {
	alloc := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
	if len(gpu) == 2 {
		alloc[corev1.ResourceName(gpu[0])] = resource.MustParse(gpu[1])
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: alloc,
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func TestParseCPUMem(t *testing.T) {
	cpu, mem, ok := parseCPUMem("cpu=2,memory=8Gi")
	if !ok || cpu.String() != "2" || mem.String() != "8Gi" {
		t.Fatalf("parseCPUMem => %q %q %v", cpu.String(), mem.String(), ok)
	}
	if _, _, ok := parseCPUMem("cpu=2"); ok {
		t.Fatalf("missing memory should be !ok")
	}
	if _, _, ok := parseCPUMem("cpu=abc,memory=8Gi"); ok {
		t.Fatalf("unparseable cpu should be !ok")
	}
}

func TestParseGPU(t *testing.T) {
	name, qty, req := parseGPU("nvidia.com/gpu=1")
	if !req || string(name) != "nvidia.com/gpu" || qty.String() != "1" {
		t.Fatalf("parseGPU => %q %q %v", name, qty.String(), req)
	}
	if _, _, req := parseGPU("nvidia.com/gpu=0"); req {
		t.Fatalf("zero gpu should be !requested")
	}
	if _, _, req := parseGPU(""); req {
		t.Fatalf("empty should be !requested")
	}
}

func TestCheckNodeFit(t *testing.T) {
	full := map[string]string{"RESOURCE_REQUESTS": "cpu=2,memory=8Gi", "GPU_REQUESTS": "nvidia.com/gpu=1"}
	cpuOnly := map[string]string{"RESOURCE_REQUESTS": "cpu=2,memory=8Gi"}

	t.Run("fits cpu+mem+gpu", func(t *testing.T) {
		cs := fake.NewClientset(node("n1", "4", "16Gi", "nvidia.com/gpu", "2"))
		if r := checkNodeFit(bg(), cs, full); r.Status != StatusOK {
			t.Fatalf("=> %v (%q), want ok", r.Status, r.Detail)
		}
	})
	t.Run("no node big enough -> fail", func(t *testing.T) {
		cs := fake.NewClientset(node("n1", "1", "2Gi"))
		if r := checkNodeFit(bg(), cs, cpuOnly); r.Status != StatusFail {
			t.Fatalf("=> %v (%q), want fail", r.Status, r.Detail)
		}
	})
	t.Run("gpu requested but none -> warn", func(t *testing.T) {
		cs := fake.NewClientset(node("n1", "4", "16Gi")) // cpu/mem fit, no gpu
		if r := checkNodeFit(bg(), cs, full); r.Status != StatusWarn {
			t.Fatalf("=> %v (%q), want warn", r.Status, r.Detail)
		}
	})
	t.Run("cpu+mem and gpu on different nodes -> warn, not ok", func(t *testing.T) {
		// The Bugbot #91 case: one node fits cpu/mem, a different node has the
		// GPU but is too small. No single node runs a GPU job → must NOT be ok.
		cs := fake.NewClientset(
			node("big", "4", "16Gi"),                       // cpu/mem, no gpu
			node("gpu", "1", "1Gi", "nvidia.com/gpu", "2"), // gpu, too small
		)
		if r := checkNodeFit(bg(), cs, full); r.Status != StatusWarn {
			t.Fatalf("=> %v (%q), want warn (no single node fits all)", r.Status, r.Detail)
		}
	})
	t.Run("single node fits cpu+mem+gpu -> ok", func(t *testing.T) {
		cs := fake.NewClientset(
			node("big", "4", "16Gi"),                         // distractor: cpu/mem only
			node("full", "4", "16Gi", "nvidia.com/gpu", "1"), // satisfies everything
		)
		if r := checkNodeFit(bg(), cs, full); r.Status != StatusOK {
			t.Fatalf("=> %v (%q), want ok", r.Status, r.Detail)
		}
	})
	t.Run("not-ready node doesn't count -> fail", func(t *testing.T) {
		n := node("n1", "8", "32Gi")
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
		cs := fake.NewClientset(n)
		if r := checkNodeFit(bg(), cs, cpuOnly); r.Status != StatusFail {
			t.Fatalf("=> %v (%q), want fail (node not ready)", r.Status, r.Detail)
		}
	})
	t.Run("missing RESOURCE_REQUESTS -> warn", func(t *testing.T) {
		cs := fake.NewClientset(node("n1", "4", "16Gi"))
		if r := checkNodeFit(bg(), cs, map[string]string{}); r.Status != StatusWarn {
			t.Fatalf("=> %v (%q), want warn", r.Status, r.Detail)
		}
	})
	// Drift nudge (#400 / backend#1236): a budget using ≤ half of what the
	// largest node could give one run gets a resources-set-max pointer; a snug
	// fit stays quiet.
	t.Run("big machine + small budget -> ok with the resize nudge", func(t *testing.T) {
		cs := fake.NewClientset(node("n1", "32", "64Gi"))
		r := checkNodeFit(bg(), cs, cpuOnly) // 2/8Gi vs max 31/61
		if r.Status != StatusOK || !strings.Contains(r.Detail, "resources set max") {
			t.Fatalf("=> %v (%q), want ok with nudge", r.Status, r.Detail)
		}
	})
	t.Run("snug fit -> ok without the nudge", func(t *testing.T) {
		cs := fake.NewClientset(node("n1", "4", "12Gi"))
		r := checkNodeFit(bg(), cs, cpuOnly) // 2/8Gi vs max 3/9: memory over half
		if r.Status != StatusOK || strings.Contains(r.Detail, "resources set max") {
			t.Fatalf("=> %v (%q), want ok without nudge", r.Status, r.Detail)
		}
	})
	t.Run("heterogeneous nodes: nudge quotes the CPU-major node, matching set max (Bugbot)", func(t *testing.T) {
		// resources.LargestReadyNode (what `set max` applies) is CPU-major:
		// it picks cpuBig (32/64Gi -> max 31/61Gi), not memBig (8/128Gi ->
		// 7/125Gi). The advertised ceiling must be the SAME node's.
		cs := fake.NewClientset(
			node("cpuBig", "32", "64Gi"),
			node("memBig", "8", "128Gi"),
		)
		r := checkNodeFit(bg(), cs, cpuOnly)
		if r.Status != StatusOK || !strings.Contains(r.Detail, "cpu=31,memory=61Gi") {
			t.Fatalf("=> %v (%q), want the CPU-major node's ceiling cpu=31,memory=61Gi", r.Status, r.Detail)
		}
	})
}

// cpPod is a control-plane pod scheduled on a node, requesting memory — the
// neighbour whose requests the fit must subtract (backend#2870).
func cpPod(name, nodeName, mem string) *corev1.Pod {
	return podOn(name, nodeName, "", mem, nil)
}

// podOn is a running pod on a node requesting cpu/mem (empty = unset), with
// optional labels (e.g. the batch/v1 job-name label that marks a training pod).
func podOn(name, nodeName, cpu, mem string, labels map[string]string) *corev1.Pod {
	reqs := corev1.ResourceList{}
	if cpu != "" {
		reqs[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		reqs[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", Labels: labels},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: reqs}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// backend#2870: node-fit must be against FREE memory (allocatable − what is
// already requested on the node), not allocatable. A node big enough by
// allocatable but over-committed by the control plane it hosts must FAIL.
func TestCheckNodeFitFreeMemory(t *testing.T) {
	req := map[string]string{"RESOURCE_REQUESTS": "cpu=2,memory=8Gi"}

	t.Run("allocatable fits but FREE does not -> fail", func(t *testing.T) {
		// 16Gi node, control plane already claims 12Gi -> 4Gi free < 8Gi envelope.
		// Allocatable (16Gi) "fits"; free (4Gi) does not.
		cs := fake.NewClientset(node("n1", "4", "16Gi"), cpPod("cp", "n1", "12Gi"))
		r := checkNodeFit(bg(), cs, req)
		if r.Status != StatusFail {
			t.Fatalf("=> %v (%q), want fail (over-committed)", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "FREE") || !strings.Contains(r.Detail, "over-asks") {
			t.Fatalf("detail should name the free-memory over-commit: %q", r.Detail)
		}
	})

	// The subtraction is load-bearing: the SAME node without the neighbour passes,
	// so it is the pod request — not the node size — that flips the verdict.
	t.Run("same node without the neighbour -> ok", func(t *testing.T) {
		cs := fake.NewClientset(node("n1", "4", "16Gi"))
		if r := checkNodeFit(bg(), cs, req); r.Status != StatusOK {
			t.Fatalf("=> %v (%q), want ok", r.Status, r.Detail)
		}
	})

	// A terminal pod holds no memory; free stays 16Gi and the job fits.
	t.Run("terminal neighbour does not consume free", func(t *testing.T) {
		done := cpPod("old", "n1", "12Gi")
		done.Status.Phase = corev1.PodSucceeded
		cs := fake.NewClientset(node("n1", "4", "16Gi"), done)
		if r := checkNodeFit(bg(), cs, req); r.Status != StatusOK {
			t.Fatalf("=> %v (%q), want ok (terminal pod ignored)", r.Status, r.Detail)
		}
	})

	// A RUNNING training job (batch/v1 job-name label) holds the envelope itself.
	// It must NOT count against free, or doctor fails on the exact healthy state it
	// blesses (Bugbot High). Same 12Gi neighbour, but labelled a Job -> still ok.
	t.Run("a running training job is excluded, not counted -> ok", func(t *testing.T) {
		job := podOn("train-sim", "n1", "", "12Gi", map[string]string{"job-name": "exp-42"})
		cs := fake.NewClientset(node("n1", "4", "16Gi"), job)
		if r := checkNodeFit(bg(), cs, req); r.Status != StatusOK {
			t.Fatalf("=> %v (%q), want ok (training job excluded)", r.Status, r.Detail)
		}
	})

	// The over-commit message names the SHORT dimension, not always memory (Bugbot).
	// Control plane claims cpu (3 of 4), leaving 1 free < the 2-cpu envelope; memory
	// is fine. The message must say cpu, not memory.
	t.Run("over-commit on cpu names cpu, not memory", func(t *testing.T) {
		cp := podOn("cp", "n1", "3", "", nil) // 3 cpu, no memory
		cs := fake.NewClientset(node("n1", "4", "16Gi"), cp)
		r := checkNodeFit(bg(), cs, req) // needs cpu=2, memory=8Gi
		if r.Status != StatusFail {
			t.Fatalf("=> %v (%q), want fail (cpu over-commit)", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "FREE cpu") {
			t.Fatalf("detail should name FREE cpu, not memory: %q", r.Detail)
		}
	})

	// A DISK-only shortfall must not take the over-commit arm (Bugbot Medium):
	// free cpu+memory are fine, only ephemeral-storage is short, so the message
	// must name disk via the generic fail, not blame FREE memory.
	t.Run("disk-only shortfall is not blamed on memory", func(t *testing.T) {
		diskReq := map[string]string{"RESOURCE_REQUESTS": "cpu=2,memory=8Gi,ephemeral-storage=50Gi"}
		n := nodeWithDisk("n1", "4", "16Gi", "20Gi") // cpu+mem fit free; disk 20Gi < 50Gi
		cs := fake.NewClientset(n)
		r := checkNodeFit(bg(), cs, diskReq)
		if r.Status != StatusFail {
			t.Fatalf("=> %v (%q), want fail (disk)", r.Status, r.Detail)
		}
		if strings.Contains(r.Detail, "over-asks") || strings.Contains(r.Detail, "FREE memory") {
			t.Fatalf("disk shortfall must not be reported as a memory over-commit: %q", r.Detail)
		}
		if !strings.Contains(r.Detail, "ephemeral-storage") {
			t.Fatalf("detail should name ephemeral-storage: %q", r.Detail)
		}
	})

	// UNKNOWN free is not a clean pass (Bugbot High): when the pod list can't be
	// read, the fit falls back to allocatable and must WARN with a caveat, never
	// green node capacity as if free were verified.
	t.Run("unknown free (pod list fails) -> warn with caveat", func(t *testing.T) {
		cs := fake.NewClientset(node("n1", "4", "16Gi"))
		cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("pods is forbidden")
		})
		r := checkNodeFit(bg(), cs, req)
		if r.Status != StatusWarn {
			t.Fatalf("=> %v (%q), want warn (free unknown)", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "allocatable only") {
			t.Fatalf("detail should caveat allocatable-only: %q", r.Detail)
		}
	})
}

func dockerSecret(name string, data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: data},
	}
}

func jmDepWithPullSecret(release, secretName string) *appsv1.Deployment {
	d := jobsManagerDep(release)
	if secretName != "" {
		d.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: secretName}}
	}
	return d
}

func TestCheckImagePull(t *testing.T) {
	rel := &cluster.ParentRelease{ReleaseName: "tb"}

	t.Run("no pull secret -> ok", func(t *testing.T) {
		cs := fake.NewClientset(jmDepWithPullSecret("tb", ""))
		if r := checkImagePull(bg(), cs, ns, rel); r.Status != StatusOK {
			t.Fatalf("=> %v (%q), want ok", r.Status, r.Detail)
		}
	})
	t.Run("valid dockerconfigjson -> ok", func(t *testing.T) {
		cs := fake.NewClientset(
			jmDepWithPullSecret("tb", "reg"),
			dockerSecret("reg", []byte(`{"auths":{}}`)),
		)
		if r := checkImagePull(bg(), cs, ns, rel); r.Status != StatusOK {
			t.Fatalf("=> %v (%q), want ok", r.Status, r.Detail)
		}
	})
	t.Run("missing secret -> fail", func(t *testing.T) {
		cs := fake.NewClientset(jmDepWithPullSecret("tb", "reg")) // secret absent
		if r := checkImagePull(bg(), cs, ns, rel); r.Status != StatusFail {
			t.Fatalf("=> %v (%q), want fail", r.Status, r.Detail)
		}
	})
	t.Run("malformed dockerconfigjson -> fail", func(t *testing.T) {
		cs := fake.NewClientset(
			jmDepWithPullSecret("tb", "reg"),
			dockerSecret("reg", []byte("not json")),
		)
		if r := checkImagePull(bg(), cs, ns, rel); r.Status != StatusFail {
			t.Fatalf("=> %v (%q), want fail", r.Status, r.Detail)
		}
	})
}

// nodeWithDisk is `node` plus an ephemeral-storage allocatable. Separate helper
// rather than another variadic on `node`: that one already overloads its tail
// for GPU, and a second overload there would make every call site ambiguous.
func nodeWithDisk(name, cpu, mem, disk string) *corev1.Node {
	n := node(name, cpu, mem)
	n.Status.Allocatable[corev1.ResourceEphemeralStorage] = resource.MustParse(disk)
	return n
}

func TestParseDisk(t *testing.T) {
	qty, req := parseDisk("cpu=2,memory=8Gi,ephemeral-storage=20Gi")
	if !req || qty.String() != "20Gi" {
		t.Fatalf("parseDisk => %q %v, want 20Gi true", qty.String(), req)
	}
	// OPTIONAL, not required: the installer does not write a disk request, so
	// its absence must read as "not requested" and leave node-fit unchanged —
	// never as a can't-read Warn the way a missing cpu/memory does.
	if _, req := parseDisk("cpu=2,memory=8Gi"); req {
		t.Fatalf("absent ephemeral-storage should be !requested")
	}
	if _, req := parseDisk("ephemeral-storage=0"); req {
		t.Fatalf("zero should be !requested")
	}
	if _, req := parseDisk("ephemeral-storage=notaquantity"); req {
		t.Fatalf("unparseable should be !requested")
	}
}

func TestCheckNodeFitDisk(t *testing.T) {
	withDisk := map[string]string{
		"RESOURCE_REQUESTS": "cpu=2,memory=8Gi,ephemeral-storage=20Gi",
	}
	noDisk := map[string]string{"RESOURCE_REQUESTS": "cpu=2,memory=8Gi"}

	t.Run("disk fits -> ok", func(t *testing.T) {
		cs := fake.NewClientset(nodeWithDisk("n1", "4", "16Gi", "40Gi"))
		if r := checkNodeFit(bg(), cs, withDisk); r.Status != StatusOK {
			t.Fatalf("=> %v (%q), want ok", r.Status, r.Detail)
		}
	})

	t.Run("disk too small -> fail even though cpu+mem fit", func(t *testing.T) {
		// backend#2223. Before disk was a dimension here, this reported OK and
		// the run then died on an ephemeral-storage eviction — which is how
		// backend#2053 came to be reported to a user as "CPU Overload".
		cs := fake.NewClientset(nodeWithDisk("n1", "4", "16Gi", "5Gi"))
		r := checkNodeFit(bg(), cs, withDisk)
		if r.Status != StatusFail {
			t.Fatalf("=> %v (%q), want fail", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "ephemeral-storage=20Gi") {
			t.Fatalf("the requirement should name the disk request, got %q", r.Detail)
		}
	})

	t.Run("cpu+mem and disk on different nodes -> fail, not ok", func(t *testing.T) {
		// The Bugbot #91 whole-node rule, applied to the third dimension: a pod
		// gets every resource from ONE node, so these must never be OR-ed.
		cs := fake.NewClientset(
			nodeWithDisk("big", "4", "16Gi", "5Gi"),   // cpu/mem fit, disk small
			nodeWithDisk("disky", "1", "1Gi", "80Gi"), // disk fits, too small
		)
		if r := checkNodeFit(bg(), cs, withDisk); r.Status != StatusFail {
			t.Fatalf("=> %v (%q), want fail (no single node fits all three)", r.Status, r.Detail)
		}
	})

	t.Run("a node not reporting disk is not failed on it", func(t *testing.T) {
		// An unreadable field must not become "no node fits" — that would tell
		// the user to resize a machine on the strength of a value we could not
		// read. Same fail-open direction the rest of doctor takes.
		cs := fake.NewClientset(node("n1", "4", "16Gi")) // no ephemeral-storage
		if r := checkNodeFit(bg(), cs, withDisk); r.Status != StatusOK {
			t.Fatalf("=> %v (%q), want ok", r.Status, r.Detail)
		}
	})

	t.Run("disk is reported even when nothing requested it", func(t *testing.T) {
		// The ticket's actual complaint: "CLI doctor measures no disk at all."
		// Visibility must not depend on someone having declared a request.
		cs := fake.NewClientset(nodeWithDisk("n1", "4", "16Gi", "40Gi"))
		r := checkNodeFit(bg(), cs, noDisk)
		if r.Status != StatusOK {
			t.Fatalf("=> %v (%q), want ok", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "ephemeral-storage=40Gi") {
			t.Fatalf("disk should be surfaced even when undeclared, got %q", r.Detail)
		}
	})

	t.Run("no disk anywhere -> detail stays as it was", func(t *testing.T) {
		// Existing single-dimension edges must not gain a dangling clause.
		cs := fake.NewClientset(node("n1", "4", "16Gi"))
		r := checkNodeFit(bg(), cs, noDisk)
		if strings.Contains(r.Detail, "ephemeral-storage") {
			t.Fatalf("no disk anywhere should mention none, got %q", r.Detail)
		}
	})
}
