package resources

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

// TestNodeLarger pins set.go:99 — the equal-CPU memory tie-break that no test
// exercised (only CPU-differing nodes were compared), so LargestReadyNode's
// determinism on equal-CPU nodes was unverified.
func TestNodeLarger(t *testing.T) {
	m := func(cpu, mem string) Machine {
		return Machine{CPU: resource.MustParse(cpu), Mem: resource.MustParse(mem)}
	}
	cases := []struct {
		name string
		a, b Machine
		want bool
	}{
		{"more CPU wins", m("8", "16Gi"), m("4", "64Gi"), true},
		{"less CPU loses despite more memory", m("4", "64Gi"), m("8", "16Gi"), false},
		{"equal CPU -> more memory wins (tie-break)", m("8", "32Gi"), m("8", "16Gi"), true},
		{"equal CPU -> less memory loses", m("8", "16Gi"), m("8", "32Gi"), false},
		{"fully equal -> not larger", m("8", "16Gi"), m("8", "16Gi"), false},
	}
	for _, c := range cases {
		if got := nodeLarger(c.a, c.b); got != c.want {
			t.Errorf("%s: nodeLarger = %v, want %v", c.name, got, c.want)
		}
	}
}
