package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gopkg.in/yaml.v3"
)

// ErrNoParentRelease is the sentinel for DiscoverParentRelease's "the namespace
// has no tracebloc release" case — distinct from an API/RBAC list failure or an
// ambiguous multiple-release match. Callers use errors.Is to tell "this cluster
// doesn't host the release" apart from "couldn't determine the release."
var ErrNoParentRelease = errors.New("no tracebloc client found")

// ParentRelease describes the tracebloc parent client chart release
// discovered in the customer's cluster. The information comes from
// the helm-managed Deployment's labels (and a few of its env vars),
// not from the helm release secret — parsing those secrets requires
// either pulling in helm.sh/helm/v3 (massive dep) or re-implementing
// the gzip+base64+JSON unwrap. The chart's Deployment labels carry
// everything we actually need, so we use those instead.
type ParentRelease struct {
	// ReleaseName is the helm release name. The chart's
	// jobs-manager Deployment is named "<release>-jobs-manager" by
	// convention and labelled `app.kubernetes.io/instance=<release>`.
	ReleaseName string

	// ChartVersion is the parent chart's version, e.g. "1.3.5".
	// Comes from the `helm.sh/chart=client-1.3.5` label.
	ChartVersion string

	// AppVersion mirrors Chart.yaml's appVersion. Useful for
	// detecting whether the customer is on a chart that supports
	// the declarative ingestor flow at all.
	AppVersion string

	// JobsManagerService is the in-cluster DNS name of the
	// jobs-manager Service, e.g.
	// "<release>-jobs-manager.<namespace>.svc.cluster.local:8080".
	// Used as the POST target for ingestion submissions WHEN
	// the CLI runs in-cluster (e.g. CI inside the same cluster).
	// For laptop / off-cluster use, the orchestrator port-forwards
	// to JobsManagerServiceName + JobsManagerPort instead.
	JobsManagerService string

	// JobsManagerServiceName + JobsManagerPort are the bare Service
	// reference for off-cluster port-forwarding (Bugbot PR #10 r3).
	// The FQDN-based JobsManagerService URL above doesn't resolve
	// from a laptop; the port-forward path uses these to set up a
	// localhost tunnel via the kubeconfig API server.
	JobsManagerServiceName string
	JobsManagerPort        int

	// IngestorSAName is the name of the ServiceAccount the CLI mints a
	// jobs-manager token for and runs its stage/teardown pods as. It is
	// discovered from the chart's ingestionAuthz ConfigMap — the policy
	// jobs-manager actually enforces (see discoverIngestorSAName, #7) — and
	// defaults to the chart default "ingestor" when the ConfigMap is absent
	// (older chart), unreadable, or ambiguous.
	IngestorSAName string

	// IngestorImageDigest is the canonical digest the cluster's
	// chart will spawn ingestor Jobs with. Comes from
	// `INGESTOR_IMAGE_DIGEST` env on jobs-manager. Empty when the
	// admin hasn't set images.ingestor.digest yet.
	IngestorImageDigest string
}

// DiscoverParentRelease finds the tracebloc parent client chart
// release in the given namespace by looking for a Deployment with
// the chart's hallmark labels. Returns a friendly error if none
// found, or if multiple candidates exist (so customers can pick).
//
// Why labels on a Deployment, not Helm's release secrets:
//
//   - The release-secret path needs gzip + base64 + JSON parsing
//     (helm v3's storage format), or pulls in helm.sh/helm/v3 as
//     a dep (which transitively pulls in ~80MB of Go modules,
//     including kube-runtime).
//   - The Deployment is what the chart actually deploys; if it's
//     not there, the chart isn't installed (or the install
//     failed). Using its labels for discovery means "the
//     discovered release is the running release" by construction.
//   - The chart's labels are part of its public contract via
//     `_helpers.tpl`; they're stable across minor versions.
//
// The chart's labels share `app.kubernetes.io/name=client` across
// EVERY resource it creates (mysql-client, jobs-manager,
// requests-proxy, etc.) — that's the helm convention where the
// chart's name is the label, not the component's. To distinguish
// jobs-manager from its siblings we filter by Deployment name
// suffix matching the chart's naming convention
// "<release>-jobs-manager" (or plain "jobs-manager" for unprefixed
// installs).
func DiscoverParentRelease(ctx context.Context, cs kubernetes.Interface, namespace string) (*ParentRelease, error) {
	deps, err := cs.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=client,app.kubernetes.io/managed-by=Helm",
	})
	if err != nil {
		return nil, fmt.Errorf("listing chart-managed deployments in namespace %s: %w", namespace, err)
	}

	// Filter to just the jobs-manager Deployment(s). The chart pins
	// the name to either "<release>-jobs-manager" or "jobs-manager"
	// depending on chart version + values; either way it ends in
	// "-jobs-manager" or equals "jobs-manager".
	var jmDeps []appsv1.Deployment
	for _, d := range deps.Items {
		if d.Name == "jobs-manager" || strings.HasSuffix(d.Name, "-jobs-manager") {
			jmDeps = append(jmDeps, d)
		}
	}

	switch len(jmDeps) {
	case 0:
		// Customer-actionable, no Helm: the CLI's own contract is that
		// customers never touch Helm, so the remediation is the flag,
		// the installer, or the doctor — not a helm invocation.
		return nil, fmt.Errorf(
			"%w in namespace %q. "+
				"If your client runs in another namespace, pass --namespace; "+
				"if this cluster has no tracebloc client yet, run the installer: "+
				"bash <(curl -fsSL https://tracebloc.io/i.sh). "+
				"Diagnose with `tracebloc cluster doctor`.",
			ErrNoParentRelease, namespace,
		)
	case 1:
		// happy path
	default:
		names := make([]string, 0, len(jmDeps))
		for _, d := range jmDeps {
			names = append(names, d.Name)
		}
		return nil, fmt.Errorf(
			"found %d tracebloc clients in namespace %q (%s); "+
				"this CLI doesn't yet support disambiguating between multiple. "+
				"Pass --namespace to target a namespace with exactly one client.",
			len(jmDeps), namespace, strings.Join(names, ", "),
		)
	}

	d := jmDeps[0]
	release := &ParentRelease{
		ReleaseName:  d.Labels["app.kubernetes.io/instance"],
		ChartVersion: chartVersionFromLabel(d.Labels["helm.sh/chart"]),
		AppVersion:   d.Labels["app.kubernetes.io/version"],
	}

	// The Service shares the Deployment's release labels by
	// convention. Construct the FQDN — we don't need to actually
	// hit the API to find it; the chart's helper templates pin the
	// service name to "<release>-jobs-manager" (or just
	// "jobs-manager" for older chart versions where the release
	// prefix wasn't included).
	//
	// We probe both names to be liberal: if the chart pinned just
	// "jobs-manager", that wins; otherwise fall back to the
	// release-prefixed form. Customers can always override via the
	// ingestor subchart's `jobsManager.endpoint` value.
	svc := pickJobsManagerService(ctx, cs, namespace, release.ReleaseName)
	const jobsManagerPort = 8080 // chart's well-known port for /internal/submit-ingestion-run
	release.JobsManagerService = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svc, namespace, jobsManagerPort)
	release.JobsManagerServiceName = svc
	release.JobsManagerPort = jobsManagerPort

	// The ingestor ServiceAccount name. Default to the chart default
	// ("ingestor"), then let the ingestionAuthz ConfigMap override it — that
	// ConfigMap IS the policy jobs-manager enforces, so it is the authoritative
	// source for which SA may call submit-ingestion-run (#7). Best-effort:
	// a missing ConfigMap (older chart), an RBAC denial, or an ambiguous
	// policy all leave the default in place.
	release.IngestorSAName = "ingestor"
	if sa := discoverIngestorSAName(ctx, cs, namespace, release.ReleaseName); sa != "" {
		release.IngestorSAName = sa
	}
	if len(d.Spec.Template.Spec.Containers) > 0 {
		for _, env := range d.Spec.Template.Spec.Containers[0].Env {
			if env.Name == "INGESTOR_IMAGE_DIGEST" {
				release.IngestorImageDigest = env.Value
			}
		}
	}

	return release, nil
}

// discoverIngestorSAName reads the chart's ingestion-authz ConfigMap
// (`<release>-ingestion-authz`, key `ingestion-authz.yaml`) and returns the
// ServiceAccount the CLI should mint a token for — the one jobs-manager will
// accept on POST /internal/submit-ingestion-run. It replaces the old hardcoded
// "ingestor" default + the manual --ingestor-sa flag (#7).
//
// The policy lists `allowed[]` entries of (service_account, namespace,
// table_prefixes). We keep only entries whose namespace matches the namespace
// we'll mint in (the rendered ConfigMap always populates namespace, defaulting
// to the release namespace), then require exactly one distinct non-empty
// service_account. Anything else — ConfigMap absent (older chart), RBAC denial,
// empty policy, or two different SAs — returns "" so the caller keeps the
// "ingestor" default rather than guessing an SA the server would reject.
// Best-effort by contract: never returns an error.
func discoverIngestorSAName(ctx context.Context, cs kubernetes.Interface, namespace, releaseName string) string {
	if releaseName == "" {
		return ""
	}
	cm, err := cs.CoreV1().ConfigMaps(namespace).Get(
		ctx, releaseName+"-ingestion-authz", metav1.GetOptions{})
	if err != nil {
		// NotFound (older chart / no ingestor subchart) or an RBAC/API error —
		// both non-fatal; fall back to the default.
		return ""
	}
	raw, ok := cm.Data["ingestion-authz.yaml"]
	if !ok {
		return ""
	}
	var policy struct {
		Allowed []struct {
			ServiceAccount string `yaml:"service_account"`
			Namespace      string `yaml:"namespace"`
		} `yaml:"allowed"`
	}
	if err := yaml.Unmarshal([]byte(raw), &policy); err != nil {
		return ""
	}
	sa := ""
	for _, e := range policy.Allowed {
		// Only entries that apply to the namespace we mint in — a token minted
		// here is worthless against a policy entry scoped to a different ns.
		if e.Namespace != namespace || e.ServiceAccount == "" {
			continue
		}
		switch {
		case sa == "":
			sa = e.ServiceAccount
		case sa != e.ServiceAccount:
			return "" // ambiguous — more than one SA for this namespace; keep the default
		}
	}
	return sa
}

// FindClientNamespaces scans every namespace the kubeconfig user may list for
// jobs-manager Deployments (the same selector + name filter DiscoverParentRelease
// uses) and returns the sorted, de-duplicated namespaces hosting one. It backs
// the fallback that makes `data list`/`cluster info` work out of the box when
// the client lives in its slug namespace rather than the kubeconfig's default
// (§7.3): a miss in the default namespace triggers this scan instead of a dead
// end. An RBAC-restricted user (cluster-wide list forbidden) gets the error
// back; callers treat that as "scan unavailable" and keep the original message.
func FindClientNamespaces(ctx context.Context, cs kubernetes.Interface) ([]string, error) {
	deps, err := cs.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=client,app.kubernetes.io/managed-by=Helm",
	})
	if err != nil {
		return nil, fmt.Errorf("scanning the cluster for tracebloc clients: %w", err)
	}
	seen := make(map[string]bool)
	var namespaces []string
	for _, d := range deps.Items {
		if d.Name != "jobs-manager" && !strings.HasSuffix(d.Name, "-jobs-manager") {
			continue
		}
		if !seen[d.Namespace] {
			seen[d.Namespace] = true
			namespaces = append(namespaces, d.Namespace)
		}
	}
	sort.Strings(namespaces)
	return namespaces, nil
}

// InClusterClient identifies a tracebloc client already installed on the cluster:
// its CLIENT_ID (the UUID auth username the pod authenticates with) and the
// namespace its release occupies.
type InClusterClient struct {
	ClientID  string
	Namespace string
}

// clientChartSelector matches the chart-managed resources of a tracebloc client
// release (the same selector DiscoverParentRelease uses on Deployments).
const clientChartSelector = "app.kubernetes.io/name=client,app.kubernetes.io/managed-by=Helm"

// DiscoverInClusterClientID finds a tracebloc client already installed on the
// cluster, if any, and returns its live CLIENT_ID + namespace (RFC-0001 §7.2
// step 1). It locates the namespace hosting the client release (its jobs-manager
// Deployment), then reads CLIENT_ID from the chart's `<release>-secrets` Secret
// there — scoping to that namespace avoids the node-agents mirror secret, which
// carries the same CLIENT_ID under the same labels.
//
// This anchors R7 adopt-backfill: a live client whose backend cluster_id is null
// must be adopted (and its anchor backfilled), never re-minted.
//
// Return contract (deliberately three-valued, so callers can tell "empty" from
// "couldn't tell" — collapsing the two is what let `client create` mint a
// duplicate over a live client and orphan it, the phantom-1060 class):
//   - (client, nil) — a live client was found;
//   - (nil, nil)    — the cluster is READABLE and genuinely has no client release;
//   - (nil, err)    — a read/RBAC error meant we could NOT determine either way.
//
// Callers must fail closed on the error case, never treat it as "nothing installed".
func DiscoverInClusterClientID(ctx context.Context, cs kubernetes.Interface) (*InClusterClient, error) {
	deps, err := cs.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: clientChartSelector,
	})
	if err != nil {
		// A reachable-but-unreadable cluster must NOT be reported as "nothing
		// installed" — that ambiguity is exactly what let a duplicate be minted
		// over a live client. Surface it so the caller fails closed.
		return nil, fmt.Errorf("listing client deployments to check for an existing client: %w", err)
	}
	ns := ""
	for _, d := range deps.Items {
		if d.Name == "jobs-manager" || strings.HasSuffix(d.Name, "-jobs-manager") {
			ns = d.Namespace
			break
		}
	}
	if ns == "" {
		return nil, nil // readable, no client release installed — a genuine fresh cluster
	}
	secrets, err := cs.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{
		LabelSelector: clientChartSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("reading the existing client's identity in namespace %q: %w", ns, err)
	}
	for _, s := range secrets.Items {
		if v, ok := s.Data["CLIENT_ID"]; ok && len(v) > 0 {
			return &InClusterClient{ClientID: string(v), Namespace: ns}, nil
		}
	}
	// A client release IS installed here (its jobs-manager Deployment exists) but its
	// CLIENT_ID secret wasn't readable — we know a client is present, so this is not a
	// fresh cluster. Fail closed rather than let the caller mint over it.
	return nil, fmt.Errorf("a tracebloc client is installed in namespace %q but its CLIENT_ID could not be read", ns)
}

// pickJobsManagerService probes for the chart's jobs-manager
// Service. The chart's helper templates have used both names over
// chart history:
//
//   - "jobs-manager"          (chart 1.3.x and earlier)
//   - "<release>-jobs-manager"  (some prefixing variants)
//
// We try the unprefixed name first because that's the dominant
// shipped behavior; fall back if it doesn't exist. Probe failures
// (timeouts, RBAC denials) fall through to the prefixed name too —
// the customer gets a clear DNS error later if neither resolves,
// which is more actionable than "couldn't enumerate services."
func pickJobsManagerService(ctx context.Context, cs kubernetes.Interface, namespace, release string) string {
	candidates := []string{
		"jobs-manager",
		release + "-jobs-manager",
	}
	for _, name := range candidates {
		_, err := cs.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return name
		}
	}
	// Last-ditch: return the unprefixed candidate; the caller will
	// hit a DNS error at POST time which surfaces with a clearer
	// message than us guessing.
	return "jobs-manager"
}

// chartVersionFromLabel extracts "1.3.5" from helm.sh/chart="client-1.3.5".
// Labels in this format are standard Helm output; we strip the
// chart-name prefix to expose just the version. Returns the raw
// label if it doesn't match the expected pattern (defensive
// fallback for unusual chart-name formats).
func chartVersionFromLabel(label string) string {
	// Expected: "<chart-name>-<semver>"; strip the "client-" prefix.
	const prefix = "client-"
	if strings.HasPrefix(label, prefix) {
		return label[len(prefix):]
	}
	return label
}
