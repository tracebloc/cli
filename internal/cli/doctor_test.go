package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/doctor"
	"github.com/tracebloc/cli/internal/ui"
)

// stubBackend points the newAPIClient seam at an httptest server for one test.
func stubBackend(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	orig := newAPIClient
	newAPIClient = func(string) *api.Client { return &api.Client{BaseURL: srv.URL, HTTP: srv.Client()} }
	t.Cleanup(func() { newAPIClient = orig })
}

// signedInConfig writes a signed-in config with an active client for the tests
// that need to get past the identity gate.
func signedInConfig(t *testing.T) {
	t.Helper()
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "x", Email: "a@b.io", ActiveClientID: "5"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
}

// ── Identity / session gate (runs before any cluster I/O) ──

func TestDoctor_NotSignedIn(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("not signed in → want exit 2, got %v", err)
	}
	if !strings.Contains(out.String(), "Not signed in") || !strings.Contains(out.String(), "login") {
		t.Errorf("want a plain 'Not signed in — run ... login', got:\n%s", out.String())
	}
}

func TestDoctor_SessionExpired401(t *testing.T) {
	signedInConfig(t)
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Invalid token."}`))
	})
	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("401 → want exit 2, got %v", err)
	}
	if !strings.Contains(out.String(), "session expired") || !strings.Contains(out.String(), "login") {
		t.Errorf("want 'Your session expired — run ... login', got:\n%s", out.String())
	}
}

func TestDoctor_OutOfDate426(t *testing.T) {
	signedInConfig(t)
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired) // 426
		_, _ = w.Write([]byte(`{"error":"upgrade_required","min_version":"0.9.0"}`))
	})
	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("426 → want exit 2, got %v", err)
	}
	if !strings.Contains(out.String(), "out of date") || !strings.Contains(out.String(), "tracebloc.io/i.sh") {
		t.Errorf("want 'This CLI is out of date — update it: <installer>', got:\n%s", out.String())
	}
}

// A 401 hard stop with --diagnose must NOT write a bundle: the fix is `login`
// (no bundle needed), and one written here would falsely record "session:
// confirmed" for an expired session. The defer is registered after the session
// probe precisely so 401/426 return first (Bugbot #365).
func TestDoctor_DiagnoseNotWrittenOnExpiredSession(t *testing.T) {
	signedInConfig(t)
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Invalid token."}`))
	})
	tmp := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	var out bytes.Buffer
	err = runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", true)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("401 + --diagnose → want exit 2, got %v", err)
	}
	if strings.Contains(out.String(), "Wrote a support bundle") {
		t.Errorf("must not write a bundle on an expired session, got:\n%s", out.String())
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tracebloc-doctor-") {
			t.Errorf("a bundle file was written on 401 (%s) — should be none", e.Name())
		}
	}
}

// A session fault (here a transport error → tokenUnreachable) that coincides with
// a local-env failure must surface as exit 2 ("a problem was found"), matching the
// full-probe path — not be masked as the local-env exit 3 (Bugbot #365).
func TestDoctor_SessionFaultDominatesEarlyExit(t *testing.T) {
	signedInConfig(t)
	// Point the API client at a closed port so WhoAmI is a transport error →
	// tokenUnreachable (not an APIError).
	origAPI := newAPIClient
	t.Cleanup(func() { newAPIClient = origAPI })
	newAPIClient = func(string) *api.Client {
		return &api.Client{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: 2 * time.Second}}
	}
	// No environment on this machine.
	origLoad := loadClusterFn
	t.Cleanup(func() { loadClusterFn = origLoad })
	loadClusterFn = func(cluster.KubeconfigOptions) (*cluster.ResolvedConfig, error) {
		return nil, errors.New("no kubeconfig here")
	}

	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("session fault + no-env → want exit 2 (problem found), got %v", err)
	}
	if !strings.Contains(out.String(), "No secure environment") {
		t.Errorf("want the no-environment line, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Can't reach tracebloc from here") &&
		!strings.Contains(out.String(), "didn't confirm your session") {
		t.Errorf("want the session fault surfaced alongside no-environment, got:\n%s", out.String())
	}
}

// A bad kubeconfig, with auth healthy, is a local-environment problem (exit 3)
// framed as "no secure environment here yet" — not a Kubernetes error dump.
func TestDoctor_KubeconfigFailIsLocalEnv(t *testing.T) {
	signedInConfig(t)
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`)) // WhoAmI ok
	})
	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "/nonexistent-kubeconfig-xyz", "", "", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 3 {
		t.Fatalf("kubeconfig-fail + auth-OK → want exit 3, got %v", err)
	}
	if !strings.Contains(out.String(), "No secure environment") {
		t.Errorf("want the plain no-environment message, got:\n%s", out.String())
	}
}

// With no --namespace/--context, doctor targets the active client's cached
// namespace (like `cluster info` + the home screen) and prints it as the
// secure-environment name. Mutation-proven: drop bindActiveClientNamespace and
// the printed name becomes the kubeconfig default.
func TestDoctor_BindsActiveClientNamespace(t *testing.T) {
	writeActiveClientConfig(t, "munich-radiology", "Munich Radiology")
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`)) // WhoAmI ok
	})
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
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kc, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	_ = runClusterDoctor(ctx, ui.New(&out, ui.WithColor(false)), kc, "", "", false)
	if !strings.Contains(out.String(), "munich-radiology") {
		t.Fatalf("doctor should name the active client's environment (munich-radiology), got:\n%s", out.String())
	}
}

// ── summarizeDoctor: the roll-up of the granular checks into two plain lines ──

func TestSummarizeDoctor(t *testing.T) {
	res := func(name string, s doctor.Status) doctor.Result { return doctor.Result{Name: name, Status: s} }
	allOK := []doctor.Result{
		res("Cluster reachable", doctor.StatusOK),
		res("Backend egress (from this machine)", doctor.StatusOK),
		res("Service Bus egress (requests-proxy)", doctor.StatusOK),
		res("Pod health", doctor.StatusOK),
		res("Dataset volume (PVC)", doctor.StatusOK),
		res("Node capacity", doctor.StatusOK),
		// Present in the fixture because Run() emits it (backend#2221). Without
		// it every "all healthy" case below silently described a shorter check
		// list than doctor actually produces, and `withDetail` -- which only
		// mutates entries that already exist -- could not reach it.
		res("Machine capacity", doctor.StatusOK),
	}
	with := func(base []doctor.Result, name string, s doctor.Status) []doctor.Result {
		out := make([]doctor.Result, len(base))
		copy(out, base)
		for i := range out {
			if out[i].Name == name {
				out[i].Status = s
			}
		}
		return out
	}
	withDetail := func(base []doctor.Result, name string, s doctor.Status, detail string) []doctor.Result {
		out := with(base, name, s)
		for i := range out {
			if out[i].Name == name {
				out[i].Detail = detail
			}
		}
		return out
	}

	t.Run("all healthy → both OK", func(t *testing.T) {
		c, r := summarizeDoctor(allOK, tokenOK)
		if c.status != doctor.StatusOK || r.status != doctor.StatusOK {
			t.Fatalf("want both OK, got connected=%v ready=%v", c.status, r.status)
		}
		if c.text != "Connected to tracebloc" || r.text != "Ready to run training" {
			t.Errorf("unexpected healthy text: %q / %q", c.text, r.text)
		}
	})

	// Bugbot on #541 (High): checkMachineChain returns StatusWarn for the
	// uncapped-k3d double-count that backend#2221 exists to surface, but the
	// rollup never read "Machine capacity" — so on a machine where Node capacity
	// is OK (the exact case the ticket describes) the default output printed
	// "✔ Ready to run training" + "Everything looks good" and exited 0, with the
	// 2x warning visible only under --verbose. A success the command has not
	// earned is worse than no check at all.
	t.Run("over-committed machine → ready Warn, not a green ✔", func(t *testing.T) {
		results := withDetail(allOK, "Machine capacity", doctor.StatusWarn,
			"Docker VM 7.75 GiB → 2 nodes claiming 15.50 GiB — Kubernetes believes 2.00× the memory this machine has")
		c, r := summarizeDoctor(results, tokenOK)
		if c.status != doctor.StatusOK {
			t.Errorf("connected should stay OK, got %v", c.status)
		}
		if r.status != doctor.StatusWarn {
			t.Fatalf("ready should be Warn on an over-committed machine, got %v (%q)", r.status, r.text)
		}
		if r.text == "Ready to run training" {
			t.Error("ready must not read as an unqualified green")
		}
		if r.remedy == "" {
			t.Error("a warn must carry a remedy")
		}
		// Warn must NOT become a hard failure: training does start here, so the
		// command still exits 0 — it just cannot claim everything looks good.
		// And the closing line must own the problem: StatusWarn, not the
		// StatusUnknown "couldn't finish some checks" partial, which would say
		// no problem was found right under the ⚠ that reported one (Bugbot).
		switch v := doctorVerdict(c.status, r.status); v {
		case doctor.StatusFail:
			t.Error("an over-committed machine must not fail the command — training does run")
		case doctor.StatusOK:
			t.Error(`"everything looks good" must not survive an over-committed machine`)
		case doctor.StatusUnknown:
			t.Error(`the closing line must not claim "no problems found" — a problem was just printed`)
		}
	})

	// Bugbot on #561/#566 (backend#2438): a real Machine-capacity Warn co-occurs
	// with a can't-check condition (pod-list RBAC failure, or unreadable
	// RESOURCE_REQUESTS). The readiness rollup used to consult the can't-check
	// arms before the Warn arm, so ready collapsed to StatusUnknown and the
	// closing line printed "No problems found, but some checks couldn't finish"
	// on top of the over-commit it exists to report — StatusUnknown carries no
	// signal and must never shadow a found Warn. Assert the Warn survives (and the
	// verdict owns it) under every can't-check that can fire alongside it.
	t.Run("over-commit Warn is not shadowed by a co-occurring Unknown", func(t *testing.T) {
		base := withDetail(allOK, "Machine capacity", doctor.StatusWarn,
			"Docker VM 7.75 GiB → 2 nodes claiming 15.50 GiB — Kubernetes believes 2.00× the memory this machine has")
		cases := map[string][]doctor.Result{
			"pod-list RBAC failure": withDetail(base, "Pod health", doctor.StatusWarn,
				"could not list pods: pods is forbidden"),
			"RESOURCE_REQUESTS unreadable": withDetail(base, "Node capacity", doctor.StatusWarn,
				"couldn't read RESOURCE_REQUESTS from jobs-manager — skipping node-fit"),
			"nodes unlistable": withDetail(base, "Node capacity", doctor.StatusWarn,
				"could not list nodes: nodes is forbidden"),
		}
		// Per-case subtests: map iteration is randomized, so a shared loop with
		// t.Fatalf would report a nondeterministic single case and hide the rest.
		for name, results := range cases {
			t.Run(name, func(t *testing.T) {
				c, r := summarizeDoctor(results, tokenOK)
				if r.status != doctor.StatusWarn {
					t.Fatalf("a can't-check must not shadow the over-commit Warn, got ready=%v (%q)", r.status, r.text)
				}
				if v := doctorVerdict(c.status, r.status); v != doctor.StatusWarn {
					t.Errorf("verdict must own the found Warn, not report a partial/unknown, got %v", v)
				}
			})
		}
	})

	// The same severity guarantee for the Fail tier: a can't-check Unknown
	// ("could not list pods") must not shadow a co-occurring, unrelated Fail.
	// Before the severity-tier reorder the pod-list Unknown arm sat above the
	// Node-capacity / dataset Fail arms and swallowed them (backend#2438) — the
	// Warn variant above got the fix + a test; this pins the Fail variant too.
	t.Run("Fail is not shadowed by a co-occurring Unknown", func(t *testing.T) {
		base := withDetail(allOK, "Pod health", doctor.StatusWarn,
			"could not list pods: pods is forbidden")
		cases := map[string][]doctor.Result{
			"node capacity Fail": withDetail(base, "Node capacity", doctor.StatusFail,
				"0 of 2 nodes can fit the training request"),
			"dataset storage Fail": withDetail(base, "Dataset volume (PVC)", doctor.StatusFail,
				"PVC is not bound"),
		}
		for name, results := range cases {
			t.Run(name, func(t *testing.T) {
				c, r := summarizeDoctor(results, tokenOK)
				if r.status != doctor.StatusFail {
					t.Fatalf("a can't-check must not shadow the Fail, got ready=%v (%q)", r.status, r.text)
				}
				if v := doctorVerdict(c.status, r.status); v != doctor.StatusFail {
					t.Errorf("verdict must own the Fail, not report a partial/unknown, got %v", v)
				}
			})
		}
	})

	t.Run("machine capacity unknown → ready stays OK (no alarm)", func(t *testing.T) {
		// StatusUnknown carries no signal (non-k3d cluster, unreadable VM), so it
		// must not degrade the verdict either way.
		_, r := summarizeDoctor(with(allOK, "Machine capacity", doctor.StatusUnknown), tokenOK)
		if r.status != doctor.StatusOK {
			t.Fatalf("an unknown machine-capacity must not move the verdict, got %v", r.status)
		}
	})

	t.Run("unreachable → connected Fail, ready can't-check", func(t *testing.T) {
		c, r := summarizeDoctor(with(allOK, "Cluster reachable", doctor.StatusFail), tokenOK)
		if c.status != doctor.StatusFail {
			t.Errorf("connected should Fail when unreachable, got %v", c.status)
		}
		if r.status != doctor.StatusUnknown {
			t.Errorf("ready should be Unknown (can't check) when unreachable, got %v", r.status)
		}
		if !strings.Contains(r.text, "can't check") {
			t.Errorf("ready text should say can't check, got %q", r.text)
		}
	})

	t.Run("node capacity can't-check → ready can't-check, never green", func(t *testing.T) {
		// Both can't-check Warn details (nodes unlistable, RESOURCE_REQUESTS
		// unreadable) must roll up to an honest Unknown — a ✔ here would assert
		// readiness without a capacity probe (Bugbot).
		for _, detail := range []string{
			"could not list nodes: nodes is forbidden",
			"couldn't read RESOURCE_REQUESTS from jobs-manager — skipping node-fit",
			// backend#2870: unreadable pod list -> free unverifiable -> can't-check,
			// not a green pass (Bugbot High: this used to roll up to "Ready").
			//
			// BUILT FROM THE PRODUCER'S CONSTANT, not retyped. This string was a
			// third copy of the prefix the classifier matches on -- so a producer
			// that reworded it would leave this test passing against a phrase
			// nothing emits any more.
			doctor.CantVerifyFreeCompute + ", so free compute could not be verified — checked against allocatable only; an over-committed control plane would be invisible here",
			// Bugbot High on #628: the same can't-check ARRIVING WITH the soft GPU
			// warn. The GPU case fires first in checkNodeFit, so this combined
			// detail is what a GPU-requesting install with an unreadable pod list
			// actually produces -- and it must roll up the same way.
			doctor.CantVerifyFreeCompute + ", so free compute could not be verified — checked against allocatable only; an over-committed control plane would be invisible here. Also, no single Ready node satisfies cpu+memory AND nvidia.com/gpu, so GPU jobs would rely on the CPU fallback (needs cpu=2, memory=8Gi)",
		} {
			_, r := summarizeDoctor(withDetail(allOK, "Node capacity", doctor.StatusWarn, detail), tokenOK)
			if r.status != doctor.StatusUnknown {
				t.Errorf("%q: ready should be Unknown, got %v", detail, r.status)
			}
			if !strings.Contains(r.text, "couldn't check free compute") {
				t.Errorf("%q: ready text should say couldn't check free compute, got %q", detail, r.text)
			}
		}
	})

	t.Run("over-commit Fail must NOT advise sizing runs to the machine", func(t *testing.T) {
		// Bugbot Medium on #628. The two Node-capacity Fails need OPPOSITE advice:
		// "no node is big enough" is fixed by giving the machine more (or sizing
		// runs to it); "big enough, but not beside what is already running" is
		// made WORSE by that, because `resources set max` measures the machine's
		// total, which is the figure this Fail rejected. A user who follows it asks
		// for more and stays stuck.
		//
		// DETAIL BUILT FROM THE PRODUCER'S CONSTANT so the arm and the producer
		// cannot drift apart -- the classification is by prefix, so a reworded
		// producer would silently fall through to the generic arm again.
		_, r := summarizeDoctor(withDetail(allOK, "Node capacity", doctor.StatusFail,
			doctor.OverCommitted+" for a training job (cpu=2, memory=8Gi) but not beside what is already running on it — the envelope over-asks the node's FREE memory, so the pod schedules Pending"), tokenOK)
		if r.status != doctor.StatusFail {
			t.Fatalf("over-commit is still Not ready, got %v", r.status)
		}
		if strings.Contains(r.remedy, "resources set max") {
			t.Errorf("the top-line remedy tells the user to size runs to the machine, which raises the ask this Fail rejected: %q", r.remedy)
		}
		if !strings.Contains(r.remedy, "Do NOT") {
			t.Errorf("the remedy should warn against sizing to the machine, got %q", r.remedy)
		}
	})

	t.Run("over-commit outranks stuck-Pending, which it CAUSES", func(t *testing.T) {
		// Bugbot Medium on #628, second pass -- and the case the test above could
		// not reach. That one starts from `allOK`, so Pod health is OK and the
		// over-commit arm is the first Fail either way. The bug lived in the state
		// where BOTH fire.
		//
		// THEY CO-OCCUR BY CONSTRUCTION, which is what makes this ordering a
		// correctness question and not a preference: the producer's Detail ends
		// "so the pod schedules Pending" (internal/doctor/doctor.go:778), so an
		// over-committed node is EXPECTED to also have pods stuck Pending. With
		// the stuck-Pending arm first, the rollup printed `computeRemedy` -- which
		// ends in `resources set max` -- in the one state the Node-capacity Fail
		// exists to refuse. The figure was fixed on the previous commit and the
		// rollup went on recommending the thing.
		//
		// Both details are built from the producer's own constant/text rather than
		// retyped, so a reworded producer reddens this instead of silently falling
		// through to the generic arm.
		results := withDetail(allOK, "Node capacity", doctor.StatusFail,
			doctor.OverCommitted+" for a training job (cpu=2, memory=8Gi) but not beside what is already running on it — the envelope over-asks the node's FREE memory, so the pod schedules Pending")
		results = withDetail(results, "Pod health", doctor.StatusWarn,
			"1 pod stuck Pending past the grace window")

		_, r := summarizeDoctor(results, tokenOK)
		if r.status != doctor.StatusFail {
			t.Fatalf("want Fail, got %v", r.status)
		}
		if strings.Contains(r.remedy, "resources set max") {
			t.Errorf("the stuck-Pending arm shadowed the over-commit arm and put `set max` back in front of the operator, in the exact state the Fail refuses: %q", r.remedy)
		}
		if !strings.Contains(r.remedy, "Do NOT") {
			t.Errorf("want the over-commit remedy, got the generic one: %q", r.remedy)
		}
		if !strings.Contains(r.text, "already claimed the room") {
			t.Errorf("want the over-commit top line, got %q", r.text)
		}
	})

	t.Run("a hard Pod-health Fail still outranks over-commit", func(t *testing.T) {
		// The other side of the reorder: over-commit was moved above the
		// stuck-Pending WARN, not above the Pod-health FAIL. Pods not running at
		// all is a different problem with a different fix (reinstall), and it is
		// not caused by over-commitment -- so it must still win. Without this,
		// "move it up" could keep sliding until it shadowed a harder failure.
		results := withDetail(allOK, "Node capacity", doctor.StatusFail,
			doctor.OverCommitted+" for a training job (cpu=2, memory=8Gi) but not beside what is already running on it")
		results = withDetail(results, "Pod health", doctor.StatusFail,
			"2 pods CrashLoopBackOff")

		_, r := summarizeDoctor(results, tokenOK)
		if r.status != doctor.StatusFail {
			t.Fatalf("want Fail, got %v", r.status)
		}
		if !strings.Contains(r.text, "isn't running") {
			t.Errorf("a hard Pod-health Fail must still win the rollup, got %q", r.text)
		}
	})

	t.Run("generic capacity Fail still gets the sizing advice", func(t *testing.T) {
		// The other side: the fix must not strip the correct advice from the Fail
		// it IS correct for -- a machine that is genuinely too small.
		_, r := summarizeDoctor(withDetail(allOK, "Node capacity", doctor.StatusFail,
			"no Ready node can fit a training job (needs cpu=2, memory=8Gi)"), tokenOK)
		if r.status != doctor.StatusFail {
			t.Fatalf("want Fail, got %v", r.status)
		}
		if !strings.Contains(r.remedy, "resources set max") {
			t.Errorf("a too-small machine should still be offered the sizing fix: %q", r.remedy)
		}
	})

	// backend#2870: the TRANSIENT shortage. The envelope fits the machine beside
	// the platform, but a running job holds the room, so the next run waits. It
	// must roll up as a Warn that says so -- not the green (which hides why a
	// second run is waiting), not a Fail (training IS running: the Bugbot High on
	// #628), and never the capacity Fail's "ask for less / grow the machine"
	// advice, which changes nothing about a job already running.
	t.Run("a running job holding the room → ready Warn that says to wait, not resize", func(t *testing.T) {
		// DETAIL BUILT FROM THE PRODUCER'S CONSTANT, same discipline as the
		// over-commit cases above: the arm classifies by prefix.
		_, r := summarizeDoctor(withDetail(allOK, "Node capacity", doctor.StatusWarn,
			doctor.HeldByRunningJob+": a Ready node fits a training job (cpu=1, memory=4864Mi) beside the platform's own pods, but running job(s) on n1 hold cpu=1, memory=4864Mi right now, so the next run waits Pending until they finish"), tokenOK)
		if r.status != doctor.StatusWarn {
			t.Fatalf("want ready Warn, got %v (%q)", r.status, r.text)
		}
		if !strings.Contains(r.text, "waits for it") {
			t.Errorf("the top line should say the next run waits, got %q", r.text)
		}
		if strings.Contains(r.remedy, "resources set max") || strings.Contains(r.remedy, "Ask for less") {
			t.Errorf("the transient remedy must not send the operator to resize or shrink: %q", r.remedy)
		}
		if !strings.Contains(r.remedy, "let the running job finish") {
			t.Errorf("the remedy should say to wait for or stop the running job: %q", r.remedy)
		}
		c, _ := summarizeDoctor(allOK, tokenOK)
		if v := doctorVerdict(c.status, r.status); v != doctor.StatusWarn {
			t.Errorf("verdict must own the Warn (exit 0, no 'everything looks good'), got %v", v)
		}
	})

	// Bugbot High on #639. The transient shortage's own SYMPTOM is a second
	// training pod sitting Pending until the running job frees the room -- the
	// `waiting_for_capacity` case. With the stuck-Pending Fail arm first, that
	// state rolled up to "Not ready ... not enough free compute" with
	// `computeRemedy` (`resources set max`) at exit 2, and the wait-for-the-job
	// line never appeared in exactly the case it was written for.
	t.Run("a Pending pod AND a running job holding the room → the cause is named, not the generic stuck-Pending Fail", func(t *testing.T) {
		results := withDetail(allOK, "Node capacity", doctor.StatusWarn,
			doctor.HeldByRunningJob+": a Ready node fits a training job (cpu=1, memory=4864Mi) beside the platform's own pods, but running job(s) on n1 hold cpu=1, memory=4864Mi right now, so the next run waits Pending until they finish")
		results = withDetail(results, "Pod health", doctor.StatusWarn,
			"Pending > 5m0s: [train-second]")
		c, r := summarizeDoctor(results, tokenOK)
		if r.status == doctor.StatusFail {
			t.Fatalf("the stuck-Pending arm shadowed the transient cause and failed the command on healthy training: %q / %q", r.text, r.remedy)
		}
		if r.status != doctor.StatusWarn {
			t.Fatalf("want ready Warn, got %v (%q)", r.status, r.text)
		}
		if !strings.Contains(r.text, "waiting for it") {
			t.Errorf("the top line should say the next run waits for the running one, got %q", r.text)
		}
		if !strings.Contains(r.remedy, "running job holds") {
			t.Errorf("the remedy should name the cause -- a running job -- got %q", r.remedy)
		}
		if strings.Contains(r.remedy, "resources set max") || strings.Contains(r.remedy, "Ask for less") {
			t.Errorf("the remedy must not send the operator to resize or shrink: %q", r.remedy)
		}
		if !strings.Contains(r.remedy, "still waiting after") {
			t.Errorf("checkPods cannot see WHY a pod is Pending, so the remedy must say what to do if the wait outlives the job: %q", r.remedy)
		}
		if v := doctorVerdict(c.status, r.status); v != doctor.StatusWarn {
			t.Errorf("verdict must own the Warn (exit 0), got %v", v)
		}
	})

	t.Run("a Pending pod with NO running job is still the stuck-Pending Fail", func(t *testing.T) {
		// The other side: the arm above is scoped to the co-occurrence. A pod
		// Pending on a machine where nothing holds the room is the generic,
		// measured-nowhere case and keeps its Fail and its sizing advice.
		_, r := summarizeDoctor(withDetail(allOK, "Pod health", doctor.StatusWarn,
			"Pending > 5m0s: [trainer-x]"), tokenOK)
		if r.status != doctor.StatusFail || !strings.Contains(r.remedy, "resources set max") {
			t.Errorf("want the generic stuck-Pending Fail with the sizing remedy, got %v %q", r.status, r.remedy)
		}
	})

	t.Run("a crash-looping pod still outranks the running-job explanation", func(t *testing.T) {
		// The exception is scoped to the stuck-Pending WARN; a Pod-health FAIL is
		// a measured failure with a different fix, and must keep winning.
		results := withDetail(allOK, "Node capacity", doctor.StatusWarn,
			doctor.HeldByRunningJob+": a Ready node fits a training job beside the platform's own pods, but running job(s) on n1 hold cpu=1, memory=4864Mi right now")
		results = withDetail(results, "Pod health", doctor.StatusFail, "crash-looping: [jobs-manager]")
		_, r := summarizeDoctor(results, tokenOK)
		if r.status != doctor.StatusFail || !strings.Contains(r.text, "isn't running") {
			t.Errorf("a Pod-health Fail must still win, got %v (%q)", r.status, r.text)
		}
	})

	t.Run("a machine that lies about its size outranks a running job", func(t *testing.T) {
		// Both are Warns; the over-commit is the more consequential finding and
		// sits first. Pin the order so a later reshuffle cannot demote it.
		results := withDetail(allOK, "Machine capacity", doctor.StatusWarn,
			"Docker VM 7.75 GiB → 2 nodes claiming 15.50 GiB — Kubernetes believes 2.00× the memory this machine has")
		results = withDetail(results, "Node capacity", doctor.StatusWarn,
			doctor.HeldByRunningJob+": a Ready node fits a training job beside the platform's own pods, but running job(s) on n1 hold cpu=1, memory=4864Mi right now")
		_, r := summarizeDoctor(results, tokenOK)
		if r.status != doctor.StatusWarn || !strings.Contains(r.text, "bigger than it is") {
			t.Errorf("want the over-commit Warn first, got %v (%q)", r.status, r.text)
		}
	})

	t.Run("node capacity GPU-soft warn → still ready", func(t *testing.T) {
		_, r := summarizeDoctor(withDetail(allOK, "Node capacity", doctor.StatusWarn,
			"no single Ready node satisfies cpu+memory AND nvidia.com/gpu — GPU jobs rely on the CPU fallback (needs cpu=2, memory=8Gi)"), tokenOK)
		if r.status != doctor.StatusOK {
			t.Errorf("GPU-soft warn must stay Ready (CPU fallback), got %v", r.status)
		}
	})

	t.Run("token unreachable → connected Fail", func(t *testing.T) {
		c, _ := summarizeDoctor(allOK, tokenUnreachable)
		if c.status != doctor.StatusFail || !strings.Contains(c.text, "can't reach tracebloc") {
			t.Errorf("token-unreachable → want connected Fail 'can't reach tracebloc', got %v %q", c.status, c.text)
		}
	})

	t.Run("results egress down → connected Fail (experiments stall)", func(t *testing.T) {
		c, _ := summarizeDoctor(with(allOK, "Service Bus egress (requests-proxy)", doctor.StatusFail), tokenOK)
		if c.status != doctor.StatusFail || !strings.Contains(c.text, "results can't reach") {
			t.Errorf("want connected Fail on results-egress down, got %v %q", c.status, c.text)
		}
	})

	t.Run("no compute → ready Fail", func(t *testing.T) {
		_, r := summarizeDoctor(with(allOK, "Node capacity", doctor.StatusFail), tokenOK)
		// GOOS-independent invariant: every variant carries the resize fix
		// (#400 — the old string pinned "Docker Desktop", wrong on WSL2/linux).
		if r.status != doctor.StatusFail || !strings.Contains(r.remedy, "resources set max") {
			t.Errorf("want ready Fail with a resize remedy, got %v remedy=%q", r.status, r.remedy)
		}
	})

	// computeRemedy (#400): the compute remedy must match the host's actual
	// memory lever — Docker Desktop's Resources slider does not exist on the
	// WSL2 backend, and bare Linux has no Docker Desktop at all.
	t.Run("computeRemedy per GOOS", func(t *testing.T) {
		win := computeRemedy("windows")
		if !strings.Contains(win, ".wslconfig") || !strings.Contains(win, "Hyper-V") {
			t.Fatalf("windows remedy must name both levers: %q", win)
		}
		mac := computeRemedy("darwin")
		if !strings.Contains(mac, "Docker Desktop → Settings → Resources → Advanced") {
			t.Fatalf("darwin remedy keeps the slider: %q", mac)
		}
		lin := computeRemedy("linux")
		if strings.Contains(lin, "Docker Desktop") || strings.Contains(lin, "wslconfig") {
			t.Fatalf("linux remedy must not name Docker Desktop/WSL: %q", lin)
		}
		for _, r := range []string{win, mac, lin} {
			if !strings.Contains(r, "resources set max") {
				t.Fatalf("every remedy carries the resize fix: %q", r)
			}
		}
	})

	t.Run("component down → ready Fail (reinstall/support)", func(t *testing.T) {
		_, r := summarizeDoctor(with(allOK, "Pod health", doctor.StatusFail), tokenOK)
		if r.status != doctor.StatusFail || !strings.Contains(r.remedy, "tracebloc.io/i.sh") {
			t.Errorf("want ready Fail with a reinstall remedy, got %v remedy=%q", r.status, r.remedy)
		}
	})

	// Pod health has TWO StatusWarn sources (checkPods): pods stuck Pending, and a
	// failure to list pods at all (e.g. RBAC). They must roll up differently.
	warnPods := func(detail string) []doctor.Result {
		out := with(allOK, "Pod health", doctor.StatusWarn)
		for i := range out {
			if out[i].Name == "Pod health" {
				out[i].Detail = detail
			}
		}
		return out
	}

	// Pods stuck Pending past the grace window: training can't schedule, so the
	// rollup must NOT report ✔ "Ready to run training" — that false green was the
	// original Bugbot finding.
	t.Run("pods stuck pending (warn) → ready Fail, not a false green", func(t *testing.T) {
		_, r := summarizeDoctor(warnPods("Pending > 5m0s: [trainer-x]"), tokenOK)
		if r.status != doctor.StatusFail {
			t.Fatalf("stuck-pending pods must roll up to not-ready, got %v %q", r.status, r.text)
		}
		if !strings.Contains(r.text, "Not ready") {
			t.Errorf("want a Not-ready readiness line for stuck-pending pods, got %q", r.text)
		}
	})

	// The other Pod-health Warn source, "could not list pods" (RBAC), is a
	// can't-check — it must NOT get the stuck-pending/compute (Docker Desktop)
	// remedy (Bugbot follow-up).
	t.Run("pod-health warn = could not list pods (RBAC) → can't-check, not stuck-pending", func(t *testing.T) {
		_, r := summarizeDoctor(warnPods("could not list pods: pods is forbidden"), tokenOK)
		if r.status == doctor.StatusFail {
			t.Errorf("a can't-list-pods warn must not be a hard not-ready, got %v %q", r.status, r.text)
		}
		if strings.Contains(r.remedy, "Docker Desktop") {
			t.Errorf("must not give the stuck-pending/compute remedy for a read failure, got remedy=%q", r.remedy)
		}
	})

	// A reachable cluster with no tracebloc installed must NOT be reported as
	// "isn't answering" with a kubectl remedy — it's a reinstall (Bugbot #365).
	// A failing image-pull check means training images can't be fetched — that
	// is not-ready, and must not be silently dropped from the rollup (Bugbot #365).
	t.Run("images can't be pulled → ready Fail", func(t *testing.T) {
		withPull := append(append([]doctor.Result{}, allOK...), res("Image pull secret", doctor.StatusFail))
		_, r := summarizeDoctor(withPull, tokenOK)
		if r.status != doctor.StatusFail || !strings.Contains(r.text, "images can't be pulled") {
			t.Errorf("image-pull down → want ready Fail 'images can't be pulled', got %v %q", r.status, r.text)
		}
		if !strings.Contains(r.remedy, "--diagnose") {
			t.Errorf("image-pull remedy should point at --diagnose, got %q", r.remedy)
		}
	})

	// A backend that ANSWERED with an error (5xx/403/decode) is a tracebloc-side
	// problem — it must not be blamed on the user's network with a proxy remedy
	// (Bugbot #365).
	t.Run("backend answered with an error → connected Fail (support, not network)", func(t *testing.T) {
		c, r := summarizeDoctor(allOK, tokenServerErr)
		if c.status != doctor.StatusFail || !strings.Contains(c.text, "server error") {
			t.Errorf("server-err → want connected Fail 'server error', got %v %q", c.status, c.text)
		}
		if strings.Contains(c.remedy, "PROXY") || strings.Contains(c.remedy, "network") {
			t.Errorf("server-err remedy must not blame the network, got %q", c.remedy)
		}
		if !strings.Contains(c.remedy, "--diagnose") {
			t.Errorf("server-err remedy should point at support/--diagnose, got %q", c.remedy)
		}
		if r.status != doctor.StatusUnknown {
			t.Errorf("server-err → ready should be can't-check (Unknown), got %v", r.status)
		}
	})

	// Disconnected but the local cluster is healthy: Ready must NOT show a green
	// ✔ next to a Connected ✖ — training can't complete while disconnected
	// (Bugbot #365).
	t.Run("disconnected but cluster healthy → ready not a false check", func(t *testing.T) {
		c, r := summarizeDoctor(with(allOK, "Service Bus egress (requests-proxy)", doctor.StatusFail), tokenOK)
		if c.status != doctor.StatusFail {
			t.Fatalf("precondition: want connected Fail (service bus down), got %v", c.status)
		}
		if r.status == doctor.StatusOK {
			t.Errorf("ready must not be a green check while disconnected, got OK %q", r.text)
		}
	})

	// The "Backend egress (from this machine)" probe is indicative-not-definitive;
	// a miss must NOT contradict a successful WhoAmI by claiming the network is
	// down. With a healthy session it stays a --verbose diagnostic (Bugbot #365).
	t.Run("backend-egress miss + healthy session → connected stays OK", func(t *testing.T) {
		c, _ := summarizeDoctor(with(allOK, "Backend egress (from this machine)", doctor.StatusFail), tokenOK)
		if c.status != doctor.StatusOK {
			t.Errorf("indicative backend-egress miss + healthy WhoAmI → want connected OK, got %v %q", c.status, c.text)
		}
		if strings.Contains(c.text, "can't reach tracebloc from here") {
			t.Errorf("must not blame the network after a successful WhoAmI, got %q", c.text)
		}
	})
}

// ── doctorVerdict: the closing "everything looks good" / problem / partial call ──

func TestDoctorVerdict(t *testing.T) {
	ok, warn, fail, unknown := doctor.StatusOK, doctor.StatusWarn, doctor.StatusFail, doctor.StatusUnknown
	cases := []struct {
		name             string
		connected, ready doctor.Status
		want             doctor.Status
	}{
		{"both OK → everything good", ok, ok, ok},
		{"ready Fail → problem", ok, fail, fail},
		{"connected Fail → problem", fail, ok, fail},
		// The Bugbot case: connected but readiness couldn't be checked (RBAC →
		// Unknown). Not a hard failure, but NOT "everything looks good".
		{"connected + ready can't-check → partial", ok, unknown, unknown},
		// Not-connected already Fails via connected, regardless of ready=Unknown.
		{"disconnected + ready unknown → problem", fail, unknown, fail},
		// The second Bugbot case (#541): a Warn is a FOUND problem, so the
		// closing line must be the warn one — never the Unknown partial, whose
		// copy claims no problem was found and that checks couldn't finish.
		{"a warn that isn't Fail → warn, not partial", ok, warn, warn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doctorVerdict(tc.connected, tc.ready); got != tc.want {
				t.Errorf("doctorVerdict(%v,%v) = %v, want %v",
					tc.connected, tc.ready, got, tc.want)
			}
		})
	}
}
