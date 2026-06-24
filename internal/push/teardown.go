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
