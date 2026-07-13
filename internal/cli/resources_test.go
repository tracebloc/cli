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
	for _, want := range []string{"This machine", "8 CPU · 32 GiB", "tracebloc uses", "up to 4 CPU · 16 GiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
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
	if !strings.Contains(buf.String(), "up to 2 CPU · 8 GiB") {
		t.Errorf("want chart-default ceiling 2 CPU · 8 GiB:\n%s", buf.String())
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

// TestRunResourcesSetDeferred_HonestExit1: the deferral handler returns a
// silent (err==nil inner, so main() prints no extra "Error:" line) exit-1 with
// an honest "not supported yet" message that points back at the SHOW view —
// never a bogus success or a mangled cluster mutation.
func TestRunResourcesSetDeferred_HonestExit1(t *testing.T) {
	var buf bytes.Buffer
	err := runResourcesSetDeferred(ui.New(&buf, ui.WithColor(false)))
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 1 {
		t.Fatalf("err = %v, want *exitError code 1", err)
	}
	if !IsSilentError(err) {
		t.Errorf("deferred message already printed a ✖ block, so the error must be silent")
	}
	out := buf.String()
	if !strings.Contains(out, "isn't supported in this build yet") {
		t.Errorf("missing honest not-yet-supported message:\n%s", out)
	}
	if !strings.Contains(out, "tracebloc resources") {
		t.Errorf("deferral should point back at `tracebloc resources`:\n%s", out)
	}
}

// TestResourcesSet_DesignedInvocationsReachDeferral is the regression for
// finding #1: the two locked-design invocations must PARSE (the `--cpu`/
// `--memory` flags and the `max` positional are wired on the `set` subcommand)
// and reach the honest exit-1 deferral — NOT die on cobra's "unknown flag:
// --cpu". Driven through the real root tree so flag/arg parsing is exercised
// end-to-end, which the direct-call test above cannot cover.
func TestResourcesSet_DesignedInvocationsReachDeferral(t *testing.T) {
	for _, args := range [][]string{
		{"resources", "set", "--cpu", "4", "--memory", "16Gi"},
		{"resources", "set", "max"},
	} {
		root := NewRootCmd(BuildInfo{Version: "test"})
		root.SetArgs(args)
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		err := root.Execute()

		// The whole point: it must not be a cobra parse failure.
		if err != nil && strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("%v: hit cobra unknown-flag error, not the honest deferral: %v", args, err)
		}
		var ee *exitError
		if !errors.As(err, &ee) || ee.Code() != 1 {
			t.Fatalf("%v: err = %v, want honest *exitError code 1", args, err)
		}
		out := buf.String()
		if !strings.Contains(out, "isn't supported in this build yet") {
			t.Errorf("%v: missing not-yet-supported message:\n%s", args, out)
		}
		if !strings.Contains(out, "tracebloc resources") {
			t.Errorf("%v: deferral should point at `tracebloc resources`:\n%s", args, out)
		}
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
