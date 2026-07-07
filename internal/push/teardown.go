package push

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// IngestionDatabase is the MySQL schema jobs-manager ingests tables
// into (data-ingestors' target DB). Centralized here so the push path
// and the teardown path agree on where a table lives.
const IngestionDatabase = "training_test_datasets"

// StagingCleanupTimeout bounds the best-effort post-success staging
// reclaim (CleanStaging). The reclaim pod reuses the image the stage pod
// just pulled, so it is normally Ready in seconds; this cap keeps a
// stuck/unschedulable cleanup pod from adding the full pod-ready timeout
// to a command the user already saw succeed.
const StagingCleanupTimeout = 45 * time.Second

// TeardownPlan enumerates the in-cluster artifacts `dataset rm` removes
// for a pushed table: the MySQL table and the dataset's directories on
// the shared PVC.
//
// It deliberately does NOT include the central tracebloc backend
// catalog entry: the CLI has no direct line to that backend (only the
// in-cluster ingestor does, with its own creds). The backend removes
// the dataset's catalog metadata automatically once these in-cluster
// artifacts are gone, so there's no CLI-side catalog teardown to do.
type TeardownPlan struct {
	Database string   // MySQL schema (IngestionDatabase)
	Table    string   // table name — MUST have passed ValidateTableName
	PVCPaths []string // absolute dirs on the shared PVC to rm -rf
}

// PlanTeardown computes the teardown plan for a table. It calls
// FinalDestPrefix/StagedPrefix, which panic on an unsafe name — every
// caller validates with ValidateTableName first (see cli.runDatasetRm).
func PlanTeardown(table string) TeardownPlan {
	return TeardownPlan{
		Database: IngestionDatabase,
		Table:    table,
		PVCPaths: []string{FinalDestPrefix(table), StagedPrefix(table)},
	}
}

// TeardownResult reports what Teardown actually removed.
type TeardownResult struct {
	DroppedTable bool
	RemovedPaths []string
}

// Teardown performs the in-cluster teardown described by plan:
//
//   - DROP the MySQL table by exec-ing `mysql` inside the mysql pod,
//     referencing the pod's own $MYSQL_ROOT_PASSWORD — so no database
//     credential ever transits the CLI.
//   - rm -rf the PVC dirs from a short-lived pod that mirrors the CLI's
//     stage pod (uid 65532 + fsGroup 65532, shared PVC mounted) — built
//     from podOpts via BuildStagePodSpec.
//
// Why an ephemeral stage-identity pod and NOT the long-lived
// jobs-manager pod (tracebloc/client#259): the staging files under
// SharedRoot/.tracebloc-staging/<table> are written by the stage pod as
// uid 65532 (and the ingestor's SharedRoot/<table> files as uid 65534).
// The jobs-manager pod runs as a different non-root uid with no shared
// fsGroup, so its `rm` hit EACCES on 65532-owned files in a
// non-group-writable directory and left orphans. A teardown pod that
// runs as the same uid that wrote the staging files OWNS them, so it
// deletes them by ownership — which works on hostPath (where fsGroup is
// a no-op, kubernetes/kubernetes#138411) and CSI alike.
//
// DESIGN NOTE: still assumes a pod whose name contains "mysql" exposes
// $MYSQL_ROOT_PASSWORD (true for the current parent chart).
func Teardown(ctx context.Context, cs kubernetes.Interface, exec Executor, namespace string, plan TeardownPlan, podOpts PodSpecOptions) (TeardownResult, error) {
	var res TeardownResult

	// 1. DROP the table — mysql pod, localhost, its own root password.
	mysqlPod, mysqlContainer, err := findRunningPod(ctx, cs, namespace, "mysql")
	if err != nil {
		return res, fmt.Errorf("locating mysql pod: %w", err)
	}
	sql := fmt.Sprintf("DROP TABLE IF EXISTS `%s`.`%s`", plan.Database, plan.Table)
	script := fmt.Sprintf(`mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -e '%s'`, sql)
	var stderr bytes.Buffer
	if err := exec.Exec(ctx, namespace, mysqlPod, mysqlContainer,
		[]string{"sh", "-c", script}, nil, nil, &stderr); err != nil {
		return res, fmt.Errorf("dropping %s.%s: %w%s", plan.Database, plan.Table, err, stderrSuffix(&stderr))
	}
	res.DroppedTable = true

	// 2. rm the PVC dirs from an ephemeral stage-identity pod (see the
	//    doc note above + #259). The pod owns the staging files it
	//    deletes, so this works on hostPath and CSI.
	podOpts.Namespace = namespace
	podName, err := CreateStagePod(ctx, cs, podOpts)
	if err != nil {
		return res, fmt.Errorf("creating teardown pod: %w", err)
	}
	// Always clean up the teardown pod — even if the wait/exec fails or
	// the parent ctx is cancelled. Fresh ctx so the delete still reaches
	// the API. Mirrors push.Stage's deferred cleanup.
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = DeleteStagePod(delCtx, cs, namespace, podName)
	}()
	if _, err := WaitForStagePodReady(ctx, cs, namespace, podName); err != nil {
		return res, fmt.Errorf("waiting for teardown pod: %w", err)
	}
	stderr.Reset()
	rmCmd := append([]string{"rm", "-rf"}, plan.PVCPaths...)
	if err := exec.Exec(ctx, namespace, podName, "stage", rmCmd, nil, nil, &stderr); err != nil {
		return res, fmt.Errorf("removing PVC paths: %w%s", err, stderrSuffix(&stderr))
	}
	res.RemovedPaths = plan.PVCPaths
	return res, nil
}

// CleanStaging best-effort removes ONLY the staged source copy at
// StagedPrefix(table) from the shared PVC — never the final table dir
// (FinalDestPrefix) and never the MySQL table.
//
// Why it's needed: the CLI streams a full copy of the dataset into
// SharedRoot/.tracebloc-staging/<table>, and the in-cluster ingestor
// COPIES (shutil.copy, not move) those files into the final table dir.
// So after a successful load the staged source lingers on the PVC until
// a later --overwrite or `data delete`, doubling disk use for
// file-bearing datasets (image/detection/segmentation). Reclaiming it on
// a clean success keeps the shared PVC from silently filling up.
//
// It reuses the same ephemeral stage-identity pod Teardown uses: that
// pod runs as the uid that WROTE the staging files (65532), so it owns
// them and the rm works by ownership on hostPath and CSI alike.
//
// Callers MUST treat a returned error as non-fatal — a leftover source
// copy must never turn an otherwise-successful ingest into a failure —
// and MUST only call this once the ingestion Job has SUCCEEDED (the
// ingestor reads from this path while it runs; removing it mid-run, or
// on a detached/failed run that may be retried, would corrupt the load).
func CleanStaging(ctx context.Context, cs kubernetes.Interface, exec Executor, namespace, table string, podOpts PodSpecOptions) error {
	// Panics on an unsafe name — callers stage only after ValidateTableName.
	staged := StagedPrefix(table)

	podOpts.Namespace = namespace
	// Create under a detached, bounded context: a parent-ctx cancel
	// (Ctrl-C) landing in the create window could otherwise drop a pod the
	// apiserver already committed, orphaning it because the deferred delete
	// below wouldn't yet have a name to reap. A fresh context keeps the
	// create → deferred-delete pair atomic.
	createCtx, cancelCreate := context.WithTimeout(context.Background(), StagingCleanupTimeout)
	defer cancelCreate()
	podName, err := CreateStagePod(createCtx, cs, podOpts)
	if err != nil {
		return fmt.Errorf("creating staging-cleanup pod: %w", err)
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = DeleteStagePod(delCtx, cs, namespace, podName)
	}()
	// Bound the wait+exec so a stuck cleanup pod (unschedulable, slow image
	// pull) can't tack the full 60s ready-timeout onto a command the user
	// already saw succeed — while still honoring a parent-ctx cancel via the
	// child. This reclaim is best-effort; the caller warns and moves on.
	workCtx, cancelWork := context.WithTimeout(ctx, StagingCleanupTimeout)
	defer cancelWork()
	if _, err := WaitForStagePodReady(workCtx, cs, namespace, podName); err != nil {
		return fmt.Errorf("waiting for staging-cleanup pod: %w", err)
	}
	var stderr bytes.Buffer
	if err := exec.Exec(workCtx, namespace, podName, "stage",
		[]string{"rm", "-rf", staged}, nil, nil, &stderr); err != nil {
		return fmt.Errorf("removing staged copy %s: %w%s", staged, err, stderrSuffix(&stderr))
	}
	return nil
}

// findRunningPod returns the name + first-container name of the first
// Running pod in namespace whose name contains substr.
func findRunningPod(ctx context.Context, cs kubernetes.Interface, namespace, substr string) (podName, container string, err error) {
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning && strings.Contains(p.Name, substr) && len(p.Spec.Containers) > 0 {
			return p.Name, p.Spec.Containers[0].Name, nil
		}
	}
	return "", "", fmt.Errorf("no Running pod with name containing %q in namespace %q", substr, namespace)
}

// stderrSuffix renders captured stderr as a parenthetical for error
// messages, or "" when the remote command was quiet.
func stderrSuffix(b *bytes.Buffer) string {
	s := strings.TrimSpace(b.String())
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}
