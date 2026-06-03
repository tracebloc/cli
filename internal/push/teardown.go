package push

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
// in-cluster ingestor does, with its own creds), so removing it is the
// cross-repo follow-up (tracebloc/cli#39). A successfully-ingested
// dataset torn down this way leaves a stale catalog entry until #39
// lands.
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

// Teardown performs the in-cluster teardown described by plan, mirroring
// the manual kubectl-exec cleanup:
//
//   - DROP the MySQL table by exec-ing `mysql` inside the mysql pod,
//     referencing the pod's own $MYSQL_ROOT_PASSWORD — so no database
//     credential ever transits the CLI.
//   - rm -rf the PVC dirs by exec-ing inside the jobs-manager pod,
//     which mounts the shared PVC at SharedRoot.
//
// DESIGN NOTE (under review): this exec-into-existing-pods approach is
// the "CLI-direct teardown". The alternative under discussion is a
// server-side jobs-manager delete endpoint that could also remove the
// backend catalog entry (#39) in one place. It assumes (a) a pod whose
// name contains "mysql" exposes $MYSQL_ROOT_PASSWORD, and (b) the
// jobs-manager pod mounts the shared PVC at SharedRoot — both true for
// the current parent chart, but worth confirming before this ships.
func Teardown(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, namespace string, plan TeardownPlan) (TeardownResult, error) {
	var res TeardownResult
	exec := &SPDYExecutor{Config: cfg, Client: cs}

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

	// 2. rm the PVC dirs — jobs-manager pod mounts the shared PVC.
	jmPod, jmContainer, err := findRunningPod(ctx, cs, namespace, "jobs-manager")
	if err != nil {
		return res, fmt.Errorf("locating jobs-manager pod: %w", err)
	}
	stderr.Reset()
	rmCmd := append([]string{"rm", "-rf"}, plan.PVCPaths...)
	if err := exec.Exec(ctx, namespace, jmPod, jmContainer, rmCmd, nil, nil, &stderr); err != nil {
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
