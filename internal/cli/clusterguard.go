package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

// clusterIDFromFn is a test seam over cluster.ClusterIDFrom.
var clusterIDFromFn = cluster.ClusterIDFrom

// targetVerifyTimeout bounds the backend lookup guardActiveClientCluster makes to
// NAME the target cluster. The lookup only makes the guard better-informed, so it
// must never make a mutating command slower than the mutation would be: past this,
// the guard falls back to the local anchor and the --no-input contract below.
const targetVerifyTimeout = 8 * time.Second

// listAccountClientsFn resolves the signed-in account's clients for the target
// verifier (verifyTargetFromAPI). Production authenticates with the stored token
// and calls the backend; tests override it. An error means "couldn't ask the API"
// — offline, not signed in, or a backend failure — which the verifier treats as
// "couldn't confirm", never as "the account has no clients".
var listAccountClientsFn = func(ctx context.Context) ([]api.ProvisionedClient, error) {
	client, _, err := authedClient()
	if err != nil {
		return nil, err
	}
	return client.ListClients(ctx)
}

// guardActiveClientCluster refuses a MUTATING operation unless it can confirm WHICH
// cluster it is about to change — and prints that identity when it can.
//
// WHY THIS EXISTS (backend#2863, then backend#2983). Every command resolved its
// target cluster from the ambient kubeconfig + current-context, while binding only
// the NAMESPACE from the active client. So on a machine whose current-context
// pointed elsewhere — a laptop that also administers a managed cluster, which is the
// normal case for anyone who runs both — a mutating command acted on that other
// cluster:
//
//   - `data ingest` staged a private dataset onto it
//   - `data delete` dropped a table and removed files from its shared PVC
//   - `resources set` rolled its jobs-manager with a new envelope
//   - `tracebloc delete` uninstalled a release of the same name
//
// backend#2863 added a local anchor to compare against, but left one path open: a
// machine with NO recorded anchor warned "the target couldn't be verified …
// Proceeding against https://127.0.0.1:8444" and then mutated anyway (backend#2983).
// The endpoint it named is a localhost port-forward — the same command shape reaches
// a different fleet depending on which tunnel is up on which port — so the operator's
// only signal about which environment was about to change was a port number, and
// --no-input (whose contract is "fail on missing required values") did not fail
// closed. A CI job could ingest into the wrong fleet and get a green exit.
//
// The fix is two-fold, matching the issue's directions:
//
//  1. VERIFY, don't merely record. Ask the API which client owns the cluster we
//     actually reached and print THAT identity (name + namespace) — an identity
//     that does not depend on local state, which is precisely what was missing in
//     the field. verifyTargetFromAPI keys on the live kube-system UID, so the
//     printed name is the backend's answer, not config's.
//  2. FAIL CLOSED when the target is unverifiable. If the API can't confirm the
//     target and no recorded anchor matches it, refuse (exit non-zero, no write)
//     unless the operator explicitly accepts the target with --i-know-the-target.
//     A scripted (--no-input) caller has no human to ask, so this honours its
//     contract; a human gets the same escape hatch, named in the message.
//
// The check is on IDENTITY, not on the context name. Pinning a context string would
// break bring-your-own-cluster installs (EKS/AKS/OpenShift have no k3d context) and
// would still pass if two kubeconfigs named the same context differently. The
// kube-system namespace UID is the same anchor the backend record uses, so local,
// remote, and backend agree by construction.
//
// FAILURE MODES, deliberately asymmetric:
//
//   - id unreadable    -> REFUSE. We are about to write to a cluster we cannot
//     identify at all. --i-know-the-target overrides (nothing left to check).
//   - anchor mismatch  -> REFUSE. Affirmative evidence of the WRONG cluster;
//     --i-know-the-target does NOT override a known-wrong target.
//   - API-verified     -> print the identity and proceed.
//   - anchor matches   -> proceed (positive local evidence; works offline).
//   - unverifiable     -> REFUSE unless --i-know-the-target (backend#2983).
//
// Read-only commands never call this: being wrong about which cluster you are
// READING is a confusing answer, not a destructive act, and the target is already
// printed by doctor / cluster info.
func guardActiveClientCluster(ctx context.Context, p *ui.Printer, t *clusterTarget, ackTarget bool) error {
	if t == nil || t.Clientset == nil {
		return nil // nothing resolved (a test seam, or a command that mutates nothing)
	}
	serverURL := t.Resolved.ServerURL

	// 1. Read the live cluster's own identity — the kube-system UID, the fingerprint
	//    the backend record and the local anchor both key on. Offline-readable;
	//    unreadable means we cannot name what we are about to write to.
	got, idErr := clusterIDFromFn(ctx, t.Clientset)
	if idErr != nil {
		// A Ctrl-C (or parent deadline) during the read is an operator abort, not an
		// unreadable cluster: exit quietly (130) rather than refuse-or-proceed on it.
		if err := interrupted(ctx); err != nil {
			return err
		}
		if ackTarget {
			p.Warnf("Couldn't read this cluster's identity (%v) — proceeding anyway because "+
				"--i-know-the-target was set. Target: %s.", idErr, serverURL)
			return nil
		}
		return &exitError{code: exitLocalEnv, err: fmt.Errorf(
			"couldn't confirm which cluster this is before changing anything (%w).\n"+
				"  Refusing rather than writing to an unidentified cluster.\n"+
				"  reached: %s\n"+
				"  Fix your --context/--kubeconfig, or pass --i-know-the-target if you are certain.",
			idErr, serverURL)}
	}

	// 2. Local anchor mismatch (backend#2863): the machine recorded which cluster its
	//    secure environment runs on, and this is a DIFFERENT one. Affirmative evidence
	//    of the wrong target — a hard refusal that --i-know-the-target does NOT
	//    override (the flag accepts an UNKNOWN target, never a known-wrong one).
	want := recordedClusterAnchor()
	if want != "" && got != want {
		return &exitError{code: exitLocalEnv, err: fmt.Errorf(
			"this is not the cluster your secure environment runs on — refusing to change anything.\n"+
				"  reached:  %s  (cluster %s, context %q)\n"+
				"  expected: cluster %s\n"+
				"  Your kubeconfig's current context points somewhere else. Either switch it, or pass\n"+
				"  --context/--kubeconfig for the cluster your secure environment runs on.",
			serverURL, short(got), t.Resolved.Context, short(want))}
	}

	// 3. VERIFY THE IDENTITY FROM THE API (backend#2983, direction 1). Ask tracebloc
	//    which client owns the cluster we reached and print THAT — an identity that
	//    does not depend on local state (a missing local anchor is exactly what fell
	//    open in the field). Authoritative and self-checking: the printed name is the
	//    backend record keyed on the live cluster fingerprint, never config's.
	c, reached, verr := verifyTargetFromAPI(ctx, got)
	if verr != nil {
		// Only a 426 (this CLI is too old for the server) reaches here — every other
		// lookup failure is folded into reached=false below. A too-old CLI must
		// HARD-STOP before mutating and before the offline anchor-match: it is exactly
		// when we must not press on against a stale local anchor (Bugbot; learned rule
		// "HTTP 426 must be a hard failure, not a warning"). --i-know-the-target does
		// not override it — upgrading is the only way through, and the error says so.
		return &exitError{code: exitFailure, err: verr}
	}
	// A Ctrl-C during the backend lookup cancels our context and surfaces as a plain
	// lookup error, which verifyTargetFromAPI folds into reached=false. Left unchecked
	// that would let the anchor-match below AUTHORIZE the mutation after the operator
	// aborted (Bugbot), or refuse with a misleading "couldn't reach / run login" at
	// exit 3 instead of a quiet 130. Catch the abort here — the inner verify timeout
	// does not cancel THIS context, so a merely-slow backend still reads as reached=false.
	if err := interrupted(ctx); err != nil {
		return err
	}
	if c != nil {
		p.Successf("Target verified with tracebloc: %s (namespace %s) — cluster %s.",
			c.Name, c.Namespace, short(got))
		return nil
	}

	// 3b. The API couldn't name it (offline / not signed in / no such record), but the
	//     machine's RECORDED anchor matches the live cluster: the operator ran
	//     `client create` here, so this is positive local evidence — proceed. This is
	//     NOT the backend#2983 fail-open, which was NO evidence at all (want == "").
	if want != "" && got == want {
		p.Infof("Target matches this machine's recorded cluster (%s) — proceeding.", short(got))
		return nil
	}

	// 4. UNVERIFIABLE: the API can't confirm the target and no recorded anchor matches
	//    it. This is the backend#2983 fail-open. Honour --no-input's contract — a
	//    scripted caller has no human to ask, so FAIL CLOSED — and require an explicit
	//    --i-know-the-target to proceed otherwise.
	if ackTarget {
		p.Warnf("Couldn't verify which cluster this is — proceeding anyway because "+
			"--i-know-the-target was set. Target: %s (cluster %s).", serverURL, short(got))
		return nil
	}
	// The refusal must not overstate what's known. Two distinct situations, and
	// neither may assert an ABSENCE:
	//   - reached=false: we couldn't ask the API at all — say exactly that.
	//   - reached=true, no anchored match: the API answered but nothing in the account
	//     is anchored to this cluster's UID. This is NOT "no client here" — discovery
	//     already found a client release on this cluster to get us this far, and a
	//     legacy client's ClusterID is empty so it can never match by UID (Bugbot).
	//     So the honest framing is "not LINKED to your account records", and
	//     `client create` is the right fix precisely because it ADOPTS the on-cluster
	//     client and records the anchor.
	if reached {
		return &exitError{code: exitLocalEnv, err: fmt.Errorf(
			"couldn't verify which cluster this is before changing anything — the tracebloc client on this "+
				"cluster isn't linked to any client in your account (it may be newly installed, or created "+
				"before cluster anchoring), and this machine hasn't recorded one.\n"+
				"  reached: %s  (cluster %s)\n"+
				"  Refusing rather than writing to an unverified cluster.\n"+
				"  Run `tracebloc client create` — it adopts the client already on this cluster and records it — "+
				"or fix your --context/--kubeconfig, or\n"+
				"  re-run with --i-know-the-target if you are certain this is the right cluster.",
			serverURL, short(got))}
	}
	return &exitError{code: exitLocalEnv, err: fmt.Errorf(
		"couldn't verify which cluster this is before changing anything — tracebloc couldn't be reached to "+
			"confirm the target, and this machine hasn't recorded one.\n"+
			"  reached: %s  (cluster %s)\n"+
			"  Refusing rather than writing to an unverified cluster.\n"+
			"  Check you're signed in (`tracebloc login`) and pointed at the right --context/--kubeconfig, or\n"+
			"  re-run with --i-know-the-target if you are certain this is the right cluster.",
		serverURL, short(got))}
}

// recordedClusterAnchor returns this machine's recorded cluster anchor (the
// active client's kube-system UID), or "" when there is no readable config or no
// anchor. Best-effort by contract: a config it cannot read is "no anchor", which
// routes to the API verification / fail-closed path rather than a hard error.
func recordedClusterAnchor() string {
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	return cfg.Current().ActiveClientClusterID
}

// verifyTargetFromAPI asks the backend which client owns the cluster identified by
// clusterID (the kube-system UID) and returns that record. The identity it returns
// is the API's, not local config's — which is the whole point (backend#2983): the
// field failure was a MISSING local anchor, so a check that read local state could
// never have caught it.
//
// It reports THREE outcomes, because collapsing "couldn't reach the API" into "the
// API says there is no such client" is the #515 trap the rest of the codebase is
// careful about (DiscoverInClusterClientID's three-valued return, clientSurvey.looked):
// telling an operator "tracebloc has no client for this cluster — run client create"
// when we simply could not ask is a false absence, and `client create` on an
// UNCONFIRMED cluster is the adopt-vs-mint hazard.
//
//   - (client, true,  nil) — the API answered and a client is anchored here: verified.
//   - (nil,    true,  nil) — the API answered but NO account client is anchored to this
//     cluster's UID. NOT a proof of absence: discovery already found a client release
//     on the cluster, and a legacy client's ClusterID is empty so it can never match by
//     UID. The caller frames this as "not linked to your account" and points at
//     `client create`, which adopts the on-cluster client and records the anchor.
//   - (nil,    false, nil) — the API could not be reached / not signed in / a transient
//     backend error: we could NOT ask. The caller must not assert absence.
//   - (nil,    false, err) — the API returned 426 Upgrade Required. This alone
//     propagates as an error: a CLI too old for the server must HARD-STOP, never be
//     swallowed and then press on against a local anchor (learned rule / Bugbot).
func verifyTargetFromAPI(ctx context.Context, clusterID string) (client *api.ProvisionedClient, reached bool, err error) {
	if clusterID == "" {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, targetVerifyTimeout)
	defer cancel()
	clients, lerr := listAccountClientsFn(ctx)
	if lerr != nil {
		var ue *api.UpgradeRequiredError
		if errors.As(lerr, &ue) {
			return nil, false, lerr // 426 → hard-stop; the caller exits with the upgrade message
		}
		return nil, false, nil // offline / not signed in / transient → couldn't ask
	}
	// The API answered (reached=true). anchoredClient is the same kube-system-UID →
	// client match `client create` uses to decide adopt-vs-mint; reuse it so the two
	// paths can't drift. A nil result here is a CONFIRMED absence, not a couldn't-ask.
	return anchoredClient(clients, clusterID), true, nil
}

// interrupted returns the quiet Ctrl-C exit (130) when the command's context was
// cancelled during a blocking guard step — an operator abort (or a parent deadline),
// NOT an unreadable/unreachable cluster. It checks the context itself, so it is not
// fooled by verifyTargetFromAPI's inner timeout (which cancels only its own derived
// context, never this one). Returns nil while the context is still live, so callers
// fall through to their normal refuse/proceed logic.
//
// The exitError carries NO inner err on purpose: exitInterrupted is a SILENT exit
// (IsSilentError keys on err==nil), so main() prints nothing — same as every other
// interrupt path (auth.go, seal.go, client_status.go). Attaching ctx.Err() would
// print a bare "Error: context canceled" on Ctrl-C.
func interrupted(ctx context.Context) error {
	if ctx.Err() != nil {
		return &exitError{code: exitInterrupted}
	}
	return nil
}

// short trims a UID for messages — enough to compare by eye, not a wall of hex.
func short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}
