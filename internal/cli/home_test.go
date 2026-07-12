package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tracebloc/cli/internal/ui"
)

// renderToString renders a model with color off (a buffer isn't a TTY), so the
// assertions match on the literal copy.
func renderToString(m homeModel) string {
	var buf bytes.Buffer
	renderHome(ui.New(&buf), m)
	return buf.String()
}

// TestRenderHome_States pins the locked copy for every state: what MUST appear,
// and — just as important for honesty — what must NOT. The absent[] column is
// the mutation guard: e.g. a "running" screen that ever printed "· Online", or a
// signed-out screen that leaked the data commands, is a regression these catch.
func TestRenderHome_States(t *testing.T) {
	cases := []struct {
		name    string
		model   homeModel
		present []string
		absent  []string
	}{
		{
			name: "online with full compute",
			model: homeModel{
				state: homeOnline, email: "alice@acme.io", envName: "acme-01",
				compute: computeInfo{CPU: 8, MemGiB: 32, GPU: 1}, hasCompute: true,
				inv: binTracebloc, fullMenu: true,
			},
			present: []string{
				"Welcome to your secure environment for AI",
				"Signed in as alice@acme.io",
				`Secure environment "acme-01" · Online (8 CPU · 32 GiB · 1 GPU)`,
				"tracebloc data ingest", "tracebloc data list", "tracebloc data delete",
				"tracebloc cluster doctor", "tracebloc delete",
				"Add --help to any command for the flags.",
				"love from tracebloc",
			},
			// The two axes stay separate + honest: no "running"/"offline"/signed-out
			// language on a healthy online screen.
			absent: []string{"running, but", "offline", "Not signed in", "No secure environment"},
		},
		{
			name: "online without GPU omits the GPU dimension",
			model: homeModel{
				state: homeOnline, email: "a@b.io", envName: "n",
				compute: computeInfo{CPU: 4, MemGiB: 16, GPU: 0}, hasCompute: true,
				inv: binTracebloc, fullMenu: true,
			},
			present: []string{"· Online (4 CPU · 16 GiB)"},
			absent:  []string{"GPU"},
		},
		{
			name: "online without readable compute omits the parenthetical",
			model: homeModel{
				state: homeOnline, email: "a@b.io", envName: "n",
				hasCompute: false, inv: binTracebloc, fullMenu: true,
			},
			present: []string{`Secure environment "n" · Online`},
			absent:  []string{"CPU", "GiB", "("},
		},
		{
			name: "running but not heartbeating never claims Online",
			model: homeModel{
				state: homeRunning, email: "a@b.io", envName: "acme-01",
				inv: binTracebloc, fullMenu: true,
			},
			present: []string{
				`Secure environment "acme-01" · running, but tracebloc hasn't heard from it — run tracebloc cluster doctor`,
			},
			absent: []string{"· Online"},
		},
		{
			name: "offline points at the doctor with the invoked name",
			model: homeModel{
				state: homeOffline, email: "a@b.io", envName: "acme-01",
				inv: binTB, fullMenu: true,
			},
			present: []string{
				`Secure environment "acme-01" · offline — run tb cluster doctor`,
				"tb data ingest", // examples echo the invoked name
			},
			absent: []string{"· Online", "tracebloc data ingest"},
		},
		{
			name:  "signed in, no environment nudges the installer",
			model: homeModel{state: homeNoEnv, email: "a@b.io", inv: binTracebloc},
			present: []string{
				"Signed in as a@b.io",
				"No secure environment on this machine yet — run the installer to set one up.",
				"tracebloc cluster doctor", // still offer to diagnose
			},
			// Lean: don't parade data commands at a machine with no environment.
			absent: []string{"data ingest", "· Online", "Add --help"},
		},
		{
			name:  "not signed in is minimal",
			model: homeModel{state: homeNotSignedIn, inv: binTracebloc},
			present: []string{
				"Welcome to tracebloc — your secure environment for AI",
				"Not signed in yet.",
				"tracebloc login",
				"love from tracebloc",
			},
			absent: []string{"data ingest", "Secure environment", "· Online", "Signed in as"},
		},
		{
			name: "tb tip shown only when invoked as tracebloc with a real alias",
			model: homeModel{
				state: homeOnline, email: "a@b.io", envName: "n",
				inv: binTracebloc, fullMenu: true, tbTip: true,
			},
			present: []string{"type  tb  instead of  tracebloc"},
		},
		{
			name: "no tb tip when it wasn't earned",
			model: homeModel{
				state: homeOnline, email: "a@b.io", envName: "n",
				inv: binTracebloc, fullMenu: true, tbTip: false,
			},
			absent: []string{"type  tb  instead"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderToString(tc.model)
			for _, want := range tc.present {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(got, bad) {
					t.Errorf("unexpected %q in:\n%s", bad, got)
				}
			}
		})
	}
}

// baseDeps is a fully-online, signed-in fake; tests override just the field
// under test.
func baseDeps() homeDeps {
	return homeDeps{
		signIn:      func() (bool, string) { return true, "alice@acme.io" },
		probeEnv:    func(context.Context) envProbe { return envProbe{local: localLive, name: "acme-01"} },
		probeBeat:   func(context.Context) heartbeatState { return beatOnline },
		invoked:     func() string { return binTracebloc },
		tbAvailable: func() bool { return false },
		budget:      2 * time.Second,
	}
}

// TestResolveHomeModel_States drives the state machine through the fakes: every
// combination of sign-in / environment / heartbeat maps to the right state.
func TestResolveHomeModel_States(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*homeDeps)
		want   homeState
	}{
		{
			name:   "signed out",
			mutate: func(d *homeDeps) { d.signIn = func() (bool, string) { return false, "" } },
			want:   homeNotSignedIn,
		},
		{
			name:   "live + heartbeating = online",
			mutate: func(d *homeDeps) {},
			want:   homeOnline,
		},
		{
			// Honesty: a heartbeat we couldn't confirm must degrade to running,
			// NEVER a green Online. Mutation guard — if the code treated unknown as
			// online this flips to homeOnline and fails.
			name:   "live + heartbeat UNKNOWN = running (honesty fallback)",
			mutate: func(d *homeDeps) { d.probeBeat = func(context.Context) heartbeatState { return beatUnknown } },
			want:   homeRunning,
		},
		{
			name:   "live + heartbeat says NOT online = running",
			mutate: func(d *homeDeps) { d.probeBeat = func(context.Context) heartbeatState { return beatNotOnline } },
			want:   homeRunning,
		},
		{
			name: "workload degraded = running",
			mutate: func(d *homeDeps) {
				d.probeEnv = func(context.Context) envProbe { return envProbe{local: localDegraded, name: "acme-01"} }
			},
			want: homeRunning,
		},
		{
			name: "cluster reachable, no release = no environment",
			mutate: func(d *homeDeps) {
				d.probeEnv = func(context.Context) envProbe { return envProbe{local: localNoRelease} }
			},
			want: homeNoEnv,
		},
		{
			name: "unreachable but provisioned (name known) = offline",
			mutate: func(d *homeDeps) {
				d.probeEnv = func(context.Context) envProbe { return envProbe{local: localUnreachable, name: "acme-01"} }
			},
			want: homeOffline,
		},
		{
			name: "unreachable and nothing configured = no environment",
			mutate: func(d *homeDeps) {
				d.probeEnv = func(context.Context) envProbe { return envProbe{local: localUnreachable} }
			},
			want: homeNoEnv,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := baseDeps()
			tc.mutate(&d)
			got := resolveHomeModel(context.Background(), d)
			if got.state != tc.want {
				t.Fatalf("state = %d, want %d", got.state, tc.want)
			}
		})
	}
}

// TestResolveHomeModel_PassesThroughFields checks the model carries email, name,
// and compute from the probes into the render input.
func TestResolveHomeModel_PassesThroughFields(t *testing.T) {
	d := baseDeps()
	d.probeEnv = func(context.Context) envProbe {
		return envProbe{local: localLive, name: "acme-01", compute: computeInfo{CPU: 8, MemGiB: 32, GPU: 2}, hasCompute: true}
	}
	m := resolveHomeModel(context.Background(), d)
	if m.email != "alice@acme.io" || m.envName != "acme-01" {
		t.Fatalf("email/name = %q/%q", m.email, m.envName)
	}
	if !m.hasCompute || m.compute != (computeInfo{CPU: 8, MemGiB: 32, GPU: 2}) {
		t.Fatalf("compute = %+v (has=%v)", m.compute, m.hasCompute)
	}
	if !m.fullMenu {
		t.Fatal("online should show the full menu")
	}
}

// TestResolveHomeModel_TbTip: the tip is earned only when invoked as `tracebloc`
// AND a real alias exists — never when invoked as `tb`, and never on a screen
// with no menu.
func TestResolveHomeModel_TbTip(t *testing.T) {
	t.Run("earned", func(t *testing.T) {
		d := baseDeps()
		d.tbAvailable = func() bool { return true }
		if !resolveHomeModel(context.Background(), d).tbTip {
			t.Fatal("tip should show when invoked as tracebloc with a real alias")
		}
	})
	t.Run("invoked as tb", func(t *testing.T) {
		d := baseDeps()
		d.invoked = func() string { return binTB }
		d.tbAvailable = func() bool { return true }
		if resolveHomeModel(context.Background(), d).tbTip {
			t.Fatal("no tip when the user already uses tb")
		}
	})
	t.Run("signed out never tips", func(t *testing.T) {
		d := baseDeps()
		d.signIn = func() (bool, string) { return false, "" }
		d.tbAvailable = func() bool { return true }
		if resolveHomeModel(context.Background(), d).tbTip {
			t.Fatal("the minimal signed-out screen carries no tip")
		}
	})
}

// TestResolveHomeModel_SlowProbesDegradeFast is the timeout/degrade contract: a
// probe slower than the budget must NOT hold up the screen, and must NOT be able
// to produce a false Online. Both cases assert we return well within the budget
// with the softer state.
func TestResolveHomeModel_SlowProbesDegradeFast(t *testing.T) {
	const budget = 80 * time.Millisecond

	t.Run("slow env probe → renders fast, not online", func(t *testing.T) {
		d := baseDeps()
		d.budget = budget
		d.probeEnv = func(ctx context.Context) envProbe {
			select {
			case <-time.After(5 * time.Second):
				return envProbe{local: localLive, name: "acme-01"} // the "would be online" answer we must NOT wait for
			case <-ctx.Done():
				return envProbe{local: localUnreachable}
			}
		}
		start := time.Now()
		m := resolveHomeModel(context.Background(), d)
		if elapsed := time.Since(start); elapsed > budget+400*time.Millisecond {
			t.Fatalf("took %v, should degrade within ~budget (%v)", elapsed, budget)
		}
		if m.state == homeOnline {
			t.Fatalf("a slow env probe must not yield Online; got state %d", m.state)
		}
	})

	t.Run("slow heartbeat → running, never a false Online", func(t *testing.T) {
		d := baseDeps()
		d.budget = budget
		d.probeEnv = func(context.Context) envProbe { return envProbe{local: localLive, name: "acme-01"} }
		d.probeBeat = func(ctx context.Context) heartbeatState {
			select {
			case <-time.After(5 * time.Second):
				return beatOnline // must NOT be awaited into a green Online
			case <-ctx.Done():
				return beatUnknown
			}
		}
		start := time.Now()
		m := resolveHomeModel(context.Background(), d)
		if elapsed := time.Since(start); elapsed > budget+400*time.Millisecond {
			t.Fatalf("took %v, should degrade within ~budget (%v)", elapsed, budget)
		}
		if m.state != homeRunning {
			t.Fatalf("live env + slow (unconfirmed) heartbeat must be running, got state %d", m.state)
		}
	})
}

// node builds a Ready (unless notReady) node with the given allocatable.
func capNode(name, cpu, mem string, notReady bool, gpu ...string) corev1.Node {
	alloc := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
	if len(gpu) == 1 {
		alloc[gpuResource] = resource.MustParse(gpu[0])
	}
	status := corev1.ConditionTrue
	if notReady {
		status = corev1.ConditionFalse
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: alloc,
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
		},
	}
}

// TestSumCapacity: Ready-node summation, GiB/CPU rounding, GPU aggregation, and
// the all-NotReady → omit contract.
func TestSumCapacity(t *testing.T) {
	t.Run("sums ready nodes, rounds, counts GPUs", func(t *testing.T) {
		got, ok := sumCapacity([]corev1.Node{
			capNode("a", "7900m", "16Gi", false, "1"), // 7.9 CPU → 8
			capNode("b", "4", "16Gi", false, "1"),     // +4 CPU, +1 GPU
			capNode("gone", "8", "32Gi", true),        // NotReady → ignored
		})
		if !ok {
			t.Fatal("expected ok")
		}
		// 7900m + 4000m = 11900m → 11.9 → 12; 32 GiB total; 2 GPU.
		if got != (computeInfo{CPU: 12, MemGiB: 32, GPU: 2}) {
			t.Fatalf("got %+v, want {12 32 2}", got)
		}
	})
	t.Run("no GPU dimension when none present", func(t *testing.T) {
		got, ok := sumCapacity([]corev1.Node{capNode("a", "2", "8Gi", false)})
		if !ok || got.GPU != 0 || got.CPU != 2 || got.MemGiB != 8 {
			t.Fatalf("got %+v ok=%v", got, ok)
		}
	})
	t.Run("all not-ready omits compute", func(t *testing.T) {
		if _, ok := sumCapacity([]corev1.Node{capNode("a", "8", "32Gi", true)}); ok {
			t.Fatal("no Ready node ⇒ ok must be false so the paren is omitted")
		}
	})
	t.Run("empty omits compute", func(t *testing.T) {
		if _, ok := sumCapacity(nil); ok {
			t.Fatal("no nodes ⇒ ok=false")
		}
	})
}

// TestSanitizeInvoked: argv[0] → the name we echo in examples.
func TestSanitizeInvoked(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/tb":           binTB,
		"tb":                          binTB,
		"tb.exe":                      binTB,
		"/opt/homebrew/bin/tracebloc": binTracebloc,
		"tracebloc":                   binTracebloc,
		"tracebloc.exe":               binTracebloc,
		"./cli.test":                  binTracebloc, // go test binary → sensible default
		"main":                        binTracebloc, // `go run` temp name
	}
	for argv0, want := range cases {
		if got := sanitizeInvoked(argv0); got != want {
			t.Errorf("sanitizeInvoked(%q) = %q, want %q", argv0, got, want)
		}
	}
}
