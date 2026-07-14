package doctor

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/tracebloc/cli/internal/cluster"
)

func TestStatus_String(t *testing.T) {
	cases := map[Status]string{
		StatusOK:   "ok",
		StatusWarn: "warn",
		StatusFail: "fail",
		Status(99): "unknown",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func TestNodeReady(t *testing.T) {
	cond := func(s corev1.ConditionStatus) corev1.Node {
		return corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: s}}}}
	}
	if !nodeReady(cond(corev1.ConditionTrue)) {
		t.Error("Ready=True must be ready")
	}
	if nodeReady(cond(corev1.ConditionFalse)) {
		t.Error("Ready=False must not be ready")
	}
	if nodeReady(corev1.Node{}) {
		t.Error("a node with no Ready condition must not be ready")
	}
}

func TestGetDeployment_EmptyCandidateAndMissing(t *testing.T) {
	empty := fake.NewClientset()
	if d := getDeployment(context.Background(), empty, "ns", []string{"", "nope"}); d != nil {
		t.Errorf("getDeployment with only an empty + a missing candidate = %v, want nil", d)
	}
	present := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}})
	if d := getDeployment(context.Background(), present, "ns", []string{"", "x"}); d == nil {
		t.Error("getDeployment must find an existing deployment (skipping the empty candidate)")
	}
}

func TestJobsManagerEnv_SkipsValueFrom(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "tracebloc-jobs-manager", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "jm", Env: []corev1.EnvVar{
				{Name: "PLAIN", Value: "v"},
				{Name: "FROM", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			}}},
		}}},
	}
	env := jobsManagerEnv(context.Background(), fake.NewClientset(dep), "ns",
		&cluster.ParentRelease{ReleaseName: "tracebloc"})
	if env["PLAIN"] != "v" {
		t.Errorf("a plain env value must be read, got %v", env)
	}
	if _, ok := env["FROM"]; ok {
		t.Error("a valueFrom env (no literal value) must be skipped")
	}
}

func TestFindDeployment_AmbiguousUnknownRelease(t *testing.T) {
	// Release unknown (nil) + two suffix-matching deployments → no safe
	// attribution → nil (don't guess across releases).
	cs := fake.NewClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "a-requests-proxy", Namespace: "ns"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "b-requests-proxy", Namespace: "ns"}},
	)
	if d := findDeployment(context.Background(), cs, "ns", nil, "requests-proxy"); d != nil {
		t.Errorf("ambiguous unknown-release match must return nil, got %q", d.Name)
	}
}
