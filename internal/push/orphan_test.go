package push

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// stagePodWithAge constructs a stage-labeled Pod whose
// creationTimestamp is `age` ago. Used to seed the fake clientset
// for each orphan-test variant.
func stagePodWithAge(name, table string, age time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "tracebloc",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			Labels: map[string]string{
				StagePodManagedByLabel: StagePodManagedByValue,
				StagePodComponentLabel: StagePodComponentValue,
				StagePodTableLabel:     table,
			},
		},
	}
}

// TestFindOrphanStagePods_NoOrphans: empty cluster returns empty,
// no error.
func TestFindOrphanStagePods_NoOrphans(t *testing.T) {
	cs := fake.NewClientset()
	got, err := FindOrphanStagePods(context.Background(), cs, "tracebloc")
	if err != nil {
		t.Fatalf("FindOrphanStagePods: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(orphans) = %d, want 0", len(got))
	}
}

// TestFindOrphanStagePods_RecentPodFiltered: a Pod just created
// (well within OrphanGracePeriod) is silently filtered out — it
// might be the current invocation's own Pod from a parallel
// workstation push.
func TestFindOrphanStagePods_RecentPodFiltered(t *testing.T) {
	cs := fake.NewClientset(stagePodWithAge("fresh", "t1", 30*time.Second))
	got, err := FindOrphanStagePods(context.Background(), cs, "tracebloc")
	if err != nil {
		t.Fatalf("FindOrphanStagePods: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("recent Pod surfaced as orphan; got %d, want 0", len(got))
	}
}

// TestFindOrphanStagePods_StaleSurfaces: a Pod past the grace
// period surfaces with its metadata intact (name, table, age).
func TestFindOrphanStagePods_StaleSurfaces(t *testing.T) {
	cs := fake.NewClientset(stagePodWithAge("stale-cats", "cats_dogs", 30*time.Minute))
	got, err := FindOrphanStagePods(context.Background(), cs, "tracebloc")
	if err != nil {
		t.Fatalf("FindOrphanStagePods: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(orphans) = %d, want 1", len(got))
	}
	o := got[0]
	if o.Name != "stale-cats" {
		t.Errorf("Name = %q, want stale-cats", o.Name)
	}
	if o.Table != "cats_dogs" {
		t.Errorf("Table = %q, want cats_dogs", o.Table)
	}
	if o.Age < 29*time.Minute {
		t.Errorf("Age = %v, want at least 29m", o.Age)
	}
}

// TestFindOrphanStagePods_IgnoresNonStagePods: the chart's own
// Pods (managed-by=Helm) and arbitrary user Pods must NOT show
// up — the label selector is the safety boundary.
func TestFindOrphanStagePods_IgnoresNonStagePods(t *testing.T) {
	chartPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "jobs-manager-abc",
			Namespace:         "tracebloc",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-1 * time.Hour)),
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
				"app.kubernetes.io/name":       "client",
			},
		},
	}
	customerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "their-app",
			Namespace:         "tracebloc",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-1 * time.Hour)),
		},
	}
	cs := fake.NewClientset(chartPod, customerPod)
	got, err := FindOrphanStagePods(context.Background(), cs, "tracebloc")
	if err != nil {
		t.Fatalf("FindOrphanStagePods: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("non-stage Pods surfaced as orphans; got %d, want 0; names=%v", len(got), got)
	}
}

// TestFormatOrphansWarning_Empty returns the empty string so the
// caller can blind-print without conditionals.
func TestFormatOrphansWarning_Empty(t *testing.T) {
	if got := FormatOrphansWarning(nil); got != "" {
		t.Errorf("FormatOrphansWarning(nil) = %q, want empty", got)
	}
}

// TestFormatOrphansWarning_ActionableHint: the warning must contain
// (a) the count, (b) the per-pod name + age + table, (c) a
// kubectl delete command the customer can copy-paste. These are
// the "what can I do about this?" surface — losing any of them
// degrades the UX.
func TestFormatOrphansWarning_ActionableHint(t *testing.T) {
	orphans := []Orphan{
		{Name: "stage-a", Namespace: "tracebloc", Table: "cats_dogs", Age: 30 * time.Minute},
		{Name: "stage-b", Namespace: "tracebloc", Table: "xrays", Age: 2 * time.Hour},
	}
	got := FormatOrphansWarning(orphans)
	for _, want := range []string{
		"2 orphan stage Pods",
		"stage-a", "cats_dogs", "30m",
		"stage-b", "xrays", "2h",
		"kubectl delete pod -n tracebloc",
		StagePodManagedByLabel, StagePodComponentLabel,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning missing %q in:\n%s", want, got)
		}
	}
}

// TestFormatOrphansWarning_NoTableLabel: very old orphans from
// before we added StagePodTableLabel might not have it. The
// formatter must not crash or render "(table: )".
func TestFormatOrphansWarning_NoTableLabel(t *testing.T) {
	got := FormatOrphansWarning([]Orphan{
		{Name: "old-orphan", Namespace: "tracebloc", Age: 1 * time.Hour},
	})
	if strings.Contains(got, "(table: )") {
		t.Errorf("warning rendered empty table parenthetical:\n%s", got)
	}
	if !strings.Contains(got, "old-orphan") {
		t.Errorf("warning missing Pod name:\n%s", got)
	}
}
