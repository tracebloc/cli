package cli

// The status-aware `tracebloc` home screen. A bare `tracebloc` (no subcommand)
// used to print a stateless command list; now it opens with where you actually
// stand — are you signed in, and is this machine's secure environment live —
// then the commands. See root.go's RunE for the wiring.
//
// Design constraints that shape this file:
//
//   - Honesty. Sign-in (you) and the secure environment (the machine) are two
//     SEPARATE lines and never fused: the client heartbeats with its own
//     credential, so the environment can be Online while you're logged out. A
//     green "· Online" is printed ONLY when the environment is live locally AND
//     positively confirmed heartbeating to tracebloc — a heartbeat we couldn't
//     confirm degrades to "· running", never a false green.
//
//   - Speed + robustness. Bare `tracebloc` runs constantly; the old screen did
//     zero I/O. Detection here is best-effort and bounded: every probe is
//     concurrent, each carries its own short timeout, and the whole thing is
//     capped by homeDetectBudget so an unreachable cluster/backend still renders
//     the right ⚠/✗ state fast. A probe that errors or times out degrades to the
//     softer state; nothing here is ever fatal.
//
// Structure: resolveHomeModel runs the probes and returns a pure homeModel;
// renderHome turns that model into output with no I/O. The split keeps rendering
// deterministic and table-testable, and lets the detection be driven by fakes.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

// doctorPath is the REAL command that diagnoses the environment on develop
// (`cluster doctor`, wired in cluster.go). Kept as a const so the home screen
// never invents a top-level `doctor` — a rename is a separate follow-up.
const doctorPath = "cluster doctor"

// Invoked binary names we recognize. `<inv>` in the examples echoes how the user
// actually called the CLI so copy-paste matches their muscle memory.
const (
	binTracebloc = "tracebloc"
	binTB        = "tb"
)

// Detection is bounded so bare `tracebloc` stays snappy even when the cluster or
// backend is unreachable. homeProbeTimeout caps each individual probe;
// homeDetectBudget caps the whole detection regardless of any single probe.
const (
	homeDetectBudget = 1500 * time.Millisecond
	homeProbeTimeout = 1200 * time.Millisecond
)

// gpuResource is the allocatable key NVIDIA's device plugin advertises; summed
// for the compute parenthetical.
const gpuResource = corev1.ResourceName("nvidia.com/gpu")

// homeState is which of the locked screens to render.
type homeState int

const (
	homeNotSignedIn homeState = iota // you're not signed in — minimal, sign in first
	homeOnline                       // signed in, environment live AND heartbeating
	homeRunning                      // signed in, environment live locally but heartbeat unconfirmed
	homeOffline                      // signed in, environment set up here but unreachable/stopped
	homeNoEnv                        // signed in, no environment on this machine
)

// computeInfo is the machine's total schedulable capacity, summed across Ready
// nodes' allocatable. Shown only in the Online state; best-effort (omitted, not
// errored, when it can't be read).
type computeInfo struct {
	CPU    int
	MemGiB int
	GPU    int
}

// homeModel is the fully-resolved, I/O-free input to renderHome. Building it
// (resolveHomeModel) does the probing; rendering it is pure.
type homeModel struct {
	state      homeState
	email      string // signed-in account, "" when unknown
	envName    string // secure environment name, when known
	compute    computeInfo
	hasCompute bool
	inv        string // invoked binary name: "tb" or "tracebloc"
	tbTip      bool   // show the "type tb instead" tip
	fullMenu   bool   // render the full command menu (an environment exists here)
}

// ── Detection ──

// heartbeatState is tracebloc's view of the client, read via the signed-in
// user's token (see realHeartbeat). "unknown" is the honest default whenever we
// can't positively confirm — logged out, backend unreachable, probe timed out —
// and it never renders as Online.
type heartbeatState int

const (
	beatUnknown heartbeatState = iota
	beatOnline
	beatNotOnline
)

// localState is what a bounded cluster probe found on THIS machine.
type localState int

const (
	localUnreachable localState = iota // couldn't reach a cluster (no kubeconfig / connect failed / timeout)
	localNoRelease                     // cluster reachable, but no tracebloc release installed
	localDegraded                      // release present, but its core workload isn't Ready
	localLive                          // release present and its core workload is Ready
)

// envProbe is the cluster probe's result: what's here, its name, and (when live)
// the machine's compute capacity.
type envProbe struct {
	local      localState
	name       string
	compute    computeInfo
	hasCompute bool
}

// homeDeps are the detection seams. defaultHomeDeps wires the real
// implementations; tests inject fakes to drive every state, the honesty
// fallback, and the timeout/degrade path without touching a real cluster.
type homeDeps struct {
	signIn      func() (signedIn bool, email string)
	probeEnv    func(ctx context.Context) envProbe
	probeBeat   func(ctx context.Context) heartbeatState
	invoked     func() string
	tbAvailable func() bool
	budget      time.Duration
}

func defaultHomeDeps() homeDeps {
	return homeDeps{
		signIn:      realSignIn,
		probeEnv:    realProbeEnv,
		probeBeat:   realHeartbeat,
		invoked:     invokedName,
		tbAvailable: tbAliasAvailable,
		budget:      homeDetectBudget,
	}
}

// renderHomeScreen is the bare-`tracebloc` entry point: detect (bounded,
// best-effort) then render. Called from root.RunE.
func renderHomeScreen(ctx context.Context, p *ui.Printer) {
	renderHome(p, resolveHomeModel(ctx, defaultHomeDeps()))
}

// resolveHomeModel runs the (best-effort, bounded) probes and assembles the
// homeModel. It never blocks longer than the budget and never returns an error:
// a slow or failing probe degrades to the softer state and the screen still
// renders.
func resolveHomeModel(ctx context.Context, d homeDeps) homeModel {
	inv := d.invoked()

	// Sign-in is a local config read (instant). When logged out we render the
	// minimal "sign in first" screen and skip all cluster/backend I/O entirely —
	// the common just-installed case returns immediately, and there's no
	// login-free credential path to honestly report the environment anyway.
	signedIn, email := d.signIn()
	if !signedIn {
		return homeModel{state: homeNotSignedIn, inv: inv}
	}

	// Signed in: probe the environment and the heartbeat concurrently, both under
	// one overall budget. Buffered result channels (cap 1) mean an abandoned probe
	// never blocks or leaks — if it finishes after we've given up on the budget,
	// its send lands in the buffer and its goroutine exits.
	bctx, cancel := context.WithTimeout(ctx, d.budget)
	defer cancel()

	envCh := make(chan envProbe, 1)
	beatCh := make(chan heartbeatState, 1)
	go func() { envCh <- d.probeEnv(bctx) }()
	go func() { beatCh <- d.probeBeat(bctx) }()

	env := envProbe{local: localUnreachable} // default if the probe never reports
	beat := beatUnknown                      // default: unconfirmed, never Online
	for got := 0; got < 2; {
		select {
		case env = <-envCh:
			envCh = nil // disable this case; a nil channel blocks forever in select
			got++
		case beat = <-beatCh:
			beatCh = nil
			got++
		case <-bctx.Done():
			got = 2 // budget spent — render with whatever arrived, softly
		}
	}

	m := homeModel{
		email:      email,
		inv:        inv,
		envName:    env.name,
		compute:    env.compute,
		hasCompute: env.hasCompute,
	}

	switch env.local {
	case localLive:
		// Online demands BOTH live-locally AND a positively-confirmed heartbeat.
		// Anything short of beatOnline (not-online, or couldn't-confirm) is the
		// honest "· running" state, never a green Online.
		if beat == beatOnline {
			m.state = homeOnline
		} else {
			m.state = homeRunning
		}
		m.fullMenu = true
	case localDegraded:
		// Workload present but not Ready → running, not Online.
		m.state = homeRunning
		m.fullMenu = true
	case localNoRelease:
		// Cluster reachable, nothing installed: there's genuinely no environment
		// here right now, even if config remembers a past one.
		m.state = homeNoEnv
		m.envName = ""
	case localUnreachable:
		// Couldn't reach a cluster. If this machine was provisioned (the probe
		// surfaced a remembered name) the environment exists but is unreachable →
		// offline; otherwise there's simply nothing here.
		if env.name != "" {
			m.state = homeOffline
			m.fullMenu = true
		} else {
			m.state = homeNoEnv
		}
	}

	// The tb tip only makes sense when we're showing a command menu and the user
	// typed the long name while a real `tb` alias is installed. Probe for the
	// alias lazily — only when it could actually be shown.
	if m.fullMenu && inv == binTracebloc {
		m.tbTip = d.tbAvailable()
	}
	return m
}

// realSignIn reports the signed-in state + cached email from local config — no
// network. The cached identity is enough for the display; token validity, when
// it matters, surfaces through the heartbeat probe (which exercises the token).
func realSignIn() (bool, string) {
	cfg, err := config.Load()
	if err != nil || !cfg.SignedIn() {
		return false, ""
	}
	return true, cfg.Current().Email
}

// realProbeEnv is the bounded cluster probe. It reuses the exact namespace
// binding + discovery the data/cluster commands use, so the home screen reports
// the very environment those commands would target. Best-effort throughout: any
// failure degrades to unreachable/no-release, never an error.
func realProbeEnv(ctx context.Context) envProbe {
	ctx, cancel := context.WithTimeout(ctx, homeProbeTimeout)
	defer cancel()

	// The name to fall back on when the cluster can't be reached but this machine
	// was provisioned — cached at `client create` time, so it needs no network.
	fallbackName := ""
	if cfg, err := config.Load(); err == nil {
		fallbackName = cfg.Current().ActiveClientName
	}

	opts := cluster.KubeconfigOptions{}
	binding := bindActiveClientNamespace(&opts)
	resolved, err := cluster.Load(opts)
	if err != nil {
		return envProbe{local: localUnreachable, name: fallbackName}
	}
	// Bound every API call so an unreachable API server can't hang the home
	// screen (mirrors cluster.ClusterID's time-boxed best-effort read).
	resolved.RestConfig.Timeout = homeProbeTimeout
	cs, err := cluster.NewClientset(resolved)
	if err != nil {
		return envProbe{local: localUnreachable, name: fallbackName}
	}

	release, nsUsed, err := discoverRelease(ctx, nil, cs, resolved.Namespace, binding.allowScan())
	if err != nil {
		if errors.Is(err, cluster.ErrNoParentRelease) {
			return envProbe{local: localNoRelease}
		}
		// A list/RBAC/connect failure: we couldn't confirm what's here. Treat it
		// as unreachable (→ offline if provisioned, else no-env).
		return envProbe{local: localUnreachable, name: fallbackName}
	}

	name := release.ReleaseName
	if name == "" {
		name = fallbackName
	}
	ep := envProbe{name: name}
	if jobsManagerReady(ctx, cs, nsUsed, release) {
		ep.local = localLive
	} else {
		ep.local = localDegraded
	}
	// Compute is only surfaced on the Online line, and only worth reading when the
	// environment is actually up.
	if ep.local == localLive {
		if c, ok := machineCapacity(ctx, cs); ok {
			ep.compute, ep.hasCompute = c, true
		}
	}
	return ep
}

// realHeartbeat reports tracebloc's view of this machine's client — the honest
// "is it heartbeating" signal.
//
// CREDENTIAL NOTE: this reads the status via the signed-in USER token
// (lookupClientStatus → client.ListClients, keyed on the active client id). The
// CLI has no login-free path that authenticates as the in-cluster client itself
// (the chart's CLIENT_ID secret is readable, but nothing here presents it to the
// backend as a credential). So the heartbeat can only be confirmed while signed
// in; logged out we don't reach here (the not-signed-in screen omits the
// environment line). Any failure — no active client, backend unreachable,
// timeout — is beatUnknown, which never renders as Online.
func realHeartbeat(ctx context.Context) heartbeatState {
	ctx, cancel := context.WithTimeout(ctx, homeProbeTimeout)
	defer cancel()

	client, cfg, err := authedClient()
	if err != nil {
		return beatUnknown
	}
	active := cfg.Current().ActiveClientID
	if active == "" {
		return beatUnknown
	}
	st, found, err := lookupClientStatus(ctx, client, active)
	if err != nil || !found {
		return beatUnknown
	}
	if st == clientStatusOnline {
		return beatOnline
	}
	return beatNotOnline
}

// jobsManagerReady reports whether the release's core workload (jobs-manager) has
// a Ready replica — the single best local "the environment is actually up"
// signal. Best-effort: an unreadable deployment reads as not-ready.
func jobsManagerReady(ctx context.Context, cs kubernetes.Interface, ns string, release *cluster.ParentRelease) bool {
	var names []string
	if release != nil && release.ReleaseName != "" {
		names = append(names, release.ReleaseName+"-jobs-manager")
	}
	names = append(names, "jobs-manager") // older unprefixed charts
	for _, n := range names {
		if d, err := cs.AppsV1().Deployments(ns).Get(ctx, n, metav1.GetOptions{}); err == nil {
			return d.Status.ReadyReplicas >= 1
		}
	}
	return false
}

// machineCapacity lists nodes and sums the Ready ones' allocatable into the
// machine's total schedulable capacity. Best-effort: a list failure or an
// all-NotReady cluster returns ok=false so the caller omits the parenthetical.
func machineCapacity(ctx context.Context, cs kubernetes.Interface) (computeInfo, bool) {
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return computeInfo{}, false
	}
	return sumCapacity(nodes.Items)
}

// sumCapacity is the pure summation behind machineCapacity — split out so the
// rounding + GPU logic is unit-testable without a clientset.
func sumCapacity(nodes []corev1.Node) (computeInfo, bool) {
	var cpuMilli, memBytes, gpu int64
	ready := 0
	for i := range nodes {
		n := nodes[i]
		if !nodeReady(n) {
			continue
		}
		ready++
		alloc := n.Status.Allocatable
		cpuMilli += alloc.Cpu().MilliValue()
		memBytes += alloc.Memory().Value()
		if q, ok := alloc[gpuResource]; ok {
			gpu += q.Value()
		}
	}
	if ready == 0 {
		return computeInfo{}, false
	}
	return computeInfo{
		CPU:    int(math.Round(float64(cpuMilli) / 1000)),
		MemGiB: int(math.Round(float64(memBytes) / (1 << 30))),
		GPU:    int(gpu),
	}, true
}

// nodeReady reports whether a node's Ready condition is True (mirrors
// doctor.nodeReady, which is unexported).
func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// invokedName is the binary name the user actually typed, so examples echo it.
func invokedName() string { return sanitizeInvoked(os.Args[0]) }

// sanitizeInvoked maps argv[0] to "tb" or "tracebloc": "tb" only when the user
// invoked the alias, otherwise "tracebloc" (covering odd argv[0]s like a `go run`
// temp name so the examples stay copy-pasteable).
func sanitizeInvoked(argv0 string) string {
	base := strings.TrimSuffix(filepath.Base(argv0), ".exe")
	if base == binTB {
		return binTB
	}
	return binTracebloc
}

// tbAliasAvailable reports whether a real tracebloc-owned `tb` alias sits next to
// this binary (the installer symlinks it there, cli#142). Reuses delete.go's
// aliasStatus so "is it ours" is judged exactly as offboarding judges it — we
// only advertise `tb` when it genuinely points at this CLI.
func tbAliasAvailable() bool {
	exe, err := osExecutable()
	if err != nil {
		return false
	}
	tb := filepath.Join(filepath.Dir(exe), binTB)
	if tb == exe {
		return false
	}
	_, ours := aliasStatus(tb, exe)
	return ours
}

// ── Rendering (pure) ──

// menuBucket is a titled group of command rows ({suffix, description}).
type menuBucket struct {
	title string
	rows  [][2]string
}

// renderHome writes the status-aware home screen. Pure: no I/O, no clock — it's
// a deterministic function of the model, which is what makes every state
// table-testable.
func renderHome(p *ui.Printer, m homeModel) {
	p.Newline()

	// Greeting: "your secure environment" only when one actually exists here.
	switch m.state {
	case homeOnline, homeRunning, homeOffline:
		p.Para("Welcome to your secure environment for AI 👋")
	default:
		p.Para("Welcome to tracebloc — your secure environment for AI 👋")
	}

	// Sign-in axis (you).
	if m.state == homeNotSignedIn {
		p.CrossLine("Not signed in yet.")
	} else if m.email != "" {
		p.CheckLine("Signed in as %s", m.email)
	} else {
		p.CheckLine("Signed in")
	}

	// Secure-environment axis (the machine) — a separate line, never fused with
	// sign-in.
	switch m.state {
	case homeOnline:
		p.CheckLine("Secure environment %q · Online%s", m.envName, computeParen(m))
	case homeRunning:
		p.WarnLine("Secure environment %q · running, but tracebloc hasn't heard from it — run %s %s",
			m.envName, m.inv, doctorPath)
	case homeOffline:
		p.CrossLine("Secure environment %q · offline — run %s %s", m.envName, m.inv, doctorPath)
	case homeNoEnv:
		p.WarnLine("No secure environment on this machine yet — run the installer to set one up.")
	}

	renderBuckets(p, m.inv, menuFor(m.state))

	// Closing.
	p.Newline()
	if m.tbTip {
		p.Hintf("tip · type  tb  instead of  tracebloc — either works")
	}
	if m.fullMenu {
		p.Hintf("Add --help to any command for the flags.")
	}
	p.Newline()
	p.Hintf("──────────────────────────────")
	p.Para("love from tracebloc 💚")
}

// menuFor is the command buckets to show per state. Not-signed-in and no-env
// stay deliberately lean (one actionable step); the environment states get the
// full menu.
func menuFor(state homeState) []menuBucket {
	switch state {
	case homeNotSignedIn:
		return []menuBucket{{
			title: "Start here",
			rows:  [][2]string{{"login", "sign in to tracebloc"}},
		}}
	case homeNoEnv:
		// The actionable next step is the installer (external); still point at
		// doctor to diagnose a half-installed environment.
		return []menuBucket{{
			title: "Your secure environment",
			rows:  [][2]string{{doctorPath, "check the connection & diagnose issues"}},
		}}
	default: // online / running / offline — an environment exists here
		return []menuBucket{
			{
				title: "Work with your data",
				rows: [][2]string{
					{"data ingest", "load a dataset into your secure environment"},
					{"data list", "list your datasets"},
					{"data delete", "remove a dataset"},
				},
			},
			{
				title: "Your secure environment",
				rows:  [][2]string{{doctorPath, "check the connection & diagnose issues"}},
			},
			{
				title: "Manage",
				rows:  [][2]string{{"delete", "remove tracebloc from this machine"}},
			},
		}
	}
}

// renderBuckets prints each bucket as a Section + "· <inv> <cmd>  <description>"
// rows, with the command column padded to a single width across ALL buckets so
// the descriptions line up in one column.
func renderBuckets(p *ui.Printer, inv string, buckets []menuBucket) {
	width := 0
	for _, b := range buckets {
		for _, r := range b.rows {
			if l := len(inv) + 1 + len(r[0]); l > width {
				width = l
			}
		}
	}
	for _, b := range buckets {
		p.Section(b.title)
		for _, r := range b.rows {
			p.Infof("%-*s  %s", width, inv+" "+r[0], r[1])
		}
	}
}

// computeParen formats the compute parenthetical, e.g. " (8 CPU · 32 GiB · 1 GPU)".
// GPU is dropped when 0; the whole thing is empty when capacity couldn't be read.
func computeParen(m homeModel) string {
	if !m.hasCompute {
		return ""
	}
	s := fmt.Sprintf(" (%d CPU · %d GiB", m.compute.CPU, m.compute.MemGiB)
	if m.compute.GPU > 0 {
		s += fmt.Sprintf(" · %d GPU", m.compute.GPU)
	}
	return s + ")"
}
