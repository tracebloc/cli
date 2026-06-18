package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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

func TestWorst(t *testing.T) {
	if got := Worst(nil); got != StatusOK {
		t.Fatalf("Worst(nil) = %v, want ok", got)
	}
	rs := []Result{{Status: StatusOK}, {Status: StatusFail}, {Status: StatusWarn}}
	if got := Worst(rs); got != StatusFail {
		t.Fatalf("Worst = %v, want fail", got)
	}
}

func TestCheckReachable(t *testing.T) {
	if r := checkReachable(nil, errors.New("boom"), ns); r.Status != StatusFail {
		t.Fatalf("error => %v, want fail", r.Status)
	}
	rel := &cluster.ParentRelease{ReleaseName: "tb", ChartVersion: "1.3.5", AppVersion: "1.3.5"}
	r := checkReachable(rel, nil, ns)
	if r.Status != StatusOK || !strings.Contains(r.Detail, "tb") {
		t.Fatalf("release => %v / %q, want ok mentioning the release", r.Status, r.Detail)
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
		{"ready", requestsProxyDep("tb", 1), StatusOK},
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
func TestCheckRequestsProxy_NilReleaseFindsPrefixed(t *testing.T) {
	cs := fake.NewClientset(requestsProxyDep("tb", 1)) // "tb-requests-proxy"
	if r := checkRequestsProxy(bg(), cs, ns, nil); r.Status != StatusOK {
		t.Fatalf("nil release with prefixed deploy => %v (%q), want ok", r.Status, r.Detail)
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
	if r := checkRequestsProxy(bg(), cs, ns, rel); r.Status != StatusOK {
		t.Fatalf("bare requests-proxy labelled for relA => %v (%q), want ok", r.Status, r.Detail)
	}
}

func TestRun_HealthyCluster(t *testing.T) {
	const rel = "tb"
	cs := fake.NewClientset(
		jobsManagerDep(rel,
			corev1.EnvVar{Name: "REQUESTS_PROXY_URL", Value: "http://requests-proxy-service:8888"},
			corev1.EnvVar{Name: "CLIENT_ENV", Value: "dev"},
		),
		requestsProxyDep(rel, 1),
		boundPVC(),
		runningPod("tb-jobs-manager-abc"),
	)

	results := Run(bg(), cs, Options{
		Namespace: ns,
		HTTPProbe: func(context.Context, string) error { return nil },
	})

	if len(results) != 6 {
		t.Fatalf("want 6 checks, got %d", len(results))
	}
	if w := Worst(results); w != StatusOK {
		for _, r := range results {
			t.Logf("%-32s %-4s %s", r.Name, r.Status, r.Detail)
		}
		t.Fatalf("healthy cluster worst = %v, want ok", w)
	}
}
