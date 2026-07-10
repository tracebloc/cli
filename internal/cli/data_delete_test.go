package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/push"
)

// resolveDeleteTarget is the backend#1027 fix: `data delete` must match the
// requested name against the client's datasets case-INSENSITIVELY (the
// teardown is case-SENSITIVE, so a mis-cased name used to DROP nothing and
// still exit 0), tear down the REAL spelling, and — because a delete is
// destructive and unrecoverable — fail CLOSED when it can't confirm the
// target. Mirrors TestDestTableExists (same listDatasetsFn seam).
func TestResolveDeleteTarget(t *testing.T) {
	resolved := &cluster.ResolvedConfig{Namespace: "ns"}

	restore := listDatasetsFn
	defer func() { listDatasetsFn = restore }()

	withDatasets := func(names []string, err error) {
		listDatasetsFn = func(_ context.Context, _ kubernetes.Interface, _ *rest.Config, _ string) ([]string, error) {
			return names, err
		}
	}

	t.Run("mis-cased name resolves to the REAL spelling and tears THAT down", func(t *testing.T) {
		withDatasets([]string{"other", "Churn"}, nil)
		matched, err := resolveDeleteTarget(context.Background(), nil, resolved, "churn")
		if err != nil {
			t.Fatalf("a mis-cased name must resolve, not fail: %v", err)
		}
		if matched != "Churn" {
			t.Errorf("matched = %q, want the EXISTING spelling %q", matched, "Churn")
		}
		// The teardown keys on the returned name — a DROP/rm against the raw
		// "churn" flag would silently no-op on a case-sensitive cluster.
		if plan := push.PlanTeardown(matched); plan.Table != "Churn" {
			t.Errorf("teardown plan targets %q, want the real name %q", plan.Table, "Churn")
		}
	})

	t.Run("exact match resolves unchanged", func(t *testing.T) {
		withDatasets([]string{"other", "Churn"}, nil)
		matched, err := resolveDeleteTarget(context.Background(), nil, resolved, "Churn")
		if err != nil || matched != "Churn" {
			t.Errorf("exact match: matched=%q err=%v, want Churn/nil", matched, err)
		}
	})

	t.Run("nonexistent name fails closed with the not-found exit code", func(t *testing.T) {
		withDatasets([]string{"alpha", "beta"}, nil)
		matched, err := resolveDeleteTarget(context.Background(), nil, resolved, "ghost")
		if matched != "" {
			t.Errorf("a no-match must not resolve a target: matched=%q", matched)
		}
		if ExitCodeFromError(err) != 5 {
			t.Fatalf("exit code = %d, want 5 (no such dataset)", ExitCodeFromError(err))
		}
		msg := err.Error()
		if !strings.Contains(msg, "ghost") || !strings.Contains(msg, "alpha") || !strings.Contains(msg, "beta") {
			t.Errorf("not-found error = %q, want it to name the request and the available datasets", msg)
		}
	})

	t.Run("nonexistent name on an empty client says so", func(t *testing.T) {
		withDatasets(nil, nil)
		_, err := resolveDeleteTarget(context.Background(), nil, resolved, "ghost")
		if ExitCodeFromError(err) != 5 {
			t.Fatalf("exit code = %d, want 5", ExitCodeFromError(err))
		}
		if !strings.Contains(err.Error(), "no ingested datasets") {
			t.Errorf("empty-client error = %q, want it to say there are no datasets", err.Error())
		}
	})

	t.Run("listing failure fails CLOSED — refuse, never delete blind", func(t *testing.T) {
		withDatasets(nil, errors.New("mysql pod not found"))
		matched, err := resolveDeleteTarget(context.Background(), nil, resolved, "churn")
		if matched != "" {
			t.Errorf("a broken list must not resolve a target: matched=%q", matched)
		}
		if ExitCodeFromError(err) != 4 {
			t.Fatalf("exit code = %d, want 4 (can't confirm the target)", ExitCodeFromError(err))
		}
		if !strings.Contains(err.Error(), "refusing to delete") || !strings.Contains(err.Error(), "mysql pod not found") {
			t.Errorf("fail-closed error = %q, want it to refuse and surface why", err.Error())
		}
	})
}
