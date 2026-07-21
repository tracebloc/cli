package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
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
				state: homeOnline, email: "alice@acme.io", name: "Alice", envName: "acme-01",
				compute: computeInfo{CPU: 8, MemGiB: 32, GPU: 1}, hasCompute: true,
				inv: binTracebloc, fullMenu: true, hasResources: true,
			},
			present: []string{
				"Welcome to your secure environment for AI, Alice 👋", // greeting by name
				"Signed in as alice@acme.io",
				`Secure environment "acme-01" · Online (8 CPU · 32 GiB · 1 GPU)`,
				"Your data",
				"tracebloc data ingest", "tracebloc data list", "tracebloc data delete",
				"Your secure environment",
				"tracebloc resources", // shown because a resources command is registered
				"tracebloc doctor", "tracebloc delete",
				"Add --help to any command for the flags.",
				"──────────────────────────────", // header + footer rule
				"love from tracebloc",
			},
			// The two axes stay separate + honest: no "running"/"offline"/signed-out
			// language on a healthy online screen. The diagnostic is the NEW
			// top-level `doctor`, never the old `cluster doctor`; the "Manage" and
			// "Work with your data" buckets are gone; and the old "type tb instead"
			// tip is gone — the menu itself echoes the launcher name now.
			absent: []string{
				"· running", "offline", "Not signed in", "No secure environment",
				"cluster doctor", "Manage", "Work with your data",
				"type  tb  instead",
			},
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
			// Heartbeat UNCONFIRMED (backend unreachable / timeout / no active
			// client): we don't know tracebloc's view, so the line must not claim
			// it — "couldn't confirm", not "hasn't heard from it".
			name: "running with unconfirmed heartbeat says couldn't-confirm, never Online",
			model: homeModel{
				state: homeRunning, email: "a@b.io", envName: "acme-01",
				inv: binTracebloc, fullMenu: true,
			},
			present: []string{
				`Secure environment "acme-01" · running — couldn't confirm it's connected to tracebloc — run tracebloc doctor`,
			},
			absent: []string{"· Online", "hasn't heard from it"},
		},
		{
			// Heartbeat CONFIRMED not-online (backend says offline/pending):
			// "hasn't heard from it" is literally what that status means — the
			// stronger claim is earned here, and only here.
			name: "running with backend-confirmed not-online says hasn't-heard, never Online",
			model: homeModel{
				state: homeRunning, email: "a@b.io", envName: "acme-01",
				inv: binTracebloc, fullMenu: true, confirmedNotOnline: true,
			},
			present: []string{
				`Secure environment "acme-01" · running, but tracebloc hasn't heard from it — run tracebloc doctor`,
			},
			absent: []string{"· Online", "couldn't confirm"},
		},
		{
			// Fix 3: a not-Ready workload is "starting up", NOT "hasn't heard from
			// it" (the heartbeat was never consulted). Distinct, honest line.
			name: "starting up has its own line, not the heartbeat wording",
			model: homeModel{
				state: homeStarting, email: "a@b.io", envName: "acme-01",
				inv: binTracebloc, fullMenu: true,
			},
			present: []string{
				`Secure environment "acme-01" · starting up, not ready yet — run tracebloc doctor`,
			},
			absent: []string{"· Online", "hasn't heard from it"},
		},
		{
			name: "offline reads honestly for both causes, with the invoked name",
			model: homeModel{
				state: homeOffline, email: "a@b.io", envName: "acme-01",
				inv: binTB, fullMenu: true,
			},
			present: []string{
				`Secure environment "acme-01" · can't reach it from here — run tb doctor`,
				"tb data ingest", // examples echo the invoked name
			},
			// Not the bare "offline" (misleading for the reachable-cluster-wrong-context
			// cause), and never a green Online.
			absent: []string{"· offline", "· Online", "tracebloc data ingest", "cluster doctor"},
		},
		{
			name:  "signed in, no environment nudges the installer",
			model: homeModel{state: homeNoEnv, email: "a@b.io", inv: binTracebloc},
			present: []string{
				"Signed in as a@b.io",
				"No secure environment on this machine yet — run the installer to set one up.",
				"tracebloc doctor", // still offer to diagnose
			},
			// Lean: don't parade data commands at a machine with no environment.
			absent: []string{"data ingest", "· Online", "Add --help", "cluster doctor"},
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
// under test. rememberedClient defaults to (false, "") (this machine was never
// provisioned), so a probe that returns no release degrades to homeNoEnv unless
// a case opts into a provisioned client.
func baseDeps() homeDeps {
	return homeDeps{
		signIn:           func() (bool, string, string) { return true, "alice@acme.io", "" },
		probeEnv:         func(context.Context) envProbe { return envProbe{local: localLive, name: "acme-01"} },
		probeBeat:        func(context.Context) heartbeatState { return beatOnline },
		rememberedClient: func() (bool, string) { return false, "" },
		invoked:          func() string { return binTracebloc },
		tbAvailable:      func() bool { return false },
		hasResources:     func() bool { return false },
		budget:           2 * time.Second,
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
			mutate: func(d *homeDeps) { d.signIn = func() (bool, string, string) { return false, "", "" } },
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
			// Fix 3: a not-Ready workload is its own "starting" state, not running.
			name: "workload degraded = starting",
			mutate: func(d *homeDeps) {
				d.probeEnv = func(context.Context) envProbe { return envProbe{local: localDegraded, name: "acme-01"} }
			},
			want: homeStarting,
		},
		{
			// Fix 1: reachable cluster that doesn't host this release, but the
			// machine WAS provisioned (probe surfaced the name) → offline, NOT the
			// "no environment / run the installer" lie.
			name: "no release but provisioned (probe surfaced name) = offline",
			mutate: func(d *homeDeps) {
				d.probeEnv = func(context.Context) envProbe { return envProbe{local: localNoRelease, name: "acme-01"} }
			},
			want: homeOffline,
		},
		{
			// Fix 1: same, but the name arrives from the remembered-name fallback
			// (the probe returned no name) — the wrong-kube-context / runs-elsewhere
			// case the sibling data commands handle via binding.explain.
			name: "no release but provisioned (remembered name) = offline",
			mutate: func(d *homeDeps) {
				d.probeEnv = func(context.Context) envProbe { return envProbe{local: localNoRelease} }
				d.rememberedClient = func() (bool, string) { return true, "acme-01" }
			},
			want: homeOffline,
		},
		{
			// Review #3: "provisioned" is a namespace signal, NOT a name one. A
			// machine with a cached active-client namespace but no cached display
			// name is still provisioned → offline, never homeNoEnv ("run the
			// installer"). Mutation-proven: revert the classifier to `env.name != ""`
			// and this case flips to homeNoEnv.
			name: "provisioned but no cached name (namespace only) = offline",
			mutate: func(d *homeDeps) {
				d.probeEnv = func(context.Context) envProbe { return envProbe{local: localNoRelease} }
				d.rememberedClient = func() (bool, string) { return true, "" } // provisioned, name not cached
			},
			want: homeOffline,
		},
		{
			// Unchanged: reachable, no release, and never provisioned → no env.
			name: "no release and unprovisioned = no environment",
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

// TestResolveHomeModel_RunningDistinguishesConfirmedNotOnline: both not-Online
// heartbeat answers land on homeRunning, but the model records which KIND so
// the copy can be accurate — true only for a backend-confirmed not-online,
// false for a mere couldn't-confirm, and never set on a healthy Online.
func TestResolveHomeModel_RunningDistinguishesConfirmedNotOnline(t *testing.T) {
	t.Run("backend positively reports not-online", func(t *testing.T) {
		d := baseDeps()
		d.probeBeat = func(context.Context) heartbeatState { return beatNotOnline }
		m := resolveHomeModel(context.Background(), d)
		if m.state != homeRunning || !m.confirmedNotOnline {
			t.Fatalf("state=%d confirmedNotOnline=%v, want running/true", m.state, m.confirmedNotOnline)
		}
	})
	t.Run("unknown heartbeat stays unconfirmed", func(t *testing.T) {
		d := baseDeps()
		d.probeBeat = func(context.Context) heartbeatState { return beatUnknown }
		m := resolveHomeModel(context.Background(), d)
		if m.state != homeRunning || m.confirmedNotOnline {
			t.Fatalf("state=%d confirmedNotOnline=%v, want running/false", m.state, m.confirmedNotOnline)
		}
	})
	t.Run("online never carries the flag", func(t *testing.T) {
		if m := resolveHomeModel(context.Background(), baseDeps()); m.confirmedNotOnline {
			t.Fatal("a healthy Online must not set confirmedNotOnline")
		}
	})
}

// TestResolveHomeModel_ProvisionedNoReleaseKeepsName is the heart of Fix 1: a
// reachable cluster that doesn't host this release, on a provisioned machine,
// must land on a NAMED offline — not the nameless "no environment" screen —
// whether the name came from the probe or the remembered-name fallback.
func TestResolveHomeModel_ProvisionedNoReleaseKeepsName(t *testing.T) {
	t.Run("name from the probe", func(t *testing.T) {
		d := baseDeps()
		d.probeEnv = func(context.Context) envProbe { return envProbe{local: localNoRelease, name: "acme-01"} }
		m := resolveHomeModel(context.Background(), d)
		if m.state != homeOffline || m.envName != "acme-01" {
			t.Fatalf("got state %d name %q, want offline/acme-01", m.state, m.envName)
		}
	})
	t.Run("name from the remembered fallback", func(t *testing.T) {
		d := baseDeps()
		d.probeEnv = func(context.Context) envProbe { return envProbe{local: localNoRelease} } // no name surfaced
		d.rememberedClient = func() (bool, string) { return true, "acme-01" }
		m := resolveHomeModel(context.Background(), d)
		if m.state != homeOffline || m.envName != "acme-01" {
			t.Fatalf("got state %d name %q, want offline/acme-01", m.state, m.envName)
		}
	})
}

// TestResolveHomeModel_PrefersRememberedNameOverReleaseName pins the fix for the
// state-dependent env label: realProbeEnv surfaces the Helm RELEASE name (e.g.
// "tracebloc") when a release is found, but the offline paths surface no name and
// fall back to the remembered CLIENT name (e.g. "acme-01"). Without preferring
// remembered, the same environment reads "tracebloc" Online but "acme-01"
// Offline. A provisioned machine must show the remembered name in every state.
// Mutation guard: revert to `if env.name == ""` and this flips to "tracebloc".
func TestResolveHomeModel_PrefersRememberedNameOverReleaseName(t *testing.T) {
	d := baseDeps()
	d.rememberedClient = func() (bool, string) { return true, "acme-01" } // friendly client name
	// The probe found a release and surfaced its Helm RELEASE name — NOT the
	// friendly client name (this is what realProbeEnv actually returns).
	d.probeEnv = func(context.Context) envProbe { return envProbe{local: localLive, name: "tracebloc"} }
	m := resolveHomeModel(context.Background(), d)
	if m.envName != "acme-01" {
		t.Fatalf("online env name = %q, want the remembered client name %q (not the Helm release name)", m.envName, "acme-01")
	}
}

// TestResolveHomeModel_LeftoverNameNoNamespaceIsNoEnv guards the offline-vs-no-env
// classifier against the display-name override: a leftover ActiveClientName/ID
// with NO cached namespace (provisioned=false) is NOT a reachable environment and
// must fall through to the no-env installer path, not Offline. The split keys off
// the namespace + the PROBE's own surfaced name — never the remembered label that
// resolveHomeModel writes onto env.name for display. Mutation guard: classify on
// env.name (post-override) instead of probeNamedEnv and this flips to homeOffline.
func TestResolveHomeModel_LeftoverNameNoNamespaceIsNoEnv(t *testing.T) {
	d := baseDeps()
	d.probeEnv = func(context.Context) envProbe { return envProbe{local: localNoRelease} } // probe surfaced no name
	// Not provisioned (no cached namespace), but a stale client id lingers in config.
	d.rememberedClient = func() (bool, string) { return false, "stale-id" }
	m := resolveHomeModel(context.Background(), d)
	if m.state != homeNoEnv {
		t.Fatalf("leftover name without a cached namespace must be homeNoEnv (installer path), got state %d (envName %q)", m.state, m.envName)
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

// TestResolveHomeModel_PrefersTB: command examples echo `tb` whenever a tb
// launcher is installed beside the CLI (the normal installed state), falling
// back to the invoked name only when no tb launcher exists — so a bare build
// never prints a `tb` that wouldn't run. Applies signed-in and signed-out.
func TestResolveHomeModel_PrefersTB(t *testing.T) {
	t.Run("tb available → examples use tb even when invoked as tracebloc", func(t *testing.T) {
		d := baseDeps()
		d.invoked = func() string { return binTracebloc }
		d.tbAvailable = func() bool { return true }
		if got := resolveHomeModel(context.Background(), d).inv; got != binTB {
			t.Fatalf("inv = %q, want %q (tb launcher present)", got, binTB)
		}
	})
	t.Run("no tb launcher → fall back to the invoked name", func(t *testing.T) {
		d := baseDeps()
		d.invoked = func() string { return binTracebloc }
		d.tbAvailable = func() bool { return false }
		if got := resolveHomeModel(context.Background(), d).inv; got != binTracebloc {
			t.Fatalf("inv = %q, want %q (no tb launcher → echo the invoked name)", got, binTracebloc)
		}
	})
	t.Run("signed-out screen also prefers tb", func(t *testing.T) {
		d := baseDeps()
		d.signIn = func() (bool, string, string) { return false, "", "" }
		d.invoked = func() string { return binTracebloc }
		d.tbAvailable = func() bool { return true }
		if got := resolveHomeModel(context.Background(), d).inv; got != binTB {
			t.Fatalf("signed-out inv = %q, want %q", got, binTB)
		}
	})
}

// TestResolveHomeModel_SlowProbesDegradeFast is the timeout/degrade contract: a
// probe slower than the budget must NOT hold up the screen, and must NOT be able
// to produce a false Online. Both cases assert we return well within the budget
// with the softer state.
func TestResolveHomeModel_SlowProbesDegradeFast(t *testing.T) {
	const budget = 80 * time.Millisecond

	// Fix 2: a probe that IGNORES its context (a kubeconfig exec-credential plugin
	// like `aws eks get-token`) must not hold up the render, and a provisioned
	// machine must degrade to a NAMED offline — never the "no environment" lie,
	// never Online. Proves the collector bails on the budget without the probe's
	// cooperation, and that the remembered name survives that path.
	t.Run("context-ignoring env probe → named offline, within budget", func(t *testing.T) {
		d := baseDeps()
		d.budget = budget
		d.rememberedClient = func() (bool, string) { return true, "acme-01" } // this machine was provisioned
		started := make(chan struct{})
		d.probeEnv = func(context.Context) envProbe {
			close(started)
			time.Sleep(800 * time.Millisecond)                         // deliberately ignores ctx
			return envProbe{local: localLive, name: "would-be-online"} // the answer we must NOT wait for
		}
		start := time.Now()
		m := resolveHomeModel(context.Background(), d)
		elapsed := time.Since(start)
		<-started // the probe really ran (its late result was abandoned)
		if elapsed > budget+300*time.Millisecond {
			t.Fatalf("took %v; must render at ~budget (%v) without waiting for the probe", elapsed, budget)
		}
		if m.state != homeOffline {
			t.Fatalf("provisioned machine whose probe timed out must be offline, got state %d", m.state)
		}
		if m.envName != "acme-01" {
			t.Fatalf("remembered name must survive the timeout; got %q", m.envName)
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

// TestCollectProbes_BudgetExpiryKeepsBufferedResults: when the budget fires
// while a finished probe's result is already sitting in its buffered channel,
// select picks uniformly among ready cases — the Done branch must DRAIN the
// buffers rather than let the random pick discard a completed answer (a live
// release rendering as offline/no-env). Pre-filled channels + an already-
// expired context make the ready-race deterministic; without the drain each
// round drops a result with probability ≥ 1/3, so 100 rounds catch a
// regression with near certainty.
func TestCollectProbes_BudgetExpiryKeepsBufferedResults(t *testing.T) {
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // budget already spent when collection starts

		envCh := make(chan envProbe, 1)
		beatCh := make(chan heartbeatState, 1)
		envCh <- envProbe{local: localLive, name: "acme-01"} // both probes finished:
		beatCh <- beatOnline                                 // results sit in the buffers

		env, beat := collectProbes(ctx, envCh, beatCh)
		if env.local != localLive || env.name != "acme-01" {
			t.Fatalf("round %d: completed env result dropped: %+v", i, env)
		}
		if beat != beatOnline {
			t.Fatalf("round %d: completed beat result dropped: %v", i, beat)
		}
	}
}

// TestRealProbeEnv_OwnershipGate: with no active-client binding, the probe must
// never go hunting for a release — the kubeconfig-default-namespace lookup and
// the cluster-wide scan can both surface an UNRELATED client (a shared cluster,
// a colleague's install), which the home screen would then greet as "your
// secure environment". Unprovisioned ⇒ bare no-release (→ the honest no-env
// screen), without touching the cluster; provisioned ⇒ the probe proceeds.
func TestRealProbeEnv_OwnershipGate(t *testing.T) {
	// Syntactically valid kubeconfig pointing at an unroutable TEST-NET address:
	// loading succeeds, so only the ownership gate stops the probe before it
	// dials — without the gate this run would end localUnreachable, not
	// localNoRelease.
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://192.0.2.1:1"}
contexts:
- name: ctx
  context: {cluster: c, user: u}
current-context: ctx
users:
- name: u
  user: {}
`
	t.Run("no active client never adopts a foreign release", func(t *testing.T) {
		t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir()) // nothing provisioned here
		kc := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(kc, []byte(kubeconfig), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KUBECONFIG", kc)
		ep := realProbeEnv(context.Background())
		if ep.local != localNoRelease || ep.name != "" {
			t.Fatalf("unprovisioned probe = %+v, want bare localNoRelease (the ownership gate)", ep)
		}
	})
	t.Run("provisioned machines still probe the cluster", func(t *testing.T) {
		writeActiveClientConfig(t, "munich-radiology", "Munich Radiology")
		kc := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(kc, []byte("not a kubeconfig"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KUBECONFIG", kc)
		if ep := realProbeEnv(context.Background()); ep.local != localUnreachable {
			t.Fatalf("provisioned probe must proceed to the cluster (and fail as unreachable here), got %+v", ep)
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

// TestRenderHome_MatchesLockedDemo is the design sign-off: the signed-in /
// Online screen must render byte-for-byte the LOCKED reference layout. The
// expected text is spelled out in full here (self-contained) from that locked
// structure — two-blank header, greeting-by-name, dim 30-col rule, the two
// status axes, the two command buckets (command column padded to a single
// width, descriptions dim), and the dim sign-off. MenuRow spacing is computed
// (not hand-typed) at the same width the renderer uses, so the only thing under
// test is that the renderer emits this exact sequence. Any drift in spacing,
// copy, glyphs, or blank-line count fails here.
// TestRenderHome_EmptyEnvNameNoEmptyQuotes: a provisioned machine can reach an
// offline (or degraded) state with no cached display name — env.name is "". The
// env line must render a clean nameless "Secure environment · …" rather than an
// empty-quoted `Secure environment "" · …`. Fails before the fix (%q on an empty
// name). Covers every named state, since any could carry an empty name defensively.
func TestRenderHome_EmptyEnvNameNoEmptyQuotes(t *testing.T) {
	for _, st := range []homeState{homeOnline, homeRunning, homeStarting, homeOffline} {
		out := renderToString(homeModel{state: st, email: "a@b.io", envName: "", inv: binTracebloc, fullMenu: true})
		if strings.Contains(out, `environment ""`) {
			t.Errorf("state %d rendered an empty-quoted env name:\n%s", st, out)
		}
		if !strings.Contains(out, "Secure environment ·") {
			t.Errorf("state %d should render a nameless 'Secure environment ·' line:\n%s", st, out)
		}
	}
}

func TestRenderHome_MatchesLockedDemo(t *testing.T) {
	// The locked example: signed in, Online, examples rendered in `tb` (a tb
	// launcher is installed beside the CLI), and a resources command registered —
	// matching the demo's inputs.
	m := homeModel{
		state: homeOnline, email: "lukas@tracebloc.io", name: "Lukas", envName: "lukas-01",
		compute: computeInfo{CPU: 8, MemGiB: 32, GPU: 1}, hasCompute: true,
		inv: binTB, fullMenu: true, hasResources: true,
	}

	rule := "  ──────────────────────────────" // 2-space indent + 30 cols
	// row mirrors ui.MenuRow's color-off output at the width the renderer uses for
	// this menu (widest entry: "tb data ingest" / "tb data delete" = 14).
	row := func(cmd, desc string) string { return fmt.Sprintf("  · %-14s    %s", cmd, desc) }

	want := strings.Join([]string{
		"",
		"",
		"  Welcome to your secure environment for AI, Lukas 👋",
		"",
		rule,
		"",
		"  ✓ Signed in as lukas@tracebloc.io",
		`  ✓ Secure environment "lukas-01" · Online (8 CPU · 32 GiB · 1 GPU)`,
		"",
		"",
		"  Your data",
		"",
		row("tb data ingest", "load a dataset into your secure environment"),
		row("tb data list", "list your datasets"),
		row("tb data delete", "remove a dataset"),
		"",
		"", // two blank lines above every section, not just the first
		"  Your secure environment",
		"",
		row("tb resources", "manage compute & memory"),
		row("tb doctor", "check the connection & diagnose issues"),
		row("tb delete", "remove tracebloc from this machine"),
		"",
		"",
		"  Add --help to any command for the flags.",
		"",
		rule,
		"",
		"  love from tracebloc 💚",
		"",
	}, "\n") + "\n"

	if got := renderToString(m); got != want {
		t.Errorf("locked-demo render drifted.\n--- got  (%d bytes) ---\n%q\n--- want (%d bytes) ---\n%q",
			len(got), got, len(want), want)
	}
}

// TestGreetingName pins the name derivation + sanitize rule: profile first name
// wins, else a clean email local-part (capitalized). BOTH candidates must be a
// single letters-only token no longer than greetingNameMax — multi-word,
// control-char, interior-newline, over-long, or otherwise "dirty" candidates
// yield "", keeping the locked greeting on exactly one line.
func TestGreetingName(t *testing.T) {
	cases := []struct {
		name, firstName, email, want string
	}{
		{"profile first name wins outright", "Lukas", "lukas@tracebloc.io", "Lukas"},
		{"profile name trimmed, beats the email", "  Divya  ", "someone@else.io", "Divya"},
		{"clean email local-part, capitalized", "", "lukas@tracebloc.io", "Lukas"},
		{"clean email local-part again", "", "alice@acme.io", "Alice"},
		{"only the first rune is touched", "", "LUKAS@x.io", "LUKAS"},
		{"dotted local-part is not one token", "", "lukas.wuttke@tracebloc.io", ""},
		{"punctuation omits", "", "l.w+tag@x.io", ""},
		{"digits omit", "", "user123@x.io", ""},
		{"empty local-part omits", "", "@nolocal.io", ""},
		{"no @ at all omits", "", "noatsign", ""},
		{"nothing to derive from", "", "", ""},
		// Bound + sanitize (cli#244 review): a candidate that isn't a single,
		// clean, <= greetingNameMax letters token is dropped, never rendered.
		{"over-long email local-part omits", "", strings.Repeat("a", 64) + "@x.io", ""},
		{"over-long FirstName omits", strings.Repeat("A", greetingNameMax+1), "", ""},
		{"FirstName exactly at the cap is used", strings.Repeat("A", greetingNameMax), "", strings.Repeat("A", greetingNameMax)},
		{"interior newline in FirstName omits", "Lu\nkas", "", ""},
		{"tab in FirstName omits", "Lu\tkas", "", ""},
		{"control char in FirstName omits", "\x01Bad", "", ""},
		{"multi-word FirstName omits", "Mary Anne", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := greetingName(c.firstName, c.email); got != c.want {
				t.Errorf("greetingName(%q, %q) = %q, want %q", c.firstName, c.email, got, c.want)
			}
		})
	}
}

// TestResolveHomeModel_Name drives the derivation through the detection layer:
// the profile-name / email-fallback / omit branches all flow into m.name.
func TestResolveHomeModel_Name(t *testing.T) {
	t.Run("profile first name is preferred over the email", func(t *testing.T) {
		d := baseDeps()
		d.signIn = func() (bool, string, string) { return true, "lukas@tracebloc.io", "Divya" }
		if got := resolveHomeModel(context.Background(), d).name; got != "Divya" {
			t.Errorf("name = %q, want Divya", got)
		}
	})
	t.Run("falls back to a clean email local-part, capitalized", func(t *testing.T) {
		d := baseDeps()
		d.signIn = func() (bool, string, string) { return true, "lukas@tracebloc.io", "" }
		if got := resolveHomeModel(context.Background(), d).name; got != "Lukas" {
			t.Errorf("name = %q, want Lukas", got)
		}
	})
	t.Run("omitted when neither yields a clean single token", func(t *testing.T) {
		d := baseDeps()
		d.signIn = func() (bool, string, string) { return true, "lukas.wuttke@tracebloc.io", "" }
		if got := resolveHomeModel(context.Background(), d).name; got != "" {
			t.Errorf("name = %q, want empty", got)
		}
	})
}

// TestRenderHome_ResourcesRowGating: the `resources` row appears iff
// m.hasResources (the render side of the command-tree gate) — driven both ways.
func TestRenderHome_ResourcesRowGating(t *testing.T) {
	base := homeModel{
		state: homeOnline, email: "a@b.io", envName: "n",
		inv: binTracebloc, fullMenu: true,
	}
	t.Run("present when a resources command is registered", func(t *testing.T) {
		m := base
		m.hasResources = true
		if got := renderToString(m); !strings.Contains(got, "tracebloc resources") {
			t.Errorf("resources row missing when registered:\n%s", got)
		}
	})
	t.Run("absent when no resources command is registered", func(t *testing.T) {
		m := base
		m.hasResources = false
		if got := renderToString(m); strings.Contains(got, "resources") {
			t.Errorf("resources row must not appear when the command isn't wired:\n%s", got)
		}
	})
}

// TestHasTopLevelCommand pins the command-tree gate that feeds hasResources:
// commands wired on the root are detected — including `resources`, now that #237
// has landed and root registers it — an unregistered name is not, and adding one
// flips the gate, which is exactly how a gated row appears automatically once its
// command ships.
func TestHasTopLevelCommand(t *testing.T) {
	root := NewRootCmd(BuildInfo{Version: "test"})

	for _, name := range []string{"data", "cluster", "doctor", "delete", "resources"} {
		if !hasTopLevelCommand(root, name) {
			t.Errorf("expected %q to be registered on the root", name)
		}
	}
	const absent = "nonesuch"
	if hasTopLevelCommand(root, absent) {
		t.Errorf("%q must not be registered — the gate would name a non-existent command", absent)
	}
	root.AddCommand(&cobra.Command{Use: absent})
	if !hasTopLevelCommand(root, absent) {
		t.Errorf("%q must be detected once registered, so the gated row appears automatically", absent)
	}
}

// TestDoctor_TopLevelSharesClusterDoctor: the new top-level `doctor` runs the
// SAME code path as the (now hidden) `cluster doctor` alias. Not-signed-in + an
// unreadable kubeconfig makes the shared path deterministic — auth fails, the
// kubeconfig load fails, and the exit escalates to 2 — so both entry points must
// produce the identical exit code and the same shared diagnostic output.
func TestDoctor_TopLevelSharesClusterDoctor(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir()) // signed out
	badKC := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(badKC, []byte("}{ this is not valid kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (string, *exitError) {
		root := NewRootCmd(BuildInfo{Version: "test"})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append(args, "--kubeconfig", badKC))
		err := root.Execute()
		var ee *exitError
		if !errors.As(err, &ee) {
			t.Fatalf("%v: want an *exitError, got %v", args, err)
		}
		return out.String(), ee
	}

	topOut, topErr := run("doctor")
	clOut, clErr := run("cluster", "doctor")

	if topErr.Code() != clErr.Code() {
		t.Errorf("exit codes differ: `doctor`=%d, `cluster doctor`=%d — they must share one code path",
			topErr.Code(), clErr.Code())
	}
	if topErr.Code() != 2 {
		t.Errorf("`doctor` exit = %d, want 2 (auth fail + kubeconfig fail escalates)", topErr.Code())
	}
	for label, out := range map[string]string{"doctor": topOut, "cluster doctor": clOut} {
		if !strings.Contains(out, "Not signed in") {
			t.Errorf("%s output missing the shared diagnostic (not-signed-in):\n%s", label, out)
		}
	}
}

// TestRenderHome_GreetingStaysOneLine is the end-to-end guard for the sanitize
// fix: a profile FirstName carrying an interior newline must never reach the
// header — Para splits on '\n', which would break the locked single-line
// greeting. Here neither the newline FirstName nor the punctuated email yields a
// clean token, so the greeting is the intact nameless one-liner and the unsafe
// bytes never appear.
func TestRenderHome_GreetingStaysOneLine(t *testing.T) {
	d := baseDeps()
	d.signIn = func() (bool, string, string) { return true, "bad.local+tag@x.io", "Bad\nName" }
	got := renderToString(resolveHomeModel(context.Background(), d))

	// The full nameless greeting on one physical line (starts after a newline,
	// ends at one, no interior break) — its exact presence proves the unsafe name
	// was dropped rather than split across lines.
	if !strings.Contains(got, "  Welcome to your secure environment for AI 👋\n") {
		t.Fatalf("greeting must be the intact nameless one-liner (unsafe name dropped), got:\n%q", got)
	}
	if strings.Contains(got, "Bad") {
		t.Fatalf("newline-bearing FirstName leaked into the render:\n%q", got)
	}
}

// TestClusterCmd_DoctorIsHiddenAlias pins the hidden-alias decision directly (so
// it's not only covered via the typo-suggestion test): `doctor` is registered
// top-level AND visible, while `cluster doctor` still exists but is HIDDEN — the
// alias keeps working without re-cluttering the `cluster` help with a duplicate.
func TestClusterCmd_DoctorIsHiddenAlias(t *testing.T) {
	root := NewRootCmd(BuildInfo{Version: "test"})

	find := func(parent *cobra.Command, name string) *cobra.Command {
		for _, c := range parent.Commands() {
			if c.Name() == name {
				return c
			}
		}
		return nil
	}

	top := find(root, "doctor")
	if top == nil || top.Hidden {
		t.Fatalf("top-level `doctor` must be registered and visible (got %v)", top)
	}
	cluster := find(root, "cluster")
	if cluster == nil {
		t.Fatal("cluster command not found")
	}
	alias := find(cluster, "doctor")
	if alias == nil {
		t.Fatal("`cluster doctor` alias must still exist so existing usage keeps working")
	}
	if !alias.Hidden {
		t.Error("`cluster doctor` must be HIDDEN — it's an alias of the top-level `doctor`")
	}
}
