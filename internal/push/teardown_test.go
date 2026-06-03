package push

import "testing"

// TestPlanTeardown pins the artifact set `dataset rm` targets: the
// MySQL table in IngestionDatabase + both PVC dirs (final dest +
// staging), in that order.
func TestPlanTeardown(t *testing.T) {
	plan := PlanTeardown("reg_train")

	if plan.Database != IngestionDatabase {
		t.Errorf("Database = %q, want %q", plan.Database, IngestionDatabase)
	}
	if plan.Table != "reg_train" {
		t.Errorf("Table = %q, want reg_train", plan.Table)
	}

	want := []string{
		"/data/shared/reg_train",
		"/data/shared/.tracebloc-staging/reg_train",
	}
	if len(plan.PVCPaths) != len(want) {
		t.Fatalf("PVCPaths = %v, want %v", plan.PVCPaths, want)
	}
	for i := range want {
		if plan.PVCPaths[i] != want[i] {
			t.Errorf("PVCPaths[%d] = %q, want %q", i, plan.PVCPaths[i], want[i])
		}
	}
}
