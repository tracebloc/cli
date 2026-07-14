// Command chart-invariants asserts the 8 tracebloc/client chart invariants the
// CLI hardcodes for discovery, doctor, and dataset ingest (cli#290). It reads a
// `helm template` render (multi-doc YAML) from a file argument or stdin and
// exits non-zero when any invariant no longer holds — so a chart rename that
// would ship green in both repos and break in the field fails CI instead.
//
// The CLI-side sources of truth for each invariant:
//
//  1. Discovery labels — internal/cluster/discover.go DiscoverParentRelease
//     lists Deployments by `app.kubernetes.io/name=client,
//     app.kubernetes.io/managed-by=Helm` and reads the release name, chart
//     version ("client-<semver>"), and app version off the labels.
//  2. jobs-manager Deployment name — the same discovery tolerates exactly two
//     forms: "<release>-jobs-manager" or bare "jobs-manager".
//  3. jobs-manager Service — pickJobsManagerService probes "jobs-manager" then
//     "<release>-jobs-manager"; the CLI port-forwards to port 8080
//     (jobsManagerPort).
//  4. Shared PVC — internal/cluster/pvc.go pins the claim name "client-pvc"
//     (SharedPVCClaimName) mounted at "/data/shared" (SharedPVCMountPath).
//  5. Ingestion authz — discoverIngestorSAName reads ConfigMap
//     "<release>-ingestion-authz", key "ingestion-authz.yaml", shaped as
//     allowed[]{service_account, namespace}, and needs exactly one distinct SA
//     for the release namespace; the CLI's fallback default is the SA name
//     "ingestor" (release.IngestorSAName), which the chart must keep rendering.
//  6. Ingestor image pin — DiscoverParentRelease reads INGESTOR_IMAGE_DIGEST
//     from the FIRST container of the jobs-manager Deployment.
//  7. requests-proxy Deployment — internal/doctor findDeployment accepts
//     "<release>-requests-proxy", or bare "requests-proxy" tied to the release
//     by its app.kubernetes.io/instance label.
//  8. Submit path — internal/submit/client.go POSTs SubmitPath
//     ("/internal/submit-ingestion-run") to port 8080; the jobs-manager
//     container must expose that port and the chart must still document the
//     path (the rendered manifests carry it in comments — a client-runtime
//     endpoint move updates those in the same breath).
//
// Usage:
//
//	helm template tracebloc <client>/client -n tracebloc \
//	  -f <client>/client/ci/bm-values.yaml | go run ./scripts/chart-invariants
//	go run ./scripts/chart-invariants -release tracebloc -namespace tracebloc rendered.yaml
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout))
}

func run(args []string, stdin io.Reader, out io.Writer) int {
	fs := flag.NewFlagSet("chart-invariants", flag.ContinueOnError)
	fs.SetOutput(out)
	release := fs.String("release", "tracebloc", "helm release name the chart was rendered with")
	namespace := fs.String("namespace", "tracebloc", "namespace the chart was rendered into (-n)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var raw []byte
	var err error
	if fs.NArg() > 0 {
		raw, err = os.ReadFile(fs.Arg(0))
	} else {
		raw, err = io.ReadAll(stdin)
	}
	if err != nil {
		printf(out, "error: reading the rendered chart: %v\n", err)
		return 2
	}

	docs, err := parseDocs(raw)
	if err != nil {
		printf(out, "error: the render is not parseable YAML (helm template failed?): %v\n", err)
		return 2
	}
	if len(docs) == 0 {
		printf(out, "error: the render contains no objects — pass the `helm template` output as a file or on stdin\n")
		return 2
	}

	c := &checker{release: *release, namespace: *namespace, raw: string(raw), docs: docs, out: out}
	printf(out, "── CLI-assumed chart invariants (tracebloc/cli#290) ─────────\n")
	c.checkDiscoveryLabels()
	c.checkDeploymentName()
	c.checkService()
	c.checkPVC()
	c.checkIngestionAuthz()
	c.checkIngestorDigestEnv()
	c.checkRequestsProxy()
	c.checkSubmitPath()
	printf(out, "──────────────────────────────────────────────────────────────\n")
	if c.failed > 0 {
		printf(out, "DRIFT: %d invariant(s) broken. Fix the chart, or bump the CLI-side constants (internal/cluster, internal/doctor, internal/submit) together with scripts/.client-ref in one deliberate PR.\n", c.failed)
		return 1
	}
	printf(out, "all 8 invariants hold.\n")
	return 0
}

// printf writes formatted output, deliberately discarding the writer error:
// this is CI log output, and a late write failure must not mask the exit code.
func printf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

type doc = map[string]any

type checker struct {
	release   string
	namespace string
	raw       string // the raw render, for comment-carried markers (invariant 8)
	docs      []doc
	out       io.Writer
	failed    int
}

func (c *checker) ok(n int, format string, a ...any) {
	printf(c.out, "  ✔ %d. %s\n", n, fmt.Sprintf(format, a...))
}

func (c *checker) fail(n int, format string, a ...any) {
	c.failed++
	printf(c.out, "  ✖ %d. %s\n", n, fmt.Sprintf(format, a...))
}

// parseDocs decodes a multi-document YAML stream into generic maps, skipping
// empty documents (helm renders plenty of them between templates).
func parseDocs(raw []byte) ([]doc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []doc
	for {
		var d doc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
}

// dig walks nested map[string]any keys; nil when any hop is missing.
func dig(v any, path ...string) any {
	cur := v
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func digStr(v any, path ...string) string {
	s, _ := dig(v, path...).(string)
	return s
}

func digList(v any, path ...string) []any {
	l, _ := dig(v, path...).([]any)
	return l
}

func kind(d doc) string { return digStr(d, "kind") }
func name(d doc) string { return digStr(d, "metadata", "name") }

// find returns every object of the given kind whose name matches any of names.
func (c *checker) find(k string, names ...string) []doc {
	var out []doc
	for _, d := range c.docs {
		if kind(d) != k {
			continue
		}
		for _, n := range names {
			if name(d) == n {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// jobsManagerDeployments mirrors DiscoverParentRelease exactly: Deployments
// carrying the discovery labels, name-filtered to the two tolerated forms.
func (c *checker) jobsManagerDeployments() []doc {
	var out []doc
	for _, d := range c.docs {
		if kind(d) != "Deployment" {
			continue
		}
		if digStr(d, "metadata", "labels", "app.kubernetes.io/name") != "client" ||
			digStr(d, "metadata", "labels", "app.kubernetes.io/managed-by") != "Helm" {
			continue
		}
		if n := name(d); n == "jobs-manager" || strings.HasSuffix(n, "-jobs-manager") {
			out = append(out, d)
		}
	}
	return out
}

// 1. Discovery labels: exactly one jobs-manager Deployment is reachable via
// the selector DiscoverParentRelease lists with, and the labels it reads the
// release identity from are all present and well-formed.
func (c *checker) checkDiscoveryLabels() {
	const n = 1
	jms := c.jobsManagerDeployments()
	if len(jms) != 1 {
		c.fail(n, "want exactly one jobs-manager Deployment under app.kubernetes.io/name=client + app.kubernetes.io/managed-by=Helm, got %d — DiscoverParentRelease can no longer find the release", len(jms))
		return
	}
	labels, _ := dig(jms[0], "metadata", "labels").(map[string]any)
	var broken []string
	if s, _ := labels["app.kubernetes.io/instance"].(string); s != c.release {
		broken = append(broken, fmt.Sprintf("app.kubernetes.io/instance=%q (want the release name %q)", s, c.release))
	}
	if s, _ := labels["helm.sh/chart"].(string); !strings.HasPrefix(s, "client-") {
		broken = append(broken, fmt.Sprintf("helm.sh/chart=%q (chartVersionFromLabel expects the \"client-\" prefix)", s))
	}
	if s, _ := labels["app.kubernetes.io/version"].(string); s == "" {
		broken = append(broken, "app.kubernetes.io/version is empty (AppVersion read)")
	}
	if len(broken) > 0 {
		c.fail(n, "jobs-manager Deployment labels drifted: %s", strings.Join(broken, "; "))
		return
	}
	c.ok(n, "jobs-manager Deployment discoverable via the chart labels (name=client, managed-by=Helm, instance, helm.sh/chart, version)")
}

// 2. Deployment name: one of the two forms DiscoverParentRelease tolerates.
func (c *checker) checkDeploymentName() {
	const n = 2
	jms := c.jobsManagerDeployments()
	if len(jms) != 1 {
		c.fail(n, "cannot check the jobs-manager Deployment name — discovery found %d candidates", len(jms))
		return
	}
	got := name(jms[0])
	if got == "jobs-manager" || got == c.release+"-jobs-manager" {
		c.ok(n, "jobs-manager Deployment named %q (a tolerated form: \"<release>-jobs-manager\" or \"jobs-manager\")", got)
		return
	}
	c.fail(n, "jobs-manager Deployment named %q — the CLI only tolerates %q or \"jobs-manager\"", got, c.release+"-jobs-manager")
}

// 3. Service: one of the names pickJobsManagerService probes, with port 8080.
func (c *checker) checkService() {
	const n = 3
	svcs := c.find("Service", "jobs-manager", c.release+"-jobs-manager")
	if len(svcs) == 0 {
		c.fail(n, "no Service named \"jobs-manager\" or %q — pickJobsManagerService finds nothing to port-forward to", c.release+"-jobs-manager")
		return
	}
	for _, s := range svcs {
		for _, p := range digList(s, "spec", "ports") {
			if port, _ := dig(p, "port").(int); port == 8080 {
				c.ok(n, "Service %q exposes port 8080 (jobsManagerPort — the CLI's port-forward + POST target)", name(s))
				return
			}
		}
	}
	c.fail(n, "Service %q exists but has no port 8080 — the CLI hardcodes jobsManagerPort=8080", name(svcs[0]))
}

// 4. PVC "client-pvc" rendered and mounted at /data/shared by jobs-manager.
func (c *checker) checkPVC() {
	const n = 4
	if len(c.find("PersistentVolumeClaim", "client-pvc")) == 0 {
		c.fail(n, "no PersistentVolumeClaim named \"client-pvc\" — SharedPVCClaimName no longer resolves")
		return
	}
	jms := c.jobsManagerDeployments()
	if len(jms) != 1 {
		c.fail(n, "PVC \"client-pvc\" rendered, but cannot verify the /data/shared mount — jobs-manager Deployment not uniquely discoverable")
		return
	}
	jm := jms[0]
	// Find the volumeMount at /data/shared, then the pod volume backing it.
	volName := ""
	for _, ct := range digList(jm, "spec", "template", "spec", "containers") {
		for _, vm := range digList(ct, "volumeMounts") {
			if digStr(vm, "mountPath") == "/data/shared" {
				volName = digStr(vm, "name")
			}
		}
	}
	if volName == "" {
		c.fail(n, "jobs-manager has no volumeMount at \"/data/shared\" (SharedPVCMountPath) — the stage-Pod mount convention breaks")
		return
	}
	for _, v := range digList(jm, "spec", "template", "spec", "volumes") {
		if digStr(v, "name") == volName {
			if claim := digStr(v, "persistentVolumeClaim", "claimName"); claim != "client-pvc" {
				c.fail(n, "the /data/shared volume %q is backed by claim %q, not \"client-pvc\"", volName, claim)
				return
			}
			c.ok(n, "PVC \"client-pvc\" rendered and mounted at \"/data/shared\" in jobs-manager (SharedPVCClaimName/SharedPVCMountPath)")
			return
		}
	}
	c.fail(n, "the /data/shared volumeMount references volume %q, which the pod spec does not define", volName)
}

// 5. ingestion-authz ConfigMap shape + the "ingestor" SA fallback.
func (c *checker) checkIngestionAuthz() {
	const n = 5
	cms := c.find("ConfigMap", c.release+"-ingestion-authz")
	if len(cms) == 0 {
		c.fail(n, "no ConfigMap named %q — discoverIngestorSAName silently degrades to the fallback", c.release+"-ingestion-authz")
		return
	}
	rawPolicy := digStr(cms[0], "data", "ingestion-authz.yaml")
	if rawPolicy == "" {
		c.fail(n, "ConfigMap %q has no \"ingestion-authz.yaml\" key — the key discoverIngestorSAName reads", c.release+"-ingestion-authz")
		return
	}
	var policy struct {
		Allowed []struct {
			ServiceAccount string `yaml:"service_account"`
			Namespace      string `yaml:"namespace"`
		} `yaml:"allowed"`
	}
	if err := yaml.Unmarshal([]byte(rawPolicy), &policy); err != nil {
		c.fail(n, "the ingestion-authz policy no longer parses as allowed[]{service_account, namespace}: %v", err)
		return
	}
	// Mirror discoverIngestorSAName: entries scoped to the release namespace,
	// exactly one distinct non-empty SA — anything else degrades to the fallback.
	sa := ""
	for _, e := range policy.Allowed {
		if e.Namespace != c.namespace || e.ServiceAccount == "" {
			continue
		}
		if sa != "" && sa != e.ServiceAccount {
			c.fail(n, "the default policy is ambiguous (SAs %q and %q for namespace %q) — the CLI would fall back to guessing", sa, e.ServiceAccount, c.namespace)
			return
		}
		sa = e.ServiceAccount
	}
	if sa == "" {
		c.fail(n, "the default policy has no entry for namespace %q with a service_account — discoverIngestorSAName finds nothing", c.namespace)
		return
	}
	// The fallback contract: the CLI hardcodes "ingestor" when the ConfigMap is
	// absent/unreadable (older charts). The chart's default SA must stay in
	// lock-step, and the SA object itself must render.
	if sa != "ingestor" {
		c.fail(n, "the chart's default ingestor SA is %q, but the CLI's hardcoded fallback is \"ingestor\" (discover.go IngestorSAName) — update both sides together", sa)
		return
	}
	if len(c.find("ServiceAccount", "ingestor")) == 0 {
		c.fail(n, "the policy references SA \"ingestor\" but no ServiceAccount \"ingestor\" is rendered — token minting would target a ghost SA")
		return
	}
	c.ok(n, "ConfigMap %q key \"ingestion-authz.yaml\" parses, and the default SA is the CLI's \"ingestor\" fallback (SA rendered)", c.release+"-ingestion-authz")
}

// 6. INGESTOR_IMAGE_DIGEST env on the FIRST jobs-manager container — the exact
// read DiscoverParentRelease does (Containers[0] only).
func (c *checker) checkIngestorDigestEnv() {
	const n = 6
	jms := c.jobsManagerDeployments()
	if len(jms) != 1 {
		c.fail(n, "cannot check INGESTOR_IMAGE_DIGEST — jobs-manager Deployment not uniquely discoverable")
		return
	}
	cts := digList(jms[0], "spec", "template", "spec", "containers")
	if len(cts) == 0 {
		c.fail(n, "jobs-manager renders no containers")
		return
	}
	for _, e := range digList(cts[0], "env") {
		if digStr(e, "name") == "INGESTOR_IMAGE_DIGEST" {
			c.ok(n, "INGESTOR_IMAGE_DIGEST env present on jobs-manager's first container (digest-pin discovery)")
			return
		}
	}
	c.fail(n, "no INGESTOR_IMAGE_DIGEST env on jobs-manager's FIRST container — the CLI reads Containers[0] only")
}

// 7. requests-proxy Deployment under a name doctor's findDeployment accepts.
func (c *checker) checkRequestsProxy() {
	const n = 7
	if len(c.find("Deployment", c.release+"-requests-proxy")) > 0 {
		c.ok(n, "requests-proxy Deployment named %q (doctor's Service Bus egress check)", c.release+"-requests-proxy")
		return
	}
	for _, d := range c.find("Deployment", "requests-proxy") {
		if digStr(d, "metadata", "labels", "app.kubernetes.io/instance") == c.release {
			c.ok(n, "requests-proxy Deployment named \"requests-proxy\", tied to the release by its instance label")
			return
		}
	}
	c.fail(n, "no Deployment named %q (or bare \"requests-proxy\" with instance=%q) — doctor's checkRequestsProxy reports the egress broker missing", c.release+"-requests-proxy", c.release)
}

// 8. Submit path: the jobs-manager container exposes 8080 (the port behind
// POST /internal/submit-ingestion-run), and the chart still references the
// path. The path lives in rendered comments, not a structured field — a weak
// but deliberate tripwire: a client-runtime endpoint move updates the chart's
// comments in the same change, and this makes that update confront the CLI's
// SubmitPath constant too.
func (c *checker) checkSubmitPath() {
	const n = 8
	jms := c.jobsManagerDeployments()
	if len(jms) != 1 {
		c.fail(n, "cannot check the submit port — jobs-manager Deployment not uniquely discoverable")
		return
	}
	port8080 := false
	for _, ct := range digList(jms[0], "spec", "template", "spec", "containers") {
		for _, p := range digList(ct, "ports") {
			if cp, _ := dig(p, "containerPort").(int); cp == 8080 {
				port8080 = true
			}
		}
	}
	if !port8080 {
		c.fail(n, "no jobs-manager container exposes containerPort 8080 — the POST %s target", "/internal/submit-ingestion-run")
		return
	}
	if !strings.Contains(c.raw, "/internal/submit-ingestion-run") {
		c.fail(n, "the render no longer references \"/internal/submit-ingestion-run\" — if the endpoint moved, update internal/submit SubmitPath and this gate together")
		return
	}
	c.ok(n, "jobs-manager exposes containerPort 8080 and the chart still references POST /internal/submit-ingestion-run (SubmitPath)")
}
