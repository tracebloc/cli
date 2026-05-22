package push

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// OrphanGracePeriod is how long a non-Running stage Pod has to live
// before we flag it as an orphan. Running Pods are NEVER flagged
// regardless of age (see FindOrphanStagePods's Phase=Running skip,
// Bugbot r7) — Pods past pod.go's StagePodActiveDeadline of 30 min
// are still legitimate "in-progress active push" candidates, and
// any per-age cap would produce false positives for near-cap pushes.
//
// 5 minutes targets the genuinely-stuck shapes:
//
//   - Phase=Pending past 5 min: image pull failure that didn't
//     resolve, scheduling stuck on missing nodes, PSA rejection
//     that took kubelet a while to surface
//   - Phase=Failed past 5 min: container crashed at startup or
//     during stream, CLI didn't get a chance to clean up
//   - Phase=Unknown past 5 min: node went away (network partition,
//     kubelet crash) and the Pod metadata is stranded
//
// All three shapes are genuinely orphan — no still-running push
// will ever progress past Pending/Failed/Unknown into Ready, so
// 5 min is comfortable for "stuck enough to surface as a warning."
const OrphanGracePeriod = 5 * time.Minute

// Orphan describes a stage Pod found by the orphan scan. Carries
// enough metadata for the warning surface to render an actionable
// "delete with: kubectl delete pod X -n Y" hint.
type Orphan struct {
	// Name is the metadata.name.
	Name string

	// Namespace is the resolved namespace (not the chart's
	// release-name, the actual API namespace).
	Namespace string

	// Table is the destination table the orphan was staging for —
	// read from the StagePodTableLabel set in BuildStagePodSpec.
	// May be empty for very old orphans (pre-label-convention),
	// but every Pod the CLI ever created carries this label.
	Table string

	// Age is how long the Pod has existed (now - creationTimestamp).
	// Surfaces in the warning so customers can spot stale Pods at
	// a glance ("3 hours old" → almost certainly safe to delete;
	// "6 minutes old" → maybe wait, might be from a slow parallel
	// push).
	Age time.Duration
}

// FindOrphanStagePods lists every stage Pod the CLI has ever
// created in the namespace and returns the ones past OrphanGrace
// Period. Pods younger than that are silently filtered — they
// might be the CURRENT invocation's own Pod (the orphan scan runs
// before CreateStagePod, but in a tight enough loop the previous
// invocation's just-created Pod could plausibly still be < grace).
//
// Returns nil + nil if there are no orphans. Doesn't return an
// error for the API call failing — orphan scanning is best-effort
// (an RBAC denial on `list pods` shouldn't block the push) — the
// caller logs the scan failure but proceeds.
//
// Hence the signature returns ([]Orphan, error): the slice is
// empty on success-with-no-orphans, the error is non-nil only on
// API failures the caller might want to surface for debugging.
func FindOrphanStagePods(ctx context.Context, cs kubernetes.Interface, namespace string) ([]Orphan, error) {
	selector := fmt.Sprintf("%s=%s,%s=%s",
		StagePodManagedByLabel, StagePodManagedByValue,
		StagePodComponentLabel, StagePodComponentValue)

	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		// Don't wrap — the caller (cli.runDatasetPush) will either
		// log this for diagnostic value and proceed, or ignore it
		// entirely. Either way, the orphan scan isn't on the
		// critical path.
		return nil, fmt.Errorf("listing stage Pods in %s: %w", namespace, err)
	}

	now := time.Now()
	var orphans []Orphan
	for i := range pods.Items {
		p := &pods.Items[i]

		// Phase=Running means an active push (this workstation or
		// another's) is still doing real work. activeDeadlineSeconds
		// is its safety net at the cluster level. Flagging it as
		// orphan would produce false positives for legitimate
		// slow/near-cap pushes — pod.go budgets ~8.5 minutes for
		// the 1 GiB-cap transfer alone, which exceeds the 5-min
		// grace below. Bugbot flagged the false-positive risk on
		// PR-b round 7.
		//
		// Pods in non-Running phases (Pending/Failed/Unknown) past
		// the grace period are still flagged — those are the
		// genuine orphan shapes (stuck on image pull, crashed
		// during stream, network-partitioned).
		if p.Status.Phase == corev1.PodRunning {
			continue
		}

		age := now.Sub(p.CreationTimestamp.Time)
		if age < OrphanGracePeriod {
			continue
		}
		orphans = append(orphans, Orphan{
			Name:      p.Name,
			Namespace: p.Namespace,
			Table:     p.Labels[StagePodTableLabel],
			Age:       age,
		})
	}
	return orphans, nil
}

// FormatOrphansWarning renders the orphan list as a multi-line
// customer-facing warning. Returns empty string when orphans is
// empty — caller can blind-print without an "if len > 0" check.
//
// The "delete with" hint is the actionable part. v0.1 doesn't
// auto-delete because:
//
//  1. We can't tell from labels alone whether the Pod is stuck
//     mid-transfer for a still-live parallel push from another
//     workstation, vs truly orphaned from a crash.
//  2. Deleting someone else's in-progress push silently is bad
//     UX. A warn-only approach respects the principle of least
//     surprise.
//
// v0.2 can layer on `--cleanup-orphans` for ops folks who want it
// automated.
func FormatOrphansWarning(orphans []Orphan) string {
	if len(orphans) == 0 {
		return ""
	}
	var s string
	s += fmt.Sprintf("WARNING: %d orphan stage Pod%s detected in this namespace — likely "+
		"leftover from a previously crashed `dataset push`:\n",
		len(orphans), pluralS(len(orphans)))
	names := make([]string, 0, len(orphans))
	for _, o := range orphans {
		tableHint := ""
		if o.Table != "" {
			tableHint = fmt.Sprintf(" (table: %s)", o.Table)
		}
		s += fmt.Sprintf("  - %s, age %s%s\n", o.Name, humanDuration(o.Age), tableHint)
		names = append(names, o.Name)
	}
	// Use SPECIFIC Pod names in the delete command, not the
	// label selector. The selector would match every stage Pod
	// in the namespace, including legitimate running ones from
	// parallel pushes (this workstation or another's) — copy-
	// pasting a label-based delete could silently kill someone
	// else's in-progress push. Bugbot flagged the over-broad
	// delete on PR-b round 7.
	s += "Delete with: kubectl delete pod -n " + orphans[0].Namespace + " " +
		strings.Join(names, " ") + "\n"
	return s
}

// pluralS returns "s" for n != 1, else "". Local helper to keep
// internal/push self-contained (the cli package has its own copy
// for the same reason — see cli/ingest.go's plural()).
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// humanDuration is a coarse "X hours" / "X minutes" / "X seconds"
// formatter for orphan warnings. time.Duration.String() returns
// "2h13m45s" which is technically more precise but harder to read
// in a one-line warning.
func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
