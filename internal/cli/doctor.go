package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/doctor"
	"github.com/tracebloc/cli/internal/installer"
	"github.com/tracebloc/cli/internal/ui"
)

// doctorRunFn is a test seam over doctor.Run (the cluster-side probe). Tests
// inject a fixed []doctor.Result so the roll-up + render can be exercised with a
// controlled mix without standing up a fake cluster.
var doctorRunFn = doctor.Run

// newDoctorCmd builds `doctor`, registered as top-level `tracebloc doctor` and
// (hidden) `tracebloc cluster doctor`. It answers, in plain language: is my
// secure environment connected to tracebloc and ready to run training — and if
// not, exactly what do I do. The Kubernetes detail lives behind --verbose;
// --diagnose writes a redacted bundle for support.
func newDoctorCmd(hidden bool) *cobra.Command {
	var (
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
		diagnose        bool
	)

	cmd := &cobra.Command{
		Use:         "doctor",
		Annotations: runtimeClassFor(classBackendCluster),
		Hidden:      hidden,
		Short:       "Check your secure environment is connected and ready to run training",
		Long: `Checks, in plain terms, whether your secure environment is connected to
tracebloc and ready to run training — and if something's wrong, exactly what to
do about it.

  --verbose    the full technical breakdown (for support)
  --diagnose   write a redacted support bundle to email to tracebloc

Exit codes:
  0   healthy
  2   a problem was found
  3   couldn't read your local config`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterDoctor(
				cmd.Context(),
				printerFor(cmd),
				kubeconfigPath, contextOverride, nsOverride, diagnose,
			)
		},
	}

	addKubeconfigFlags(cmd, &kubeconfigPath, &contextOverride, kubeconfigFlagUsage, contextFlagUsage)
	addNamespaceFlag(cmd, &nsOverride,
		"namespace where your secure environment is installed (default: your active client's)")
	cmd.Flags().BoolVar(&diagnose, "diagnose", false,
		"write a redacted support bundle for tracebloc support and exit")

	return cmd
}

func runClusterDoctor(
	ctx context.Context,
	p *ui.Printer,
	kubeconfigPath, contextOverride, nsOverride string,
	diagnose bool,
) (rerr error) {
	// 1. Who you are — a local config read (instant), so we can answer even
	//    before any cluster is reachable (RFC-0001 §8.5).
	cfg, err := config.Load()
	if err != nil {
		p.Errorf("Couldn't read your tracebloc config — run `%s login` to recreate it.", launcher())
		return &exitError{code: exitLocalEnv, err: nil}
	}
	if !cfg.SignedIn() {
		p.Errorf("Not signed in — run `%s login`.", launcher())
		return &exitError{code: exitChecksFailed, err: nil}
	}
	if email := cfg.Current().Email; email != "" {
		p.Para("Signed in as " + email)
	} else {
		p.Para("Signed in")
	}

	// State we accumulate so `--diagnose` can leave a bundle from wherever we
	// exit. tok is declared here because the session probe below sets it.
	var (
		tok              = tokenOK
		resolved         *cluster.ResolvedConfig
		results          []doctor.Result
		connected, ready healthLine
	)

	// 2. Is the session still good with tracebloc? A 401 (expired/revoked) or a
	//    426 (CLI too old) is a hard stop with a one-command fix. Anything else
	//    folds into the Connected line below — but a backend that ANSWERED with
	//    an error (5xx/403/decode) is a tracebloc-side problem, distinct from a
	//    network failure to reach it at all. Conflating the two would blame the
	//    user's network (and hand them a proxy remedy) for tracebloc's own error.
	// knownSessionEnv, not cfg.CurrentEnv: the session probe must target the same
	// host authedClient would, or `doctor` reports on a backend no other command
	// talks to. Reading CurrentEnv directly drops the $CLIENT_ENV fallback and the
	// normalisation — a second resolution of the same question. It also FAILS CLOSED
	// on an unrecognised env: probing prod with this token when the user believes
	// another backend is the backend#2171 leak, so surface the misconfiguration with
	// a one-command fix instead of dialling prod.
	env, envErr := knownSessionEnv(cfg)
	if envErr != nil {
		p.Newline()
		p.Errorf("%v", envErr)
		return &exitError{code: exitChecksFailed, err: nil}
	}
	apiClient := newAPIClient(env)
	apiClient.Token = cfg.Current().Token
	if _, werr := apiClient.WhoAmI(ctx); werr != nil {
		var ae *api.APIError
		var ue *api.UpgradeRequiredError
		switch {
		case errors.As(werr, &ae) && ae.StatusCode == http.StatusUnauthorized:
			p.Newline()
			p.Errorf("Your session expired — run `%s login`.", launcher())
			return &exitError{code: exitChecksFailed, err: nil}
		case errors.As(werr, &ue):
			p.Newline()
			p.Errorf("This CLI is out of date — update it: %s", installer.Cmd)
			return &exitError{code: exitChecksFailed, err: nil}
		case errors.As(werr, &ae):
			tok = tokenServerErr // tracebloc answered, just not with 200
		default:
			tok = tokenUnreachable // couldn't reach tracebloc at all (network/proxy)
		}
	}

	// Register the --diagnose bundle writer now that the session outcome is known.
	// Placed AFTER the 401/426 hard stops (which return above): a bundle is
	// pointless for those one-command fixes (login / update), and — crucially —
	// writing one there would record "session: confirmed" for an expired or
	// upgrade-required session, misleading triage. For every path past here tok is
	// accurate, so `--diagnose` always leaves a truthful bundle (even on the
	// no-environment / clientset-error exits, exactly where a remedy sends the
	// user). The write only sets the exit code when nothing worse already did, so
	// a bundle hiccup never masks a real Fail verdict.
	if diagnose {
		defer func() {
			p.Newline()
			if werr := writeDiagnoseBundle(p, resolved, results, tok, connected, ready); werr != nil && rerr == nil {
				rerr = werr
			}
		}()
	}

	// 3. Find the secure environment (local kubeconfig read). If it isn't here,
	//    that's the headline — but surface a session/backend fault we already
	//    found first, so a reinstall is never recommended while a live session
	//    problem goes unexplained. (A 401/426 is a hard stop earlier; only the
	//    soft tokenUnreachable/tokenServerErr states reach here.)
	opts := cluster.KubeconfigOptions{Path: kubeconfigPath, Context: contextOverride, Namespace: nsOverride}
	binding := bindActiveClientNamespace(&opts)
	resolved, err = loadClusterFn(opts)
	if err != nil {
		p.Newline()
		noteSessionProblem(p, tok)
		p.Errorf("No secure environment on this machine yet.")
		p.Hintf("     Set one up: %s", installer.Cmd)
		return &exitError{code: earlyExitCode(tok), err: nil}
	}
	cs, err := newClientsetFn(resolved)
	if err != nil {
		p.Newline()
		noteSessionProblem(p, tok)
		p.Errorf("Couldn't connect to your secure environment — check your kubeconfig/context.")
		renderDetailsIfVerbose(p, resolved, results)
		return &exitError{code: earlyExitCode(tok), err: nil}
	}
	// 4. Probe the cluster.
	results = doctorRunFn(ctx, cs, doctor.Options{Namespace: resolved.Namespace, ServerURL: resolved.ServerURL})

	// #515: a namespace we CHOSE for the user can be wrong, and a miss on it is
	// not evidence that this machine has no environment — yet doctor's only
	// reading of "no chart here" is "no secure environment on this machine yet",
	// which then recommends reinstalling over a healthy install. Extend #401's
	// local fallback to the wrong-pointer case: re-probe the namespace the
	// KUBECONFIG selects, but only when the binding (not the user) picked the
	// namespace that missed, and only on a LOCAL cluster — on a remote/shared one
	// the ownership gate stands and we would risk naming a colleague's client.
	// The retry is adopted only if it actually finds an environment, so a genuine
	// no-environment machine keeps the original results (and the --diagnose
	// bundle keeps describing the namespace the user is configured for).
	// pointerStale records that the re-probe SUCCEEDED — i.e. this machine has a
	// healthy environment AND its active-client pointer is wrong. Both halves
	// have to be said; see the verdict block below for why finding the
	// environment is not on its own good news.
	pointerStale := false
	if binding.applied && reachStateOf(results) == doctor.ReachNoEnv {
		if ns, ok := localEnvNamespace(cluster.KubeconfigOptions{Path: kubeconfigPath, Context: contextOverride}); ok && ns != resolved.Namespace {
			// Adopt the re-probe ONLY on a positive confirmation that a client is
			// there. `!= ReachNoEnv` was wrong (Bugbot): ReachUnreachable and
			// ReachError also satisfy it, and both mean "we could not tell" — so a
			// stale pointer plus RBAC or a transient read on the context namespace
			// would have named an unconfirmed namespace as a secure environment and
			// told the user this cluster already runs a client, pushing `client
			// create` into a MINT. Same absence-as-presence collapse surveyCluster
			// and residencyOf exist to avoid; an unconfirmed re-probe keeps the
			// original results and the honest no-environment path.
			if retry := doctorRunFn(ctx, cs, doctor.Options{Namespace: ns, ServerURL: resolved.ServerURL}); reachConfirmedOK(retry) {
				resolved.Namespace, results, pointerStale = ns, retry, true
			}
		}
	}

	// A reachable cluster with no tracebloc chart installed is the same "no secure
	// environment here" state as a missing kubeconfig — route it through the same
	// message (which also surfaces any session fault) rather than naming an
	// environment that isn't installed. This unifies all three no-environment
	// exits: missing kubeconfig, clientset error, and no chart.
	if reachStateOf(results) == doctor.ReachNoEnv {
		p.Newline()
		noteSessionProblem(p, tok)
		p.Errorf("No secure environment on this machine yet.")
		p.Hintf("     Set one up: %s", installer.Cmd)
		renderDetailsIfVerbose(p, resolved, results)
		return &exitError{code: earlyExitCode(tok), err: nil}
	}

	// An environment is installed here — name it (nothing prints between this and
	// "Signed in" above, so the two context lines read as a pair), then roll up.
	p.Para(fmt.Sprintf("Secure environment %q", envDisplayName(resolved)))
	connected, ready = summarizeDoctor(results, tok)
	if pointerStale {
		// The environment above is healthy — but `data ingest`, `data list`,
		// `resources` and `seal` all bind the active-client POINTER, and that
		// still misses, so they keep failing with exit 4. "Ready to run training"
		// is therefore false no matter how green the cluster checks are. Replace
		// the readiness line rather than printing a green tick with a warning
		// beside it that contradicts it — and the replacement carries the remedy,
		// so the finding and the fix read as one thing. The support bundle
		// records this line too, which is what triage needs to see.
		ready = healthLine{
			status: doctor.StatusFail,
			text: fmt.Sprintf("Not ready — your active client points at namespace %q, which isn't on this cluster, so data commands will keep failing until you repoint.",
				binding.namespace),
			remedy: fmt.Sprintf("Point this machine at the environment above: %s client create  (this cluster already runs a client, so it adopts it — no new credential)",
				launcher()),
		}
	}

	p.Newline()
	renderHealth(p, connected)
	renderHealth(p, ready)
	renderDetailsIfVerbose(p, resolved, results)
	// --diagnose writes the support bundle via the deferred writer registered
	// above, so it fires on this path and on every early exit alike.

	// 6. Verdict + exit code (0 healthy/partial, 2 a problem).
	p.Newline()
	verdict := doctorVerdict(connected.status, ready.status)
	switch {
	case pointerStale:
		// A problem WAS found — it just isn't in the cluster. Exit 2 (the code
		// doctor already uses for every actionable finding), never 0 with
		// "you're ready to run training": the very next `data ingest` exits 4,
		// and a doctor that greens that is reporting success it hasn't earned
		// — the class BUGBOT.md flags first. The remedy is already printed
		// above, so this doesn't also send them to write a support bundle.
		return &exitError{code: exitChecksFailed, err: nil}
	case verdict == doctor.StatusFail:
		if !diagnose { // they just wrote a bundle — don't send them to write it again
			p.Hintf("Still stuck? Email support@tracebloc.io with the output of `%s doctor --diagnose`.", launcher())
		}
		return &exitError{code: exitChecksFailed, err: nil}
	case verdict == doctor.StatusOK:
		p.Successf("Everything looks good — you're ready to run training.")
	case verdict == doctor.StatusWarn:
		// A ⚠ readiness printed a real problem (with its remedy) one line up.
		// This must NOT fall through to the "No problems found, but some checks
		// couldn't finish" line: a problem WAS found and every check DID finish
		// — both halves would be false (Bugbot). Training still runs → exit 0.
		p.Warnf("Training will run, but doctor found a problem — the ⚠ above says how to fix it.")
	default:
		// Connected and nothing failed, but a check couldn't complete (e.g. pod
		// health unreadable — RBAC → ready is StatusUnknown). Don't overclaim
		// "everything looks good"; say so honestly. Not a failure → exit 0 (Bugbot).
		p.Infof("No problems found, but some checks couldn't finish — re-run with --verbose for detail.")
	}
	return nil
}

// healthLine is one rolled-up, user-facing health signal: a status, the plain
// line to show, and (on failure) one concrete action.
type healthLine struct {
	status doctor.Status
	text   string
	remedy string
}

// tokenState classifies the WhoAmI probe for the Connected rollup: the session
// confirmed (tokenOK), the backend unreachable from this machine (tokenUnreachable
// — network/proxy), or the backend reachable but answering with an error
// (tokenServerErr — 5xx/403/decode, a tracebloc-side problem, not the network).
type tokenState int

const (
	tokenOK tokenState = iota
	tokenUnreachable
	tokenServerErr
)

// noteSessionProblem prints a soft WhoAmI/session fault (unreachable backend or a
// server-side error) as a standalone ✖ + fix. Used on the no-local-environment
// path so a fault detected before the kubeconfig read isn't silently dropped when
// that read fails. It's a no-op when the session is fine (tokenOK).
func noteSessionProblem(p *ui.Printer, tok tokenState) {
	switch tok {
	case tokenUnreachable:
		p.Errorf("Can't reach tracebloc from here.")
		p.Hintf("     Check your network / HTTP(S)_PROXY, then run `%s doctor` again.", launcher())
	case tokenServerErr:
		p.Errorf("tracebloc didn't confirm your session (server error).")
		p.Hintf("     Try again shortly; if it persists, email support@tracebloc.io with `%s doctor --diagnose`.", launcher())
	}
}

// earlyExitCode picks the exit code for a local-environment early exit (no
// kubeconfig / clientset error). A connectivity or session fault we already
// detected (tok != tokenOK) is a checks failure and takes precedence over the
// local-env code, so those states exit 2 here just as they do on the full probe
// path — a script keying on "exit 2 = a problem was found" never misses a
// session fault just because local config also failed.
func earlyExitCode(tok tokenState) int {
	if tok != tokenOK {
		return exitChecksFailed
	}
	return exitLocalEnv
}

// tokenLabel is the one-line session status recorded in the support bundle.
func tokenLabel(tok tokenState) string {
	switch tok {
	case tokenUnreachable:
		return "backend unreachable from this machine (network/proxy)"
	case tokenServerErr:
		return "backend answered with an error (5xx/403/decode)"
	default:
		return "confirmed"
	}
}

// summarizeDoctor collapses the granular checks into the two lines the owner
// reads: "Connected to tracebloc" and "Ready to run training". Each expands to
// the specific plain-language problem + fix on failure; the Kubernetes detail
// stays in --verbose. When the owner isn't connected — for ANY reason — a
// healthy local cluster still can't run training, so readiness degrades honestly
// to "can't check" rather than showing a reassuring ✔ next to a Connected ✖.
// computeRemedy is the "not enough compute" remedy for the host OS (#400). On
// Windows the default Docker backend is WSL2, where Docker Desktop has NO
// Resources→Memory slider — the VM's memory comes from %UserProfile%\.wslconfig.
// macOS keeps the slider; bare Linux has no Docker Desktop at all. Every
// variant ends with the drift fix: size runs to the machine (backend#1236's
// install-time auto-size can go stale when a machine shrinks).
func computeRemedy(goos string) string {
	resize := fmt.Sprintf("size runs to this machine with `%s resources set max`", launcher())
	switch goos {
	case "windows":
		return fmt.Sprintf("Free some up, give Docker more memory (WSL2 backend: `[wsl2] memory=…` in %%UserProfile%%\\.wslconfig then `wsl --shutdown`; Hyper-V backend: Docker Desktop → Settings → Resources → Advanced), or %s.", resize)
	case "darwin":
		return fmt.Sprintf("Free some up, raise the machine's allocation in Docker Desktop → Settings → Resources → Advanced, or %s.", resize)
	default:
		return fmt.Sprintf("Free some up on this machine, or %s.", resize)
	}
}

func summarizeDoctor(results []doctor.Result, tok tokenState) (connected, ready healthLine) {
	by := make(map[string]doctor.Result, len(results))
	for _, r := range results {
		by[r.Name] = r
	}
	reach := by["Cluster reachable"]

	switch {
	case reach.Status == doctor.StatusFail:
		// ReachNoEnv (reachable, no chart) is short-circuited in runClusterDoctor,
		// so it never reaches here. The two remaining fails are worded from the
		// classification, never a kubectl; an unclassified fail (e.g. a hand-built
		// result) defaults to "isn't answering" — the safe interpretation.
		switch reach.Reach {
		case doctor.ReachError:
			connected = healthLine{doctor.StatusFail,
				"Not connected — couldn't read your secure environment.",
				fmt.Sprintf("Email support@tracebloc.io with the output of `%s doctor --diagnose`.", launcher())}
		default: // ReachUnreachable
			connected = healthLine{doctor.StatusFail,
				"Not connected — your secure environment isn't answering.",
				reach.Remedy}
		}
	case tok == tokenUnreachable:
		// WhoAmI is the definitive "can this machine reach tracebloc" signal, so it
		// alone drives this line. The "Backend egress (from this machine)" check is
		// explicitly indicative-not-definitive (it probes the cluster's configured
		// host from here, not the cluster's real egress path), so it must NOT flip
		// Connected to a network error after WhoAmI already succeeded — that would
		// contradict the successful session probe. It stays a --verbose diagnostic.
		connected = healthLine{doctor.StatusFail,
			"Not connected — can't reach tracebloc from here.",
			fmt.Sprintf("Check your network / HTTP(S)_PROXY, then run `%s doctor` again.", launcher())}
	case tok == tokenServerErr:
		connected = healthLine{doctor.StatusFail,
			"Not connected — tracebloc didn't confirm your session (server error).",
			fmt.Sprintf("Try again shortly; if it persists, email support@tracebloc.io with `%s doctor --diagnose`.", launcher())}
	case by["Service Bus egress (requests-proxy)"].Status == doctor.StatusFail:
		connected = healthLine{doctor.StatusFail,
			"Training results can't reach tracebloc — experiments will stall.",
			fmt.Sprintf("Email support@tracebloc.io with the output of `%s doctor --diagnose`.", launcher())}
	default:
		connected = healthLine{doctor.StatusOK, "Connected to tracebloc", ""}
	}

	// Readiness only means something once we're connected: a healthy local
	// cluster still can't run training while it can't reach tracebloc. If
	// Connected is anything but OK, degrade honestly — never a green ✔ under a ✖.
	if connected.status != doctor.StatusOK {
		ready = healthLine{doctor.StatusUnknown, "Ready to run training — can't check yet", ""}
		return connected, ready
	}
	// The arms are ordered by SEVERITY, not by check: every Fail first, then the
	// one Warn, then the can't-check Unknowns, then OK. A switch stops at the
	// first match, so a can't-check arm placed above a real finding would SHADOW
	// it — ready would report StatusUnknown and doctorVerdict would print "No
	// problems found, but some checks couldn't finish" over a Fail or Warn that
	// was actually detected (Bugbot on #561/#566: "Warn rollup shadowed by
	// Unknown", backend#2438). StatusUnknown carries no signal, so it must be the
	// last thing consulted — it may only win when nothing above it fired.
	switch {
	// ── Fail: a real, training-blocking problem (worst wins) ──
	case by["Pod health"].Status == doctor.StatusFail:
		ready = healthLine{doctor.StatusFail,
			"Not ready — part of your secure environment isn't running.",
			fmt.Sprintf("Reinstall with `%s`, or email support@tracebloc.io with `%s doctor --diagnose`.", installer.Cmd, launcher())}
	case by["Pod health"].Status == doctor.StatusWarn && !strings.HasPrefix(by["Pod health"].Detail, "could not list pods"):
		// Pods stuck Pending past the grace window (unschedulable / image can't
		// pull) mean training can't actually schedule — so this is NOT ready, even
		// though the granular Pod-health check rates it a softer ⚠. Without this,
		// a stuck-pending environment rolled up to ✔ "Ready to run training" and
		// the "Everything looks good" verdict (Bugbot). The negated guard excludes
		// the "could not list pods" can't-check, which is handled in the Unknown
		// tier below now that this arm no longer relies on sitting after it.
		ready = healthLine{doctor.StatusFail,
			"Not ready — part of your secure environment can't start yet.",
			fmt.Sprintf("Some pods are stuck starting — usually not enough free compute, or a training image that can't be pulled. %s Then re-run `%s doctor`; if it persists, email support@tracebloc.io with `%s doctor --diagnose`.", computeRemedy(runtime.GOOS), launcher(), launcher())}
	case by["Image pull secret"].Status == doctor.StatusFail:
		ready = healthLine{doctor.StatusFail,
			"Not ready — the training images can't be pulled.",
			fmt.Sprintf("Email support@tracebloc.io with the output of `%s doctor --diagnose`.", launcher())}
	case by["Dataset volume (PVC)"].Status == doctor.StatusFail:
		ready = healthLine{doctor.StatusFail,
			"Not ready — dataset storage isn't available.",
			fmt.Sprintf("Email support@tracebloc.io with the output of `%s doctor --diagnose`.", launcher())}
	case by["Node capacity"].Status == doctor.StatusFail:
		ready = healthLine{doctor.StatusFail,
			"Not ready — not enough free compute to start a training.",
			computeRemedy(runtime.GOOS)}
	// ── Warn: a real problem WAS found; training still runs (exit 0) ──
	case by["Machine capacity"].Status == doctor.StatusWarn:
		// backend#2221 (Bugbot on #541). checkMachineChain warns when the
		// environment claims more memory than the machine actually has -- the
		// uncapped-k3d double-count. This case has to exist, because the shape of
		// that bug is "every individual check looks fine": Node capacity truthfully
		// says a node can fit the job, so without this the rollup printed
		// "Ready to run training" plus "Everything looks good" and exited 0, and the
		// warning was only visible under --verbose. Reporting success we have not
		// earned is worse than not checking at all.
		//
		// Warn, not Fail: training genuinely does start here, so failing the command
		// would be its own lie (and would break every existing 2-node edge's exit
		// code). Warn keeps the exit at 0 while doctorVerdict withholds
		// "everything looks good" -- which is exactly the honest verdict.
		//
		// The remedy is REWORDED rather than passing through the granular
		// check's own Remedy, which is otherwise the obvious move (@LukasWodka
		// on #541). That text names `k3d --servers-memory/--agents-memory`, and
		// these two rolled-up lines are the one place in doctor that is
		// deliberately free of Kubernetes vocabulary -- renderDoctorDetails is
		// documented as "the only place Kubernetes vocabulary appears". So the
		// durable fix is named in plain terms here and the flag-level detail
		// stays one --verbose away, where the granular Remedy already says it.
		ready = healthLine{doctor.StatusWarn,
			"Ready to run training — but your environment thinks this machine is bigger than it is.",
			fmt.Sprintf("It reports more memory than the machine really has, so two trainings that each look like they fit can together run it out of memory and take the environment down. Run one training at a time; to fix it for good, recreate the environment as a single-node one. `%s doctor --verbose` shows the numbers and the exact flags.", launcher())}
	// ── Unknown: a check couldn't complete — no signal, so it never shadows a
	// Fail or Warn above and only surfaces when nothing real was found. ──
	case by["Pod health"].Status == doctor.StatusWarn && strings.HasPrefix(by["Pod health"].Detail, "could not list pods"):
		// checkPods returns StatusWarn for TWO different situations: pods stuck
		// Pending (the Fail arm above) AND a failure to list pods at all (e.g. RBAC,
		// doctor.go checkPods). For the latter we simply can't tell whether training
		// can run, so report an honest can't-check — never the stuck-pending/compute
		// remedy, which would misdiagnose a permissions problem (Bugbot).
		ready = healthLine{doctor.StatusUnknown,
			"Ready to run training — couldn't check your workloads (run with --verbose)", ""}
	case by["Node capacity"].Status == doctor.StatusWarn &&
		(strings.HasPrefix(by["Node capacity"].Detail, "couldn't read RESOURCE_REQUESTS") ||
			strings.HasPrefix(by["Node capacity"].Detail, "could not list nodes") ||
			strings.HasPrefix(by["Node capacity"].Detail, doctor.CantVerifyFreeCompute)):
		// checkNodeFit's Warn covers two different situations: a can't-check
		// (RESOURCE_REQUESTS unreadable, nodes unlistable) and the soft GPU
		// fallback. For a can't-check we simply don't know whether a node can fit
		// a training job, so report an honest can't-check — mirroring the Pod-health
		// list-failure case above — never a ✔ that skipped the capacity probe
		// (Bugbot). The GPU-soft Warn intentionally stays Ready (falls through to
		// the OK default): training still runs via the jobs-manager's CPU fallback.
		ready = healthLine{doctor.StatusUnknown,
			"Ready to run training — couldn't check free compute (run with --verbose)", ""}
	default:
		ready = healthLine{doctor.StatusOK, "Ready to run training", ""}
	}
	return connected, ready
}

// renderHealth prints one rolled-up line: ✔ for OK, ✖ + remedy for a problem,
// and a neutral · "can't check" for StatusUnknown (no false green, no alarm).
func renderHealth(p *ui.Printer, h healthLine) {
	switch h.status {
	case doctor.StatusOK:
		p.Successf("%s", h.text)
	case doctor.StatusUnknown:
		p.Infof("%s", h.text)
	case doctor.StatusWarn:
		// A qualified yes: the thing works, but not cleanly. Rendering it through
		// the Fail branch below would read as "Not ready" for an environment that
		// does run training (backend#2221), and a false alarm spends the same
		// credibility as a false green.
		p.Warnf("%s", h.text)
		if h.remedy != "" {
			p.Hintf("     %s", h.remedy)
		}
	default:
		p.Errorf("%s", h.text)
		if h.remedy != "" {
			p.Hintf("     %s", h.remedy)
		}
	}
}

// renderDoctorDetails is the --verbose (and --diagnose) technical breakdown:
// kubeconfig + every granular check. This is the only place Kubernetes
// vocabulary appears.
// renderDetailsIfVerbose prints the --verbose technical breakdown when the flag
// is set and a cluster config was resolved. Called at every exit that has a
// resolved config — including the no-environment / clientset-error early exits —
// so `tb doctor --verbose` still yields the support-facing detail on the failure
// paths that most need it, not only on the healthy full run.
func renderDetailsIfVerbose(p *ui.Printer, resolved *cluster.ResolvedConfig, results []doctor.Result) {
	if p.Verbose() && resolved != nil {
		renderDoctorDetails(p, resolved, results)
	}
}

func renderDoctorDetails(p *ui.Printer, resolved *cluster.ResolvedConfig, results []doctor.Result) {
	p.Newline()
	p.Section("Details (for support)")
	p.Field("context", resolved.Context)
	p.Field("server", resolved.ServerURL)
	p.Field("namespace", resolved.Namespace)
	for _, r := range results {
		mark := "·"
		switch r.Status {
		case doctor.StatusOK:
			mark = "OK  "
		case doctor.StatusWarn:
			mark = "WARN"
		case doctor.StatusFail:
			mark = "FAIL"
		}
		p.Detailf("%s %s — %s", mark, r.Name, r.Detail)
		if r.Remedy != "" {
			p.Detailf("       %s", r.Remedy)
		}
	}
}

// writeDiagnoseBundle owns the support bundle (moved out of install-k8s.sh, which
// the user may not have on disk). It writes the redacted technical breakdown to a
// file the owner emails to support.
func writeDiagnoseBundle(p *ui.Printer, resolved *cluster.ResolvedConfig, results []doctor.Result, tok tokenState, connected, ready healthLine) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tracebloc doctor — support bundle (%s)\n\n", time.Now().Format(time.RFC3339))
	bp := ui.New(&buf, ui.WithColor(false), ui.WithVerbose(true))
	// Session + rolled-up verdict first — the state support reads before the raw
	// Kubernetes detail, and the reason each remedy sends them here. On an early
	// exit (no environment, clientset error) the roll-up never ran, so record the
	// session state and note the short-circuit instead of empty verdict lines.
	bp.Detailf("session:   %s", tokenLabel(tok))
	switch {
	case connected.text != "":
		bp.Detailf("connected: %s — %s", connected.status, connected.text)
		bp.Detailf("ready:     %s — %s", ready.status, ready.text)
	case len(results) > 0:
		// Probed, but exited before the two-line roll-up (e.g. no chart installed).
		// The granular checks are written below, so don't claim we never probed.
		bp.Detailf("outcome:   early exit — no roll-up verdict (granular checks below)")
	default:
		bp.Detailf("outcome:   early exit before the cluster was probed")
	}
	if resolved != nil {
		renderDoctorDetails(bp, resolved, results)
	}

	name := fmt.Sprintf("tracebloc-doctor-%s.txt", time.Now().Format("20060102-150405"))
	if werr := os.WriteFile(name, buf.Bytes(), 0o600); werr != nil {
		p.Errorf("Couldn't write the support bundle: %v", werr)
		return &exitError{code: exitLocalEnv, err: nil}
	}
	p.Successf("Wrote a support bundle to ./%s", name)
	p.Hintf("     Email it to support@tracebloc.io.")
	return nil
}

// launcher resolves the command name to print in remedies: "tb" on a real
// install (the alias is beside the CLI), else the invoked name — same rule the
// home screen uses, so copy-paste always works.
func launcher() string {
	if tbAliasAvailable() {
		return binTB
	}
	return invokedName()
}

// envDisplayName is the secure environment's user-facing handle — the namespace
// slug (RFC-0001 §7: the slug IS the handle).
func envDisplayName(r *cluster.ResolvedConfig) string {
	if r != nil && r.Namespace != "" {
		return r.Namespace
	}
	return "your secure environment"
}

// reachStateOf returns the "Cluster reachable" check's classification, so the
// caller can tell a reachable-but-uninstalled cluster (ReachNoEnv) apart from one
// that simply isn't answering. ReachOK when the check is absent.
func reachStateOf(results []doctor.Result) doctor.ReachState {
	for _, r := range results {
		if r.Name == "Cluster reachable" {
			return r.Reach
		}
	}
	return doctor.ReachOK
}

// reachConfirmedOK reports whether the "Cluster reachable" check RAN and came
// back ReachOK — a positive confirmation that a tracebloc client is in the probed
// namespace.
//
// Deliberately not `reachStateOf(results) == ReachOK`: reachStateOf defaults to
// ReachOK when the check is ABSENT, which is the right lenient default for the
// main path (an older probe set shouldn't block a verdict) and precisely the
// wrong one for #515's re-probe, where the whole question is whether we may
// believe an unproven namespace. Absent, unreachable and errored all answer
// "we could not tell", and none of them may authorize naming a secure
// environment or advising `client create`.
func reachConfirmedOK(results []doctor.Result) bool {
	for _, r := range results {
		if r.Name == "Cluster reachable" {
			return r.Reach == doctor.ReachOK
		}
	}
	return false
}

// worseStatus returns the more severe of two doctor statuses (Fail > Warn > OK).
// StatusUnknown carries no signal, so it never worsens the verdict.
func worseStatus(a, b doctor.Status) doctor.Status {
	if a == doctor.StatusFail || b == doctor.StatusFail {
		return doctor.StatusFail
	}
	if a == doctor.StatusWarn || b == doctor.StatusWarn {
		return doctor.StatusWarn
	}
	return doctor.StatusOK
}

// doctorVerdict decides the closing line from the two rolled-up health lines.
// It is four-valued because the closing copy has four honest things to say:
//
//	StatusFail    → a real problem, exit 2
//	StatusOK      → BOTH genuinely OK → "everything looks good"
//	StatusWarn    → a problem WAS found and printed one line up, but training
//	                still runs → exit 0, and the closing line must own the
//	                problem, not claim none was found (Bugbot on #541: the old
//	                two-valued verdict lumped Warn in with Unknown, printing
//	                "No problems found, but some checks couldn't finish" —
//	                both halves false for an over-committed machine)
//	StatusUnknown → nothing failed but a check couldn't complete → the partial
//	                "couldn't finish some checks" line (still exit 0)
//
// The key subtlety survives from the two-valued version: OK requires both to
// be StatusOK, NOT merely "not Fail" — a readiness we couldn't determine
// (StatusUnknown, e.g. a pod-list RBAC failure) must not be reported as good,
// even though worseStatus treats Unknown as non-worsening (Bugbot).
func doctorVerdict(connected, ready doctor.Status) doctor.Status {
	switch {
	case worseStatus(connected, ready) == doctor.StatusFail:
		return doctor.StatusFail
	case connected == doctor.StatusOK && ready == doctor.StatusOK:
		return doctor.StatusOK
	case worseStatus(connected, ready) == doctor.StatusWarn:
		return doctor.StatusWarn
	default:
		return doctor.StatusUnknown
	}
}
