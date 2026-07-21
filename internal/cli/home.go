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
//     concurrent, each carries its own short timeout, and homeDetectBudget caps
//     the RENDER — the wall-clock we'll wait — not the probe goroutines. A probe
//     that ignores its context (a kubeconfig exec-credential plugin like
//     `aws eks get-token` is the realistic offender) can outlive the render; the
//     buffered result channels just stop it from blocking or leaking, and we
//     render the softer/remembered state at the budget. A probe that errors or
//     times out degrades to the softer state; nothing here is ever fatal.
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
	"unicode"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

// doctorPath is the command the home screen + env-status lines tell the user to
// run to diagnose the environment. It's the top-level `doctor` (newDoctorCmd,
// registered on the root; `cluster doctor` stays working as a hidden alias), so
// the lines read "run <inv> doctor".
const doctorPath = "doctor"

// homeRule is the 30-column dim divider drawn under the greeting and above the
// sign-off — the same string top and bottom, matching the locked design.
const homeRule = "──────────────────────────────"

// Invoked binary names we recognize. `<inv>` in the examples echoes how the user
// actually called the CLI so copy-paste matches their muscle memory.
const (
	binTracebloc = "tracebloc"
	binTB        = "tb"
)

// Detection is bounded so bare `tracebloc` stays snappy even when the cluster or
// backend is unreachable. homeProbeTimeout caps each individual probe;
// homeDetectBudget caps the whole detection regardless of any single probe.
// These must clear a COLD round-trip: the heartbeat hits the backend and every
// `tb` run is a fresh process (fresh DNS+TCP+TLS, ~1s), so the old 1200ms cap
// made a healthy "· Online" flicker to "couldn't confirm" (cli#357) — don't
// lower them back. Fast path is unaffected: collectProbes returns once both report.
const (
	homeDetectBudget = 3500 * time.Millisecond
	homeProbeTimeout = 3000 * time.Millisecond
)

// gpuResource is the allocatable key NVIDIA's device plugin advertises; summed
// for the compute parenthetical.
const gpuResource = corev1.ResourceName("nvidia.com/gpu")

// homeState is which of the locked screens to render.
type homeState int

const (
	homeNotSignedIn homeState = iota // you're not signed in — minimal, sign in first
	homeOnline                       // signed in, environment live AND heartbeating
	homeRunning                      // signed in, live locally but not Online (heartbeat unconfirmed, or confirmed not-online)
	homeStarting                     // signed in, release present but its workload isn't Ready yet
	homeOffline                      // signed in, provisioned, but the environment isn't reachable from here
	homeNoEnv                        // signed in, no environment provisioned on this machine
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
	name       string // first name for the greeting (profile → email local-part → ""); see greetingName
	envName    string // secure environment name, when known
	compute    computeInfo
	hasCompute bool
	inv        string // command name to echo in examples: "tb" when a tb launcher is installed, else the invoked name
	fullMenu   bool   // render the full command menu (an environment exists here)
	// hasResources gates the `resources` row: shown only when a `resources`
	// command is actually registered on the root (root.go checks the tree), so
	// the menu never advertises a command that isn't wired (#237).
	hasResources bool
	// confirmedNotOnline distinguishes the two honest flavors of homeRunning:
	// true = tracebloc's backend POSITIVELY reported this client not-online
	// (offline/pending), false = we merely couldn't confirm either way. Same
	// state, different knowledge — the running line words each accurately.
	confirmedNotOnline bool
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
	signIn    func() (signedIn bool, email, firstName string)
	probeEnv  func(ctx context.Context) envProbe
	probeBeat func(ctx context.Context) heartbeatState
	// hasResources reports whether a `resources` command is registered on the
	// root — the gate for the home screen's `resources` row. The production
	// wiring (renderHomeScreen) checks the real command tree; tests drive both
	// sides.
	hasResources func() bool
	// rememberedClient reads the active client cached at provision time (no
	// network) and reports BOTH whether this machine is provisioned and its
	// display name. "provisioned" comes from the SAME signal the cluster probe's
	// ownership gate uses (a cached active-client namespace), so the home screen
	// and the probe never disagree about whether an environment exists. name is
	// the label we fall back on whenever the probe couldn't surface one (incl. the
	// budget-timeout path), so a provisioned machine degrades to a *named*
	// "offline", never to the "no environment / run the installer" lie.
	rememberedClient func() (provisioned bool, name string)
	invoked          func() string
	tbAvailable      func() bool
	budget           time.Duration
}

func defaultHomeDeps() homeDeps {
	return homeDeps{
		signIn:           realSignIn,
		probeEnv:         realProbeEnv,
		probeBeat:        realHeartbeat,
		rememberedClient: realRememberedClient,
		invoked:          invokedName,
		tbAvailable:      tbAliasAvailable,
		// Safe default: assume `resources` isn't wired. renderHomeScreen
		// overrides this from the real command tree; a test may too.
		hasResources: func() bool { return false },
		budget:       homeDetectBudget,
	}
}

// renderHomeScreen is the bare-`tracebloc` entry point: detect (bounded,
// best-effort) then render. Called from root.RunE. resourcesRegistered says
// whether a `resources` command is wired on the root (root.go inspects the
// tree) — the gate for the home screen's `resources` row, so it never lists a
// command that isn't there.
func renderHomeScreen(ctx context.Context, p *ui.Printer, resourcesRegistered bool) {
	d := defaultHomeDeps()
	d.hasResources = func() bool { return resourcesRegistered }
	renderHome(p, resolveHomeModel(ctx, d))
}

// resolveHomeModel runs the (best-effort, bounded) probes and assembles the
// homeModel. It never blocks longer than the budget and never returns an error:
// a slow or failing probe degrades to the softer state and the screen still
// renders.
func resolveHomeModel(ctx context.Context, d homeDeps) homeModel {
	// Examples echo `tb` — the canonical short launcher — whenever it's actually
	// installed beside the CLI (the normal installed state; the installer places
	// it), so the home screen teaches the short command directly. Fall back to the
	// name the user invoked only when no `tb` launcher exists (e.g. a bare
	// `go build`), so we never print a `tb` that wouldn't run. tbAvailable is a
	// local file check (no network), safe even on the instant logged-out path.
	inv := d.invoked()
	if d.tbAvailable() {
		inv = binTB
	}

	// Sign-in is a local config read (instant). When logged out we render the
	// minimal "sign in first" screen and skip all cluster/backend I/O entirely —
	// the common just-installed case returns immediately, and there's no
	// login-free credential path to honestly report the environment anyway.
	signedIn, email, firstName := d.signIn()
	if !signedIn {
		return homeModel{state: homeNotSignedIn, inv: inv}
	}

	// The active client cached at provision time (no network): whether this
	// machine is provisioned, and its display name. Both are read up front so
	// they're available even on the budget-timeout path below.
	provisioned, remembered := d.rememberedClient()

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

	env, beat := collectProbes(bctx, envCh, beatCh)

	// Whether the PROBE itself surfaced an environment name — the signal the
	// offline-vs-no-env classifier keys on (alongside `provisioned`). Captured
	// BEFORE the display-name override below: otherwise a remembered client label
	// left in config without a cached namespace would masquerade as a
	// probe-surfaced environment and flip the no-env installer path to Offline.
	probeNamedEnv := env.name != ""

	// The environment's display name is the remembered client name (e.g.
	// "acme-01") — the friendly, per-client identity provisioned on this machine.
	// Prefer it over whatever the probe surfaced, which is the Helm RELEASE name
	// (e.g. "tracebloc" — shared across installs, not user-facing): otherwise the
	// SAME environment reads as "tracebloc" Online/Starting but "acme-01" Offline,
	// since the offline paths surface no name and fall back to remembered. Keep
	// the probe's name only when nothing was remembered — a last-resort label for
	// a release present on a machine that never cached a client. This also keeps
	// the "provisioned ⇒ named offline" fallback (a degraded probe returns no
	// name) living in exactly one place.
	if remembered != "" {
		env.name = remembered
	}

	m := homeModel{
		email:        email,
		name:         greetingName(firstName, email),
		inv:          inv,
		envName:      env.name,
		compute:      env.compute,
		hasCompute:   env.hasCompute,
		hasResources: d.hasResources(),
	}

	switch env.local {
	case localLive:
		// Online demands BOTH live-locally AND a positively-confirmed heartbeat.
		// Anything short of beatOnline (not-online, or couldn't-confirm) is the
		// honest "· running" state, never a green Online — but the model records
		// WHICH kind of not-Online it is, so the running line can word a
		// backend-confirmed "not online" differently from a mere couldn't-confirm.
		if beat == beatOnline {
			m.state = homeOnline
		} else {
			m.state = homeRunning
			m.confirmedNotOnline = beat == beatNotOnline
		}
		m.fullMenu = true
	case localDegraded:
		// Release present but its workload isn't Ready — it's coming up, which is a
		// different story from "live but tracebloc hasn't heard from it" (heartbeat
		// was never consulted here). Its own honest line, still → doctor.
		m.state = homeStarting
		m.fullMenu = true
	case localNoRelease, localUnreachable:
		// Either the cluster couldn't be reached, or it was reached but doesn't
		// host this client's release (wrong kube-context, or the client runs on a
		// cluster this kubeconfig doesn't point at — the sibling data commands
		// explain this as "runs elsewhere"). Offline vs. "no environment": it's an
		// environment we just can't reach (offline) if EITHER this machine is
		// PROVISIONED — a cached active-client namespace, the same signal the
		// probe's ownership gate uses — OR the PROBE itself surfaced an environment
		// name. Adding the provisioned test (not name alone) is the fix for a
		// provisioned-but-unnamed profile that used to misread as "no environment /
		// run the installer"; keeping the probe-name test preserves the case where
		// the probe surfaced a name without one being cached. It must be the
		// probe's own name (probeNamedEnv), NOT the post-override env.name — a
		// remembered display label with no cached namespace is not evidence of a
		// reachable environment and must fall through to the installer path.
		if provisioned || probeNamedEnv {
			m.state = homeOffline
			m.fullMenu = true
		} else {
			m.state = homeNoEnv
		}
	}

	return m
}

// collectProbes receives both probe results, bounded by ctx (the detection
// budget). When the budget fires, a probe that finished JUST beforehand has its
// result sitting in the buffered channel — and select picks uniformly among
// ready cases, so without care the Done branch could win the race and DROP a
// completed answer in favor of the softer default (a live release rendering as
// offline/no-env). So on Done we drain, non-blocking, anything already
// buffered; only a probe that truly hasn't reported keeps its default
// (localUnreachable / beatUnknown — the honest "couldn't confirm" values).
func collectProbes(ctx context.Context, envCh <-chan envProbe, beatCh <-chan heartbeatState) (envProbe, heartbeatState) {
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
		case <-ctx.Done():
			// Budget spent — render with whatever arrived, softly. Honor results
			// that DID arrive (they're in the buffers), without blocking.
			if envCh != nil {
				select {
				case env = <-envCh:
				default:
				}
			}
			if beatCh != nil {
				select {
				case beat = <-beatCh:
				default:
				}
			}
			got = 2
		}
	}
	return env, beat
}

// realSignIn reports the signed-in state + cached email and first name from
// local config — no network. The cached identity is enough for the display;
// token validity, when it matters, surfaces through the heartbeat probe (which
// exercises the token).
func realSignIn() (bool, string, string) {
	cfg, err := config.Load()
	if err != nil || !cfg.SignedIn() {
		return false, "", ""
	}
	prof := cfg.Current()
	return true, prof.Email, prof.FirstName
}

// greetingNameMax caps the greeting name, counted in runes (not bytes). A
// candidate longer than this is dropped rather than stretch the locked
// single-line header; it's generous enough for real first names.
const greetingNameMax = 14

// greetingName derives the first name shown in the signed-in greeting. It
// prefers the profile's stored first name, then the email's local part, running
// BOTH through cleanGreetingToken so only a safe, single, short token is used;
// the email fallback is capitalized ("lukas@…" → "Lukas"). Anything that isn't a
// clean token — multi-word, an interior newline, control chars, digits,
// punctuation, or longer than greetingNameMax — yields "" so the greeting drops
// the name (and its comma) gracefully.
func greetingName(firstName, email string) string {
	if n := cleanGreetingToken(firstName); n != "" {
		return n
	}
	local, _, found := strings.Cut(email, "@")
	if !found {
		return ""
	}
	if n := cleanGreetingToken(local); n != "" {
		return capitalizeFirst(n)
	}
	return ""
}

// cleanGreetingToken trims outer whitespace and returns s only if it's a single
// clean token safe for the one-line header: a non-empty run of at most
// greetingNameMax letters and nothing else. Interior whitespace or newlines
// (which Para would split across lines), control characters, digits,
// punctuation, multi-word names, and over-long tokens all return "" so the
// caller omits the name.
func cleanGreetingToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	n := 0
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return ""
		}
		if n++; n > greetingNameMax {
			return ""
		}
	}
	return s
}

// capitalizeFirst upper-cases the first rune of s, leaving the rest untouched
// ("lukas" → "Lukas"). "" stays "" (never indexes an empty rune slice).
func capitalizeFirst(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// realRememberedClient reads the active client cached at `client create` time
// (RFC-0001 §7.3) — no network. provisioned is true when a client namespace is
// cached: the SAME signal bindActiveClientNamespace / the probe's ownership gate
// key on, so "is there an environment here?" is answered identically on the home
// screen and in the probe. name is the friendly display, falling back to the
// client ID so a provisioned-but-unnamed profile still renders a *named* offline
// rather than being misread as "no environment".
func realRememberedClient() (provisioned bool, name string) {
	cfg, err := config.Load()
	if err != nil {
		return false, ""
	}
	p := cfg.Current()
	name = p.ActiveClientName
	if name == "" {
		name = p.ActiveClientID
	}
	return p.ActiveClientNamespace != "", name
}

// realProbeEnv is the bounded cluster probe. It reuses the exact namespace
// binding + discovery the data/cluster commands use, so the home screen reports
// the very environment those commands would target. Best-effort throughout: any
// failure degrades to unreachable/no-release, never an error.
func realProbeEnv(ctx context.Context) envProbe {
	ctx, cancel := context.WithTimeout(ctx, homeProbeTimeout)
	defer cancel()

	// The name for a discovered release is set below; the unreachable / no-release
	// returns leave it empty and let resolveHomeModel fill the remembered name, so
	// the "provisioned ⇒ named offline" fallback lives in exactly one place.
	opts := cluster.KubeconfigOptions{}
	binding := bindActiveClientNamespace(&opts)
	// OWNERSHIP GATE: no active-client binding ⇒ nothing was ever provisioned
	// for this profile, so no release the kubeconfig can reach is honestly
	// YOURS. Without the binding, discovery would fall back to the kubeconfig's
	// default namespace and then the cluster-wide scan — either can surface an
	// UNRELATED client (a shared cluster, a colleague's install), which this
	// screen would then greet as "your secure environment". The data commands
	// run that scan behind a visible retarget note and an explicit user action;
	// a status screen has neither, and §7.5's rule (a miss must never silently
	// retarget to some other client) applies doubly here. Report no-release —
	// resolveHomeModel renders the honest no-env screen (or a named offline via
	// the remembered-name fallback) — and skip the cluster I/O entirely, which
	// also keeps the common unprovisioned re-entry instant.
	if !binding.applied {
		return envProbe{local: localNoRelease}
	}
	resolved, err := loadClusterFn(opts)
	if err != nil {
		return envProbe{local: localUnreachable}
	}
	// Bound every API call so an unreachable API server can't hang the home
	// screen (mirrors cluster.ClusterID's time-boxed best-effort read).
	resolved.RestConfig.Timeout = homeProbeTimeout
	cs, err := newClientsetFn(resolved)
	if err != nil {
		return envProbe{local: localUnreachable}
	}

	release, nsUsed, err := discoverRelease(ctx, nil, cs, resolved.Namespace, binding.allowScan())
	if err != nil {
		if errors.Is(err, cluster.ErrNoParentRelease) {
			// Cluster reachable, but this release isn't in the resolved context.
			// Provisioned ⇒ resolveHomeModel turns this into a named "offline".
			return envProbe{local: localNoRelease}
		}
		// A list/RBAC/connect failure: we couldn't confirm what's here. Treat it
		// as unreachable (→ offline if provisioned, else no-env).
		return envProbe{local: localUnreachable}
	}

	ep := envProbe{name: release.ReleaseName}
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
// table-testable. The layout is the locked design (cli#244): a two-blank-line
// header, the greeting, a dim rule, the two honest status axes, the command
// buckets, then a dim sign-off.
func renderHome(p *ui.Printer, m homeModel) {
	// ── header ── two blank lines above the greeting; below it a blank line, the
	// rule, then a blank line before the status block.
	p.Newline()
	p.Newline()
	p.Para(greeting(m))
	p.Newline()
	p.Hintf(homeRule)
	p.Newline()

	// ── status: two axes (you · the machine), never fused ──

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
	//
	// Env label. A provisioned machine can reach an offline (or degraded) state
	// with no cached display name — namespace known, name not — so render a clean
	// nameless line rather than an empty-quoted `Secure environment ""`.
	envLabel := "Secure environment"
	if m.envName != "" {
		envLabel = fmt.Sprintf("Secure environment %q", m.envName)
	}
	switch m.state {
	case homeOnline:
		p.CheckLine("%s · Online%s", envLabel, computeParen(m))
	case homeRunning:
		// Two honest flavors of running, split by what we actually KNOW.
		// Backend positively reported not-online (offline/pending): "hasn't
		// heard from it" is literally what that status means. Merely
		// couldn't-confirm (backend unreachable, timeout, no active client):
		// don't claim tracebloc's view — it may be heartbeating fine while only
		// our probe failed — say we couldn't confirm.
		if m.confirmedNotOnline {
			p.WarnLine("%s · running, but tracebloc hasn't heard from it — run %s",
				envLabel, p.Command(m.inv+" "+doctorPath))
		} else {
			p.WarnLine("%s · running — couldn't confirm it's connected to tracebloc — run %s",
				envLabel, p.Command(m.inv+" "+doctorPath))
		}
	case homeStarting:
		p.WarnLine("%s · starting up, not ready yet — run %s",
			envLabel, p.Command(m.inv+" "+doctorPath))
	case homeOffline:
		// One honest line for both causes: a stopped/unreachable cluster AND a
		// reachable cluster that doesn't host this release from the current
		// kube-context. "can't reach it from here" is true either way.
		p.CrossLine("%s · can't reach it from here — run %s",
			envLabel, p.Command(m.inv+" "+doctorPath))
	case homeNoEnv:
		p.WarnLine("No secure environment on this machine yet — run the installer to set one up.")
	}

	// ── command buckets ── two blank lines above EVERY section: renderBuckets
	// emits a Newline before each Section (whose own leading blank makes two), so
	// the status→first-section and inter-section gaps are identical.
	renderBuckets(p, m.inv, menuFor(m))

	// ── footer ── two blank lines between the last row and the tip; a trailing
	// blank so the sign-off stands alone above the prompt.
	p.Newline()
	p.Newline()
	if m.fullMenu {
		p.Hintf("Add --help to any command for the flags.")
	}
	p.Newline()
	p.Hintf(homeRule)
	p.Newline()
	p.Hintf("love from tracebloc 💚") // dim, like the supporting text
	p.Newline()
}

// greeting is the locked welcome line. States where a secure environment exists
// greet the user by name ("… for AI, Lukas 👋"), dropping the name gracefully
// when none could be derived; the not-signed-in and no-environment screens use
// the brand greeting ("Welcome to tracebloc — …") instead, since there's no
// environment here to call "yours".
func greeting(m homeModel) string {
	switch m.state {
	case homeOnline, homeRunning, homeStarting, homeOffline:
		if m.name != "" {
			return fmt.Sprintf("Welcome to your secure environment for AI, %s 👋", m.name)
		}
		return "Welcome to your secure environment for AI 👋"
	default:
		return "Welcome to tracebloc — your secure environment for AI 👋"
	}
}

// menuFor is the command buckets to show per state. Not-signed-in and no-env
// stay deliberately lean (one actionable step); the environment states get the
// full two-bucket menu. Offboarding (`delete`) lives under "Your secure
// environment"; the `resources` row appears only when that command is actually
// registered (m.hasResources) — see the field's note.
func menuFor(m homeModel) []menuBucket {
	switch m.state {
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
	default: // online / running / starting / offline — an environment exists here
		env := make([][2]string, 0, 3)
		if m.hasResources {
			env = append(env, [2]string{"resources", "manage compute & memory"})
		}
		env = append(env,
			[2]string{doctorPath, "check the connection & diagnose issues"},
			[2]string{"delete", "remove tracebloc from this machine"},
		)
		return []menuBucket{
			{
				title: "Your data",
				rows: [][2]string{
					{"data ingest", "load a dataset into your secure environment"},
					{"data list", "list your datasets"},
					{"data delete", "remove a dataset"},
				},
			},
			{title: "Your secure environment", rows: env},
		}
	}
}

// renderBuckets prints each bucket as a leading blank line, a Section title,
// another blank line, then one MenuRow per command ("· <inv> <cmd>
// <description>") — the leading blank plus Section's own give two blank lines
// above every section. The command column is padded to a single width across
// ALL buckets so the descriptions line up in one column (commands normal,
// descriptions dim).
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
		p.Newline() // two blank lines above each section (this + Section's own leading blank)
		p.Section(b.title)
		p.Newline() // blank line under the title
		for _, r := range b.rows {
			p.MenuRow(width, inv+" "+r[0], r[1])
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
