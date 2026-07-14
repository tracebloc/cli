package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodRender is a minimal render carrying every shape the 8 invariants assert.
// It tests the CHECKER's logic — the real chart is exercised in CI at the
// pinned ref (scripts/.client-ref) by .github/workflows/chart-drift.yml.
const goodRender = `---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ingestor
  namespace: tracebloc
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: tracebloc-ingestion-authz
  namespace: tracebloc
data:
  ingestion-authz.yaml: |
    allowed:
      - service_account: "ingestor"
        namespace: "tracebloc"
        table_prefixes:
          - "*"
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: client-pvc
  namespace: tracebloc
---
apiVersion: v1
kind: Service
metadata:
  # (POST /internal/submit-ingestion-run) served here
  name: jobs-manager
  namespace: tracebloc
spec:
  ports:
    - name: http
      port: 8080
      targetPort: 8080
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tracebloc-jobs-manager
  namespace: tracebloc
  labels:
    app.kubernetes.io/name: client
    app.kubernetes.io/instance: tracebloc
    app.kubernetes.io/version: "1.0"
    app.kubernetes.io/managed-by: Helm
    helm.sh/chart: client-1.9.4
spec:
  template:
    spec:
      containers:
        - name: api
          ports:
            - name: http
              containerPort: 8080
          volumeMounts:
            - name: shared-volume
              mountPath: "/data/shared"
          env:
            - name: INGESTOR_IMAGE_DIGEST
              value: ""
      volumes:
        - name: shared-volume
          persistentVolumeClaim:
            claimName: client-pvc
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tracebloc-requests-proxy
  namespace: tracebloc
  labels:
    app.kubernetes.io/name: client
    app.kubernetes.io/instance: tracebloc
    app.kubernetes.io/managed-by: Helm
`

// runOn feeds a render through run() on stdin and returns (exitCode, output).
func runOn(t *testing.T, render string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := run(nil, strings.NewReader(render), &out)
	return code, out.String()
}

func TestAllInvariantsHold(t *testing.T) {
	code, out := runOn(t, goodRender)
	if code != 0 {
		t.Fatalf("want exit 0 on the good render, got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "all 8 invariants hold") {
		t.Fatalf("missing success line:\n%s", out)
	}
}

func TestFileArgument(t *testing.T) {
	p := filepath.Join(t.TempDir(), "render.yaml")
	if err := os.WriteFile(p, []byte(goodRender), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{p}, strings.NewReader(""), &out); code != 0 {
		t.Fatalf("want exit 0 reading from a file, got %d:\n%s", code, out.String())
	}
}

func TestBrokenInvariants(t *testing.T) {
	tests := []struct {
		name string
		// mutate the good render via string replacement
		old, new string
		wantMark string // "✖ <n>." line that must appear
	}{
		{
			name:     "discovery label selector broken",
			old:      "app.kubernetes.io/name: client\n    app.kubernetes.io/instance: tracebloc\n    app.kubernetes.io/version",
			new:      "app.kubernetes.io/name: renamed\n    app.kubernetes.io/instance: tracebloc\n    app.kubernetes.io/version",
			wantMark: "✖ 1.",
		},
		{
			name:     "chart label lost its client- prefix",
			old:      "helm.sh/chart: client-1.9.4",
			new:      "helm.sh/chart: renamed-1.9.4",
			wantMark: "✖ 1.",
		},
		{
			name:     "deployment renamed to an untolerated form",
			old:      "name: tracebloc-jobs-manager",
			new:      "name: tracebloc-manager-of-jobs",
			wantMark: "✖ 1.", // no longer matches the discovery name filter at all
		},
		{
			name:     "service renamed",
			old:      "  name: jobs-manager\n  namespace: tracebloc\nspec:\n  ports:",
			new:      "  name: manager-svc\n  namespace: tracebloc\nspec:\n  ports:",
			wantMark: "✖ 3.",
		},
		{
			name:     "service port moved off 8080",
			old:      "      port: 8080",
			new:      "      port: 9090",
			wantMark: "✖ 3.",
		},
		{
			name:     "pvc renamed",
			old:      "name: client-pvc\n  namespace: tracebloc",
			new:      "name: client-data\n  namespace: tracebloc",
			wantMark: "✖ 4.",
		},
		{
			name:     "mount path moved",
			old:      `mountPath: "/data/shared"`,
			new:      `mountPath: "/data/datasets"`,
			wantMark: "✖ 4.",
		},
		{
			name:     "mount backed by a different claim",
			old:      "claimName: client-pvc",
			new:      "claimName: other-pvc",
			wantMark: "✖ 4.",
		},
		{
			name:     "authz configmap renamed",
			old:      "name: tracebloc-ingestion-authz",
			new:      "name: tracebloc-ingest-policy",
			wantMark: "✖ 5.",
		},
		{
			name:     "authz policy key renamed",
			old:      "ingestion-authz.yaml: |",
			new:      "policy.yaml: |",
			wantMark: "✖ 5.",
		},
		{
			name:     "default SA no longer the CLI fallback",
			old:      `service_account: "ingestor"`,
			new:      `service_account: "pusher"`,
			wantMark: "✖ 5.",
		},
		{
			name:     "SA object not rendered",
			old:      "kind: ServiceAccount\nmetadata:\n  name: ingestor",
			new:      "kind: ServiceAccount\nmetadata:\n  name: other-sa",
			wantMark: "✖ 5.",
		},
		{
			name:     "digest env dropped",
			old:      "- name: INGESTOR_IMAGE_DIGEST",
			new:      "- name: INGESTOR_IMAGE_SHA",
			wantMark: "✖ 6.",
		},
		{
			name:     "requests-proxy renamed",
			old:      "name: tracebloc-requests-proxy",
			new:      "name: tracebloc-egress-broker",
			wantMark: "✖ 7.",
		},
		{
			name:     "container port moved off 8080",
			old:      "containerPort: 8080",
			new:      "containerPort: 9090",
			wantMark: "✖ 8.",
		},
		{
			name:     "submit path marker gone from the render",
			old:      "(POST /internal/submit-ingestion-run) served here",
			new:      "(POST /internal/run-ingestion) served here",
			wantMark: "✖ 8.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := strings.Replace(goodRender, tt.old, tt.new, 1)
			if mutated == goodRender {
				t.Fatalf("mutation %q did not apply — fixture drifted", tt.old)
			}
			code, out := runOn(t, mutated)
			if code != 1 {
				t.Fatalf("want exit 1, got %d:\n%s", code, out)
			}
			if !strings.Contains(out, tt.wantMark) {
				t.Fatalf("want a %q failure, got:\n%s", tt.wantMark, out)
			}
		})
	}
}

func TestBareRequestsProxyTiedByInstanceLabel(t *testing.T) {
	// Older unprefixed charts: doctor accepts a bare "requests-proxy" ONLY when
	// its instance label ties it to the release.
	render := strings.Replace(goodRender, "name: tracebloc-requests-proxy", "name: requests-proxy", 1)
	if code, out := runOn(t, render); code != 0 {
		t.Fatalf("bare requests-proxy with instance label must pass, got %d:\n%s", code, out)
	}
	render = strings.Replace(render,
		"name: requests-proxy\n  namespace: tracebloc\n  labels:\n    app.kubernetes.io/name: client\n    app.kubernetes.io/instance: tracebloc",
		"name: requests-proxy\n  namespace: tracebloc\n  labels:\n    app.kubernetes.io/name: client\n    app.kubernetes.io/instance: someone-else", 1)
	code, out := runOn(t, render)
	if code != 1 || !strings.Contains(out, "✖ 7.") {
		t.Fatalf("bare requests-proxy owned by ANOTHER release must fail invariant 7, got %d:\n%s", code, out)
	}
}

func TestUnparseableRenderIsAnError(t *testing.T) {
	if code, _ := runOn(t, "kind: [unclosed"); code != 2 {
		t.Fatalf("want exit 2 on unparseable YAML, got %d", code)
	}
	if code, _ := runOn(t, ""); code != 2 {
		t.Fatalf("want exit 2 on an empty render, got %d", code)
	}
}
