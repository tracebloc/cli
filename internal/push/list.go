package push

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ListDatasets returns the names of the datasets ingested into the
// cluster — the tables in IngestionDatabase — by querying the mysql pod.
// It reuses the same exec seam + pod discovery as Teardown.
//
// The query goes through information_schema (not SHOW TABLES) so a
// cluster where nothing has been pushed yet — the database doesn't even
// exist — returns an empty list rather than an error.
func ListDatasets(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, namespace string) ([]string, error) {
	mysqlPod, mysqlContainer, err := findRunningPod(ctx, cs, namespace, "mysql")
	if err != nil {
		return nil, fmt.Errorf("locating mysql pod: %w", err)
	}
	return listDatasetsWith(ctx, &SPDYExecutor{Config: cfg, Client: cs}, namespace, mysqlPod, mysqlContainer)
}

// listDatasetsWith runs the information_schema table query in the given mysql
// pod through exec and parses the result. It is split out from ListDatasets
// (which locates the pod and builds the production SPDYExecutor) so the
// exec + error-wrap + parse path is unit-testable with a fake Executor.
func listDatasetsWith(ctx context.Context, exec Executor, namespace, pod, container string) ([]string, error) {
	// -N drops the column header so stdout is one bare table name per
	// line. IngestionDatabase is a compile-time constant, so the
	// interpolation carries no injection risk.
	query := fmt.Sprintf(
		"SELECT table_name FROM information_schema.tables WHERE table_schema='%s' ORDER BY table_name",
		IngestionDatabase)
	script := fmt.Sprintf(`mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -N -e "%s"`, query)

	var stdout, stderr bytes.Buffer
	if err := exec.Exec(ctx, namespace, pod, container,
		[]string{"sh", "-c", script}, nil, &stdout, &stderr); err != nil {
		return nil, fmt.Errorf("querying datasets: %w%s", err, stderrSuffix(&stderr))
	}
	return parseDatasetList(stdout.String()), nil
}

// parseDatasetList turns the raw `mysql -N` output (one table name per
// line) into a cleaned slice, dropping blank lines and surrounding
// whitespace. Kept separate from the exec so it's unit-testable without
// a cluster.
func parseDatasetList(raw string) []string {
	var names []string
	for _, line := range strings.Split(raw, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			names = append(names, t)
		}
	}
	return names
}
