package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/ui"
)

func resNode(name, cpu, mem string, extra ...string) *corev1.Node {
	alloc := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
	if len(extra) == 2 {
		alloc[corev1.ResourceName(extra[0])] = resource.MustParse(extra[1])
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: alloc,
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func resJMDeploy(release string, env map[string]string) *appsv1.Deployment {
	var vars []corev1.EnvVar
	for k, v := range env {
		vars = append(vars, corev1.EnvVar{Name: k, Value: v})
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: release + "-jobs-manager", Namespace: "tracebloc"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "jobs-manager", Env: vars}}},
			},
		},
	}
}

func resTarget(cs *fake.Clientset) *clusterTarget {
	return &clusterTarget{
		Resolved:  &cluster.ResolvedConfig{Namespace: "tracebloc"},
		Clientset: cs,
		Release:   &cluster.ParentRelease{ReleaseName: "tb"},
	}
}

// TestRenderResources_ShowsMachineAndTrainingCeiling: the happy path renders the
// machine capacity (summed allocatable) and the per-run training ceiling read
// from the jobs-manager env.
func TestRenderResources_ShowsMachineAndTrainingCeiling(t *testing.T) {
	cs := fake.NewClientset(
		resNode("n1", "8", "32Gi"),
		resJMDeploy("tb", map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi", "RESOURCE_REQUESTS": "cpu=4,memory=16Gi"}),
	)
	var buf bytes.Buffer
	if err := renderResources(context.Background(), ui.New(&buf, ui.WithColor(false)), resTarget(cs)); err != nil {
		t.Fatalf("renderResources: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Your secure environment has:", "8 CPU · 32 GiB", "Each training run may use up to:", "4 CPU · 16 GiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Remote target (no ServerURL set) → no physical "Your machine" line.
	if strings.Contains(out, "Your machine has:") {
		t.Errorf("remote target must not show the machine line:\n%s", out)
	}
	// No Kubernetes vocabulary must leak into the default view.
	for _, banned := range []string{"allocatable", "RESOURCE_LIMITS", "limits", "requests"} {
		if strings.Contains(out, banned) {
			t.Errorf("leaked k8s term %q in default view:\n%s", banned, out)
		}
	}
}

// TestRenderResources_ChartDefaultWhenEnvUnset: with no RESOURCE_* env, the
// ceiling reported is the chart default (cpu=2,memory=8Gi), not "unknown".
func TestRenderResources_ChartDefaultWhenEnvUnset(t *testing.T) {
	cs := fake.NewClientset(resNode("n1", "8", "32Gi"), resJMDeploy("tb", map[string]string{}))
	var buf bytes.Buffer
	if err := renderResources(context.Background(), ui.New(&buf, ui.WithColor(false)), resTarget(cs)); err != nil {
		t.Fatalf("renderResources: %v", err)
	}
	if !strings.Contains(buf.String(), "Each training run may use up to:") ||
		!strings.Contains(buf.String(), "2 CPU · 8 GiB") {
		t.Errorf("want chart-default ceiling 2 CPU · 8 GiB:\n%s", buf.String())
	}
}

// TestRenderResources_LocalShowsMachineLine: on a LOCAL install (loopback API
// server) the physical "Your machine" line appears above the environment. Its
// value is host-dependent (DetectHost), so we assert the label, not the numbers.
func TestRenderResources_LocalShowsMachineLine(t *testing.T) {
	cs := fake.NewClientset(
		resNode("n1", "8", "32Gi"),
		resJMDeploy("tb", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"}),
	)
	target := resTarget(cs)
	target.Resolved.ServerURL = "https://127.0.0.1:6550" // loopback ⇒ local install
	var buf bytes.Buffer
	if err := renderResources(context.Background(), ui.New(&buf, ui.WithColor(false)), target); err != nil {
		t.Fatalf("renderResources: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Your machine has:", "Your secure environment has:", "Each training run may use up to:"} {
		if !strings.Contains(out, want) {
			t.Errorf("local install missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderResources_GPUSurfaced: a node exposing a GPU shows it on the machine
// line, and a GPU training run shows it on the ceiling line.
func TestRenderResources_GPUSurfaced(t *testing.T) {
	cs := fake.NewClientset(
		resNode("n1", "8", "32Gi", "nvidia.com/gpu", "1"),
		resJMDeploy("tb", map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi", "GPU_LIMITS": "nvidia.com/gpu=1"}),
	)
	var buf bytes.Buffer
	if err := renderResources(context.Background(), ui.New(&buf, ui.WithColor(false)), resTarget(cs)); err != nil {
		t.Fatalf("renderResources: %v", err)
	}
	if c := strings.Count(buf.String(), "1 GPU"); c < 2 {
		t.Errorf("expected GPU on both machine + training lines, got %d occurrences:\n%s", c, buf.String())
	}
}

// TestRenderResources_PhantomGPUSuppressed: the chart stamps
// GPU_LIMITS=nvidia.com/gpu=1 as literal env even on CPU-only hosts. On a node
// that exposes NO GPU, the per-run ceiling must NOT advertise a GPU — otherwise
// the view contradicts its own "gpu: none detected" detail. Mirrors the set
// path's phantom-GPU normalization (Bugbot #241). Fails before the fix (the
// training line prints "1 GPU" on a GPU-less machine).
func TestRenderResources_PhantomGPUSuppressed(t *testing.T) {
	cs := fake.NewClientset(
		resNode("n1", "8", "32Gi"), // no GPU on the node
		resJMDeploy("tb", map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi", "GPU_LIMITS": "nvidia.com/gpu=1"}),
	)
	var buf bytes.Buffer
	if err := renderResources(context.Background(), ui.New(&buf, ui.WithColor(false), ui.WithVerbose(true)), resTarget(cs)); err != nil {
		t.Fatalf("renderResources: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "GPU") && !strings.Contains(out, "none detected") {
		t.Fatalf("unexpected GPU vocabulary on a GPU-less machine:\n%s", out)
	}
	// The per-run ceiling line must not carry a GPU clause.
	if strings.Contains(out, "· 1 GPU") {
		t.Errorf("phantom GPU must be suppressed on a GPU-less machine:\n%s", out)
	}
	// And the honest detail must still render (no contradiction).
	if !strings.Contains(out, "none detected") {
		t.Errorf("verbose view should report gpu: none detected:\n%s", out)
	}
}

// TestRenderResources_NodeListErrorIsNotFatal: even when node capacity can't be
// read, the training ceiling still renders and the machine line says so.
func TestRenderResources_VerboseShowsRawEnv(t *testing.T) {
	cs := fake.NewClientset(resNode("n1", "8", "32Gi"), resJMDeploy("tb", map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi"}))
	var buf bytes.Buffer
	if err := renderResources(context.Background(), ui.New(&buf, ui.WithColor(false), ui.WithVerbose(true)), resTarget(cs)); err != nil {
		t.Fatalf("renderResources: %v", err)
	}
	out := buf.String()
	// Verbose is the ONLY place the raw env + namespace/client are allowed.
	for _, want := range []string{"Details", "cpu=4,memory=16Gi", "tracebloc"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose view missing %q:\n%s", want, out)
		}
	}
}

// TestRunResourcesShow_BadKubeconfigExit3: a broken kubeconfig fails with exit 3
// before any cluster read (mirrors runDataList's contract).
func TestRunResourcesShow_BadKubeconfigExit3(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(bad, []byte("}{ not valid kubeconfig"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := runResourcesShow(context.Background(), ui.New(&buf, ui.WithColor(false)),
		cluster.KubeconfigOptions{Path: bad})
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 3 {
		t.Fatalf("err = %v, want *exitError code 3", err)
	}
}

// TestResourcesSet_DesignedInvocationsParse: the locked-design invocations must
// PARSE cleanly through the real root tree (the `--cores`/`--memory`/`--gpus`
// flags, the hidden `--cpu` alias, and the `max` positional are all wired on the
// `set` subcommand) — never a cobra "unknown flag" error. They fail with SHOW's
// exit-3 kubeconfig contract here (broken --kubeconfig) because they route into
// the cluster-resolving apply path, which proves parsing succeeded.
func TestResourcesSet_DesignedInvocationsParse(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(bad, []byte("}{ not valid kubeconfig"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"resources", "set", "--cores", "4", "--memory", "16Gi", "--yes", "--kubeconfig", bad},
		{"resources", "set", "--cpu", "4", "--yes", "--kubeconfig", bad},
		{"resources", "set", "max", "--yes", "--kubeconfig", bad},
	} {
		root := NewRootCmd(BuildInfo{Version: "test"})
		root.SetArgs(args)
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		err := root.Execute()

		if err != nil && strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("%v: hit cobra unknown-flag error: %v", args, err)
		}
		var ee *exitError
		if !errors.As(err, &ee) || ee.Code() != 3 {
			t.Fatalf("%v: err = %v, want SHOW's *exitError code 3 (proves it parsed + reached cluster resolve)", args, err)
		}
	}
}

// TestResourcesSet_MaxWithExplicitRejected: `max` + an explicit value flag is a
// contradiction and must fail with exit 2 BEFORE any cluster read (no kubeconfig
// needed to reach it — the pure request check runs first).
func TestResourcesSet_MaxWithExplicitRejected(t *testing.T) {
	root := NewRootCmd(BuildInfo{Version: "test"})
	root.SetArgs([]string{"resources", "set", "max", "--cores", "4"})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	err := root.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("err = %v, want *exitError code 2", err)
	}
}

// TestResourcesBare_RoutesToShow: bare `tracebloc resources` still routes to the
// SHOW path (not the `set` deferral, not an "unknown command"). Proven through
// the tree by feeding a broken --kubeconfig and asserting SHOW's exit-3
// kubeconfig-load contract — an exit 1 here would mean it wrongly hit the
// deferral, and a nil error would mean it never ran SHOW.
func TestResourcesBare_RoutesToShow(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(bad, []byte("}{ not valid kubeconfig"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := NewRootCmd(BuildInfo{Version: "test"})
	root.SetArgs([]string{"resources", "--kubeconfig", bad})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	err := root.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 3 {
		t.Fatalf("bare resources err = %v, want SHOW's *exitError code 3 (proves it routed to SHOW)", err)
	}
}

// TestResourcesCmd_WiredIntoTree: the command is reachable from the root tree
// and `--help` renders without error.
func TestResourcesCmd_WiredIntoTree(t *testing.T) {
	root := NewRootCmd(BuildInfo{Version: "test"})
	root.SetArgs([]string{"resources", "--help"})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("resources --help: %v", err)
	}
	if !strings.Contains(buf.String(), "how much of this machine tracebloc may use") {
		t.Errorf("help missing the one-concept summary:\n%s", buf.String())
	}
}
