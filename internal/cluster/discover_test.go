package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// jobsManagerDeployment builds the minimal Deployment the chart
// creates for jobs-manager — labels mirror the chart's
// _helpers.tpl output as of client 1.3.5. Tests use this to seed
// the fake clientset with realistic data; if the chart's label
// contract ever changes, these tests fail loudly and we update both
// here and the discovery logic in lockstep.
//
// Note: app.kubernetes.io/name is "client" (the chart name) on
// EVERY resource the chart creates, not "jobs-manager". This is
// the helm convention. The discovery logic distinguishes
// jobs-manager from its mysql / requests-proxy siblings by name
// suffix matching ("-jobs-manager") since the chart-level labels
// don't disambiguate.
func jobsManagerDeployment(release, namespace, chartLabel, appVersion, digest string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release + "-jobs-manager",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "client",
				"app.kubernetes.io/instance":   release,
				"app.kubernetes.io/managed-by": "Helm",
				"app.kubernetes.io/version":    appVersion,
				"helm.sh/chart":                chartLabel,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "api",
						Env: []corev1.EnvVar{
							{Name: "INGESTOR_IMAGE_DIGEST", Value: digest},
						},
					}},
				},
			},
		},
	}
}

func jobsManagerService(name, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func TestDiscoverParentRelease_HappyPath(t *testing.T) {
	const ns = "tracebloc"
	cs := fake.NewClientset(
		jobsManagerDeployment("tracebloc", ns,
			"client-1.3.5", "1.3.5",
			"sha256:463e236748708a5e3564569eec9173ea8cb3bcf515992d4939c5b610f3807a4a"),
		jobsManagerService("jobs-manager", ns),
	)

	release, err := DiscoverParentRelease(context.Background(), cs, ns)
	if err != nil {
		t.Fatalf("DiscoverParentRelease: %v", err)
	}

	want := ParentRelease{
		ReleaseName:            "tracebloc",
		ChartVersion:           "1.3.5",
		AppVersion:             "1.3.5",
		JobsManagerService:     "http://jobs-manager." + ns + ".svc.cluster.local:8080",
		JobsManagerServiceName: "jobs-manager",
		JobsManagerPort:        8080,
		IngestorSAName:         "ingestor",
		IngestorImageDigest:    "sha256:463e236748708a5e3564569eec9173ea8cb3bcf515992d4939c5b610f3807a4a",
	}
	if *release != want {
		t.Errorf("mismatch.\ngot:  %+v\nwant: %+v", *release, want)
	}
}

// clientSecret builds the chart's `<release>-secrets` Secret carrying CLIENT_ID
// (the live client's UUID username). extraLabels lets a test mimic the
// node-agents mirror, which shares the labels + CLIENT_ID in another namespace.
func clientSecret(release, namespace, clientID string, extraLabels map[string]string) *corev1.Secret {
	labels := map[string]string{
		"app.kubernetes.io/name":       "client",
		"app.kubernetes.io/instance":   release,
		"app.kubernetes.io/managed-by": "Helm",
	}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: release + "-secrets", Namespace: namespace, Labels: labels},
		Data:       map[string][]byte{"CLIENT_ID": []byte(clientID)},
	}
}

func TestDiscoverInClusterClientID_HappyPath(t *testing.T) {
	const ns = "tracebloc"
	cs := fake.NewClientset(
		jobsManagerDeployment("tracebloc", ns, "client-1.3.5", "1.3.5", "sha256:x"),
		clientSecret("tracebloc", ns, "uuid-live", nil),
		// node-agents mirror: same labels + CLIENT_ID, DIFFERENT namespace. The read
		// is scoped to the jobs-manager namespace, so this must be ignored.
		clientSecret("tracebloc", "node-agents", "uuid-live", map[string]string{"app": "resource-monitor"}),
	)
	got, err := DiscoverInClusterClientID(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ClientID != "uuid-live" || got.Namespace != ns {
		t.Errorf("got %+v, want {ClientID:uuid-live Namespace:%s}", got, ns)
	}
}

func TestDiscoverInClusterClientID_NoRelease(t *testing.T) {
	got, err := DiscoverInClusterClientID(context.Background(), fake.NewClientset())
	if err != nil || got != nil {
		t.Errorf("empty cluster: want (nil,nil), got (%+v,%v)", got, err)
	}
}

func TestDiscoverInClusterClientID_ReleaseButNoSecret(t *testing.T) {
	cs := fake.NewClientset(jobsManagerDeployment("tracebloc", "tracebloc", "client-1.3.5", "1.3.5", "d"))
	got, err := DiscoverInClusterClientID(context.Background(), cs)
	if err != nil || got != nil {
		t.Errorf("release but no secret: want (nil,nil), got (%+v,%v)", got, err)
	}
}

func TestDiscoverParentRelease_NoReleaseFound(t *testing.T) {
	cs := fake.NewClientset() // empty cluster

	_, err := DiscoverParentRelease(context.Background(), cs, "tracebloc")
	if err == nil {
		t.Fatal("expected error when no jobs-manager deployment exists")
	}
	// The error message has to be customer-actionable. Pin the
	// key remediation phrase so a future refactor that loses it
	// (or worse, replaces it with a stack trace) fails this test.
	for _, want := range []string{"no tracebloc client found", "--namespace", "https://tracebloc.io/i.sh", "cluster doctor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to mention %q, got: %s", want, err)
		}
	}
	// The sentinel gates the §7.3 "runs elsewhere" rewrite (cli#128) — a
	// genuine not-found must be errors.Is-identifiable.
	if !errors.Is(err, ErrNoParentRelease) {
		t.Errorf("not-found error should match ErrNoParentRelease, got: %v", err)
	}
}

func TestDiscoverParentRelease_MultipleReleases(t *testing.T) {
	const ns = "tracebloc"
	cs := fake.NewClientset(
		jobsManagerDeployment("rel-a", ns, "client-1.3.5", "1.3.5", ""),
		jobsManagerDeployment("rel-b", ns, "client-1.3.4", "1.3.4", ""),
	)

	_, err := DiscoverParentRelease(context.Background(), cs, ns)
	if err == nil {
		t.Fatal("expected error when multiple releases exist")
	}
	// Customer needs to see which releases the CLI found so they
	// can pick (or clean up).
	for _, want := range []string{"found 2", "rel-a", "rel-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to mention %q, got: %s", want, err)
		}
	}
	// An ambiguous match is NOT "no release" — it must not trip the §7.3
	// rewrite (the cluster does host tracebloc, just more than one).
	if errors.Is(err, ErrNoParentRelease) {
		t.Error("multiple-release error must not match ErrNoParentRelease")
	}
}

// Regression for the real-cluster discovery bug. Pre-fix, the
// selector was `app.kubernetes.io/name=jobs-manager` which never
// matches because the chart's convention is `name=<chart-name>`
// — so the real-cluster smoke test returned "no parent release
// found" even though one was clearly running. The fix is two-part:
// match on `name=client` (the chart name) AND filter the result
// set by Deployment-name suffix to pick out jobs-manager from its
// mysql/requests-proxy siblings, all of which share the
// chart-level labels.
func TestDiscoverParentRelease_FiltersOutSiblingDeployments(t *testing.T) {
	const ns = "tracebloc"
	// All three deployments share the chart-level labels — only
	// the name suffix distinguishes them.
	mysqlClient := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mysql-client",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "client",
				"app.kubernetes.io/instance":   "tracebloc",
				"app.kubernetes.io/managed-by": "Helm",
				"helm.sh/chart":                "client-1.3.5",
			},
		},
	}
	requestsProxy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tracebloc-requests-proxy",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "client",
				"app.kubernetes.io/instance":   "tracebloc",
				"app.kubernetes.io/managed-by": "Helm",
				"helm.sh/chart":                "client-1.3.5",
			},
		},
	}

	cs := fake.NewClientset(
		mysqlClient,
		requestsProxy,
		jobsManagerDeployment("tracebloc", ns, "client-1.3.5", "1.3.5", ""),
		jobsManagerService("jobs-manager", ns),
	)

	release, err := DiscoverParentRelease(context.Background(), cs, ns)
	if err != nil {
		t.Fatalf("DiscoverParentRelease: %v", err)
	}
	// Should pick jobs-manager, NOT mysql-client or requests-proxy.
	if release.ReleaseName != "tracebloc" {
		t.Errorf("ReleaseName = %q, want %q (siblings should be filtered out by name suffix)",
			release.ReleaseName, "tracebloc")
	}
}

func TestDiscoverParentRelease_FallsBackToReleasePrefixedService(t *testing.T) {
	// Some older chart versions exposed the Service as
	// "<release>-jobs-manager" rather than the unprefixed
	// "jobs-manager". The discover code probes both names and
	// picks whichever resolves. This test seeds ONLY the
	// release-prefixed Service and asserts the FQDN reflects it.
	const ns = "tracebloc"
	cs := fake.NewClientset(
		jobsManagerDeployment("custom", ns, "client-1.3.5", "1.3.5", ""),
		jobsManagerService("custom-jobs-manager", ns),
		// NOTE: no "jobs-manager" service
	)

	release, err := DiscoverParentRelease(context.Background(), cs, ns)
	if err != nil {
		t.Fatalf("DiscoverParentRelease: %v", err)
	}
	wantFQDN := "http://custom-jobs-manager." + ns + ".svc.cluster.local:8080"
	if release.JobsManagerService != wantFQDN {
		t.Errorf("JobsManagerService = %q, want %q", release.JobsManagerService, wantFQDN)
	}
}

func TestChartVersionFromLabel(t *testing.T) {
	cases := map[string]string{
		"client-1.3.5":    "1.3.5",
		"client-2.0.0-rc": "2.0.0-rc",
		"client":          "client",      // unexpected shape — return as-is
		"other-1.0.0":     "other-1.0.0", // non-client chart — return as-is
		"":                "",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := chartVersionFromLabel(in); got != want {
				t.Errorf("chartVersionFromLabel(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

// FindClientNamespaces backs the cluster-wide fallback scan (§7.3): a miss in
// the kubeconfig's default namespace must find the client in its slug
// namespace instead of dead-ending. These pin the scan's filtering + ordering.
func TestFindClientNamespaces(t *testing.T) {
	cs := fake.NewSimpleClientset(
		jobsManagerDeployment("tracebloc", "lukas-01", "client-1.6.0", "1.6.0", ""),
		jobsManagerDeployment("tracebloc", "zeta-ns", "client-1.6.0", "1.6.0", ""),
		// a chart-labeled sibling that is NOT a jobs-manager must not count
		siblingDeployment("mysql-client", "other-ns"),
	)
	got, err := FindClientNamespaces(context.Background(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"lukas-01", "zeta-ns"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("expected sorted namespaces %v, got %v", want, got)
	}
}

func TestFindClientNamespaces_NoneFound(t *testing.T) {
	cs := fake.NewSimpleClientset(siblingDeployment("mysql-client", "somewhere"))
	got, err := FindClientNamespaces(context.Background(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no namespaces, got %v", got)
	}
}

// siblingDeployment builds a chart-labeled Deployment that is not a
// jobs-manager — the scan must ignore it.
func siblingDeployment(name, namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "client",
				"app.kubernetes.io/managed-by": "Helm",
			},
		},
	}
}
