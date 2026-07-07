package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/slug"
	"github.com/tracebloc/cli/internal/ui"
)

// readClusterID reads the cluster's kube-system UID — the RFC-0001 idempotency
// anchor (§7.2 / backend#883). A package var so tests can stub it without a
// reachable cluster.
var readClusterID = cluster.ClusterID

// readInClusterClient discovers a tracebloc client already live on the target
// cluster (its CLIENT_ID + namespace) — the RFC-0001 §7.2 / R7 adopt-backfill
// anchor. A package var so tests can stub it without a reachable cluster.
var readInClusterClient = cluster.DiscoverInClusterClient

// newClientCmd wires the `tracebloc client` subtree — provisioning the client
// (machine) this host enrolls as. Consumes the backend provisioning endpoints
// (backend#836) with the user token from `tracebloc login`.
//
// The single-machine CLI (RFC-0001 §7.10) owns exactly one client, so there is
// nothing to *select*: `client use` is withdrawn, and `client list` is hidden
// (kept callable for the installer's one-client-per-machine pre-flight, off the
// user-facing surface). `create` provisions this machine's client; offboarding
// is the top-level `tracebloc delete`.
func newClientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Provision this machine's tracebloc client",
		Long: `Provision a tracebloc client for this machine. Requires sign-in first
(` + "`tracebloc login`" + `). To remove tracebloc from this machine, use
` + "`tracebloc delete`" + `.`,
	}
	cmd.AddCommand(newClientCreateCmd(), newClientListCmd(), newClientStatusCmd())
	return cmd
}

func newClientCreateCmd() *cobra.Command {
	var name, location, kubeconfigPath, contextOverride, credentialFile string
	var yes bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Provision a tracebloc client for this machine (auto-named; no flags required)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClientCreate(cmd.Context(), printerFor(cmd), clientPrompter(),
				clientCreateOpts{name: name, location: location, kubeconfigPath: kubeconfigPath, contextOverride: contextOverride, credentialFile: credentialFile, yes: yes})
		},
	}
	// --name / --location default from their TRACEBLOC_CLIENT_* env vars so an
	// unattended run (or the installer) can set them without the flag; an explicit
	// flag still wins. Empty --name → auto-generated <firstname>-NN; empty
	// --location → sent as unset (no silent default).
	cmd.Flags().StringVar(&name, "name", os.Getenv("TRACEBLOC_CLIENT_NAME"),
		"client name (default: $TRACEBLOC_CLIENT_NAME, else auto-generated <firstname>-NN; shown on your dashboard + carbon reports)")
	cmd.Flags().StringVar(&location, "location", os.Getenv("TRACEBLOC_CLIENT_LOCATION"),
		"optional location zone for carbon reporting, e.g. DE (default: $TRACEBLOC_CLIENT_LOCATION; omitted if unset)")
	cmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "",
		"path to the kubeconfig for the target cluster (default: $KUBECONFIG, then ~/.kube/config) — read to anchor the client to this cluster")
	cmd.Flags().StringVar(&contextOverride, "context", "",
		"kubeconfig context for the target cluster (default: current-context)")
	cmd.Flags().StringVar(&credentialFile, "credential-file", "",
		"write the machine credential to this path (mode 0600, sourceable env) instead of printing it — for the installer to feed the chart (never shown on the terminal)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

// clientCreateOpts bundles the `client create` inputs (flags + resolved prompts).
type clientCreateOpts struct {
	name, location, kubeconfigPath, contextOverride, credentialFile string
	yes                                                             bool
}

// newClientListCmd is HIDDEN (RFC-0001 §7.10): with `use` withdrawn a user has
// nothing to select, but the installer's one-client-per-machine pre-flight still
// shells out to `client list`. Keep it callable, off the user-facing surface.
func newClientListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the clients in your account",
		Hidden:  true,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClientList(cmd.Context(), printerFor(cmd))
		},
	}
}

// clientPrompter returns the interactive prompter on a TTY, else nil (so
// commands fall back to flags-only and never block on a pipe / in CI).
func clientPrompter() prompter {
	if isInteractiveTTY() {
		return surveyPrompter{}
	}
	return nil
}

// sessionEnv resolves the backend env for the signed-in session: the env saved
// at login, falling back (legacy / empty config) to $CLIENT_ENV then prod. Shared
// by authedClient and logout so every authenticated call — including the revoke
// on sign-out — talks to the host the token was actually issued for.
func sessionEnv(cfg *config.Config) string {
	if cfg.CurrentEnv != "" {
		return cfg.CurrentEnv
	}
	return api.ResolveEnv("")
}

// authedClient loads the signed-in config and returns a token-bearing API
// client, or an error telling the user to log in.
func authedClient() (*api.Client, *config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if !cfg.SignedIn() {
		return nil, nil, errors.New("not signed in — run `tracebloc login` first")
	}
	client := newAPIClient(sessionEnv(cfg))
	client.Token = cfg.Current().Token
	return client, cfg, nil
}

func runClientCreate(ctx context.Context, p *ui.Printer, pr prompter, opts clientCreateOpts) (err error) {
	// Always leave a full provision trace on disk, even on a quiet/headless run
	// (RFC-0001 §8.5). On any failure, point at the (idempotent) resume command
	// + `cluster doctor`, so a zero-prompt connect that breaks isn't a dead end.
	ilog, logPath := newInstallLog()
	defer ilog.Close()
	ilog.Logf("client create: name=%q location=%q", opts.name, opts.location)
	defer func() {
		if err != nil {
			ilog.Logf("FAILED: %v", err)
			p.Newline()
			p.Hintf("Provisioning didn't complete. Re-running is safe — on the same cluster it adopts the existing client instead of minting a duplicate (idempotent):")
			p.Hintf("    %s", resumeCommand(opts))
			p.Hintf("Diagnose auth / cluster problems with: tracebloc cluster doctor")
			if logPath != "" {
				p.Hintf("Full log: %s", logPath)
			}
			return
		}
		// Success/cancel: the terminal outcome was already logged at its own
		// branch (minted / adopted / cancelled), so don't blanket-log "done"
		// here — a declined confirm must not read as a successful provision.
		if logPath != "" {
			p.Detailf("full log: %s", logPath)
		}
	}()

	client, cfg, err := authedClient()
	if err != nil {
		return &exitError{code: 1, err: err}
	}
	ilog.Logf("authenticated; provisioning against the signed-in account")

	name, location := opts.name, opts.location
	// cli#137 — the installer path provisions with zero flags and zero prompts:
	//   • name: auto-generated below (<firstname>-NN) once the account's client
	//     list is in hand, so --name is never required; --name /
	//     TRACEBLOC_CLIENT_NAME still override.
	//   • location: optional — never prompted, never required. With no --location we
	//     send nothing (CreateClientRequest.Location is omitempty) and the backend
	//     records the client with no location rather than a silent default
	//     (backend#993). The backend is the source of truth for valid zones — a bad
	//     --location surfaces as its real create error, not a CLI-side guess.

	// Read the cluster anchor (kube-system UID) so create is get-or-create keyed on
	// it — re-running on the same cluster adopts the existing client instead of
	// minting a duplicate (RFC-0001 §7.2 / backend#883). Best-effort + never silent:
	// if the cluster isn't reachable we provision WITHOUT an anchor (a plain mint)
	// and say so, rather than blocking.
	clusterID, cidErr := readClusterID(ctx, cluster.KubeconfigOptions{Path: opts.kubeconfigPath, Context: opts.contextOverride})
	if cidErr != nil {
		p.Hintf("Couldn't read the target cluster's identity — provisioning without a cluster anchor, so re-running won't be idempotent. Point --kubeconfig/--context at the reachable cluster to enable that.")
	}
	ilog.Logf("cluster anchor: %q (read err: %v)", clusterID, cidErr)

	// One account-scoped client list, reused for R7 adopt-backfill and for
	// namespace-collision avoidance on the mint path. A list failure is non-fatal
	// for both (best-effort — we still derive a base slug / fall through to create).
	accountClients, listErr := client.ListClients(ctx)
	if listErr != nil {
		ilog.Logf("client list failed (non-fatal): %v", listErr)
	}

	// Auto-name when neither --name nor TRACEBLOC_CLIENT_NAME was given (cli#137):
	// no prompt, ever. <firstname>-NN, NN = the next free two-digit number across
	// the account's existing clients — a second machine on the account lands on
	// lukas-02, not a slug -2 bump. The derived name is already slug-clean, so it
	// passes through slug.Derive unchanged (display name = namespace = handle).
	if name == "" {
		// Numbering is only unique if we could actually read the account's clients.
		// A list failure would otherwise number against an empty set and mint a
		// DETERMINISTIC duplicate (`<base>-01` almost always already exists), whose
		// name AND namespace collide with no server-side uniqueness to catch it. So
		// fail closed here, exactly like the adopt pre-flight — retry, or name it by
		// hand. (A supplied --name still tolerates a list blip: it's best-effort for
		// slug-collision avoidance only.)
		if listErr != nil {
			// A 426 means the CLI is too old — retrying won't help, so surface the
			// upgrade signal verbatim instead of framing it as a transient outage.
			var ue *api.UpgradeRequiredError
			if errors.As(listErr, &ue) {
				return &exitError{code: 1, err: ue}
			}
			return &exitError{code: 1, err: fmt.Errorf(
				"couldn't reach the backend to choose a unique client name (%v) — retry, "+
					"or pass --name explicitly", listErr)}
		}
		// A re-run on a cluster already anchored to a client will ADOPT that client
		// (get-or-create by cluster_id), so reuse its existing name — otherwise the
		// review/confirm and POST body would describe a freshly-numbered handle
		// (lukas-02) that the backend then ignores in favour of the anchored record.
		if anchored := anchoredClient(accountClients, clusterID); anchored != nil {
			name = anchored.Name
			ilog.Logf("reusing anchored client name %q (re-run on this cluster adopts it)", name)
		} else {
			name = autoClientName(cfg.Current(), accountClients)
			ilog.Logf("auto-named client %q (no --name/TRACEBLOC_CLIENT_NAME)", name)
		}
	}
	// Reflect the resolved name + location back into opts so the failure-path resume
	// command reproduces them — opts otherwise carries only the raw flags (Bugbot).
	opts.name, opts.location = name, location

	var pc *api.ProvisionedClient
	var adopted bool
	// password is the freshly generated machine credential; set only on the mint
	// path below and consumed only by the mint output branch (an adopt keeps the
	// existing credential — §7.2).
	var password string

	// R7 — existing-fleet adopt-backfill (RFC-0001 §7.2). If a client is already
	// live on this cluster but its backend cluster_id is null (it predates the
	// anchor), a create keyed on the freshly-read UID matches nothing and mints a
	// DUPLICATE, orphaning the live client. Instead adopt the live client and
	// backfill its anchor onto it.
	//
	// Run this even when the UID read failed (clusterID == ""): detecting the live
	// client (readInClusterClient) is independent of the anchor, and only the
	// backfill needs it. Gating the whole block on clusterID != "" meant a
	// reachable cluster whose kube-system UID read failed (RBAC on
	// namespaces/kube-system, a transient API error) skipped detection entirely and
	// minted over the live client — orphaning it (#158). adopt now adopts-without-
	// backfill in that case; a genuinely-unreachable cluster fails detection too and
	// falls through to a non-anchored mint, unchanged.
	adoptedPC, handled, aerr := adoptLiveInClusterClient(ctx, p, ilog, client, opts, accountClients, listErr, clusterID)
	if aerr != nil {
		return aerr
	}
	if handled {
		pc, adopted = adoptedPC, true
	}

	if pc == nil {
		// No live client to adopt → mint (or adopt via the backend's cluster_id
		// get-or-create). Derive the namespace slug, avoiding collisions with the
		// account's OTHER clients — skip the one already anchored here (a re-run
		// adopts it, so its namespace isn't a collision and must not bump the slug).
		// Track whether this cluster is already anchored to a client: then
		// CreateClient will adopt (HTTP 200, no credential minted or printed), so the
		// consent guard below must not block that idempotent re-run.
		var existing []string
		willAdopt := false
		for _, c := range accountClients {
			if clusterID != "" && c.ClusterID == clusterID {
				willAdopt = true
				continue
			}
			if c.Namespace != "" {
				existing = append(existing, c.Namespace)
			}
		}
		namespace, derr := slug.Derive(name, existing, "client-"+randHex(4))
		if derr != nil {
			return &exitError{code: 1, err: derr}
		}

		if pr != nil && !opts.yes {
			renderClientReview(p, name, namespace, location, clusterID)
			ok, cerr := pr.Confirm("Provision this client?", true)
			if cerr != nil {
				return mapClientErr(cerr)
			}
			if !ok {
				ilog.Logf("cancelled by user at the confirm prompt")
				p.Hintf("Cancelled.")
				return nil
			}
		} else if pr == nil && !opts.yes && opts.credentialFile == "" && !willAdopt {
			// Non-interactive with no way to confirm AND no --credential-file: a fresh
			// MINT here would side-effect silently and print the machine credential to
			// stdout (into whatever captured it). Require an explicit signal first —
			// --yes to consent, or --credential-file to keep the secret off stdout.
			// Skipped when this cluster is already anchored (willAdopt): that re-run
			// adopts and prints no credential, so it stays zero-friction. The installer
			// passes both flags anyway; this only stops an accidental bare
			// `client create` in a pipe / CI from leaking a freshly minted credential.
			if listErr != nil {
				// The list failed, so willAdopt is unknown — we can't tell a fresh mint
				// (would leak a credential) from an idempotent adopt (safe). Fail closed,
				// but name the real cause: a retry once the backend is reachable will
				// adopt an existing client without any flag.
				return &exitError{code: 1, err: fmt.Errorf(
					"couldn't read the account's client list to tell whether this cluster is new "+
						"or already registered (%v) — retry when the backend is reachable (a re-run "+
						"adopts an existing client), or pass --yes/--credential-file to provision now", listErr)}
			}
			return &exitError{code: 1, err: errors.New(
				"refusing to provision non-interactively without confirmation — pass --yes to " +
					"confirm, and --credential-file to write the credential to a file instead of stdout")}
		}

		// The machine credential: the CLI generates the password, the backend stores
		// it (write-only). Sent on every create but used only when minting — on an
		// idempotent adopt the backend keeps the existing client's credential (§7.2),
		// so the generated value is never printed in that case.
		password = randHex(24)
		var cerr error
		pc, adopted, cerr = client.CreateClient(ctx, api.CreateClientRequest{
			Name:      name,
			Namespace: namespace,
			Location:  location,
			Password:  password,
			ClusterID: clusterID,
		})
		if cerr != nil {
			var ae *api.APIError
			if errors.As(cerr, &ae) {
				switch ae.StatusCode {
				case http.StatusForbidden:
					return askAnAdmin(ctx, p, client, "provision a client", "provisioning")
				case http.StatusConflict:
					// Per RFC C.3 the only 409 on POST /edge-device/ is cluster_conflict
					// (R6): this cluster_id is bound to another account.
					return &exitError{code: 1, err: errors.New(crossAccountConflictMsg)}
				}
			}
			return &exitError{code: 1, err: cerr}
		}
	}

	setActiveClient(cfg.Current(), pc)

	p.Newline()
	if adopted {
		// Idempotent re-run: either the backend matched this cluster_id to an
		// existing client (HTTP 200), or the R7 path above matched a live in-cluster
		// client whose cluster_id was null, backfilled the anchor onto it, and
		// adopted it. Either way — no new credential; the existing one stands.
		ilog.Logf("adopted existing client id=%d namespace=%s", pc.ID, pc.Namespace)
		p.Successf("This cluster is already registered as client %q (namespace %s) — adopted it.", pc.Name, pc.Namespace)
		p.Hintf("No new credential issued; the existing one stands. This machine is set to enroll as client %d.", pc.ID)
		if opts.credentialFile != "" {
			// No password to hand over on adopt (it's write-only on the backend and
			// the existing one stands). Emit id + namespace + an ADOPTED marker so the
			// installer reconciles the existing release rather than expecting a fresh
			// credential (#838).
			if werr := writeClientCredential(opts.credentialFile, []string{
				// TRACEBLOC_CLIENT_ID is the *auth username* the client pod sends to
				// api-token-auth (cred → helm clientId → secret CLIENT_ID →
				// controller getenv("CLIENT_ID") as username). The backend
				// authenticates an EdgeDevice by its UUID username, NOT the numeric
				// dashboard id — so write pc.Username, not pc.ID (id is display-only).
				"TRACEBLOC_CLIENT_ID=" + pc.Username,
				"TB_NAMESPACE=" + pc.Namespace,
				"TRACEBLOC_CLIENT_ADOPTED=1",
			}); werr != nil {
				return &exitError{code: 1, err: werr}
			}
			p.Hintf("Wrote client id + namespace to %s (no new credential — the existing one stands).", opts.credentialFile)
		}
		// Mirror the mint path: a config-save failure shouldn't bury the result —
		// hint how to set the pointer by hand and still exit clean.
		if serr := cfg.Save(); serr != nil {
			p.Hintf("Couldn't save the active-client pointer (%v) — re-run `tracebloc client create` (it adopts this cluster's client) to set it.", serr)
		}
		return nil
	}
	// Mint. With --credential-file the secret goes to a 0600 file (never the
	// terminal — RFC §9 "secure by invisibility") for the installer to source;
	// otherwise it's printed (the interim, until the installer drives this).
	p.Successf("Provisioned client %q (namespace %s).", pc.Name, pc.Namespace)
	if opts.credentialFile != "" {
		if werr := writeClientCredential(opts.credentialFile, []string{
			// The auth username (UUID), NOT the numeric dashboard id — see the
			// adopt path above. api-token-auth authenticates by username.
			"TRACEBLOC_CLIENT_ID=" + pc.Username,
			"TRACEBLOC_CLIENT_PASSWORD=" + password,
			"TB_NAMESPACE=" + pc.Namespace,
		}); werr != nil {
			// The credential is the only copy — a write failure must be fatal, not a
			// silent drop (the installer would have nothing to connect with).
			return &exitError{code: 1, err: werr}
		}
		p.Hintf("Credential written to %s (mode 0600, not shown). This machine is set to enroll as client %d.", opts.credentialFile, pc.ID)
	} else {
		// Print the credential FIRST — it's the only copy (the backend stores only
		// the hash), so a later config-save failure must never cost it.
		p.Section("Machine credential — needed by the installer to connect this client")
		// The installer's "Client ID" prompt takes the auth username (UUID);
		// that IS TRACEBLOC_CLIENT_ID; the numeric id is a dashboard reference only.
		p.Field("client id", pc.Username)
		p.Field("dashboard id", strconv.Itoa(pc.ID)) // human reference at ai.tracebloc.io/clients — NOT an installer input
		p.Field("password", password)
	}
	ilog.Logf("minted client id=%d namespace=%s", pc.ID, pc.Namespace)
	if serr := cfg.Save(); serr != nil {
		p.Hintf("Couldn't save the active-client pointer (%v) — re-run `tracebloc client create` (it adopts this cluster's client).", serr)
	}
	return nil
}

// crossAccountConflictMsg is the guidance shown when this cluster — or the client
// already live on it — belongs to a different tracebloc account. Shared by the
// create 409 (R6) and the R7 not-owned / anchor-taken refusals so they read alike.
const crossAccountConflictMsg = "this cluster is already registered to a different tracebloc account — " +
	"sign in to that account, or ask your admin (cluster_conflict)"

// adoptLiveInClusterClient implements the RFC-0001 §7.2 / R7 adopt-backfill. It
// discovers a tracebloc client already live on the target cluster and, when the
// signed-in account owns it, backfills the cluster anchor onto it (PATCH) and
// returns it for adoption — so a re-run on a pre-anchor box reconciles the live
// client instead of minting a duplicate that orphans it.
//
// Returns (client, true, nil) when it handled provisioning (caller adopts and
// skips the mint); (nil, false, nil) when there's nothing live to adopt (caller
// mints as normal); and a non-nil error to abort — when the live client belongs
// to a DIFFERENT account (never silent-adopt across accounts — R6), when the
// anchor is already taken, or when ownership can't be verified.
func adoptLiveInClusterClient(
	ctx context.Context,
	p *ui.Printer,
	ilog *installLog,
	apiClient *api.Client,
	opts clientCreateOpts,
	accountClients []api.ProvisionedClient,
	listErr error,
	clusterID string,
) (*api.ProvisionedClient, bool, error) {
	live, err := readInClusterClient(ctx, cluster.KubeconfigOptions{Path: opts.kubeconfigPath, Context: opts.contextOverride})
	if err != nil {
		// Best-effort: couldn't inspect the cluster for a live client. Fall through
		// to a plain create (the backend's cluster_id get-or-create still applies).
		ilog.Logf("in-cluster client discovery failed (non-fatal): %v", err)
		return nil, false, nil
	}
	if live == nil {
		return nil, false, nil // fresh cluster — nothing installed to adopt
	}
	ilog.Logf("live in-cluster client: id=%s namespace=%s", live.ClientID, live.Namespace)

	// A client is live here — we must NOT mint over it. If the account couldn't be
	// listed we can't verify ownership, so fail closed (re-run) rather than mint a
	// duplicate (orphan) or adopt across accounts.
	if listErr != nil {
		return nil, false, &exitError{code: 1, err: fmt.Errorf(
			"a tracebloc client is already running on this cluster, but listing your account to verify ownership failed (%w) — re-run once tracebloc is reachable, or resolve manually", listErr)}
	}

	// Is the live client one of THIS account's? Match on the UUID auth username
	// (the value stored in-cluster as CLIENT_ID); the numeric dashboard id isn't
	// readable in-cluster.
	var owner *api.ProvisionedClient
	for i := range accountClients {
		if accountClients[i].Username == live.ClientID {
			owner = &accountClients[i]
			break
		}
	}
	if owner == nil {
		// Live here, but not in the signed-in account — adopting it would be a silent
		// cross-account takeover. Refuse (mirrors the create 409, R6).
		ilog.Logf("live client %s not in this account — refusing cross-account adopt", live.ClientID)
		return nil, false, &exitError{code: 1, err: errors.New(crossAccountConflictMsg)}
	}

	switch {
	case clusterID == "":
		// A live owned client is here, but the cluster UID read failed
		// (namespaces/kube-system unreadable — RBAC, a transient API error) so
		// there's nothing to backfill and no anchor to compare against. The
		// invariant that matters is §7.2 — never mint over a live client — so
		// adopt it as-is (no Patch, no mismatch check) rather than fall through
		// and mint a duplicate. Warn that idempotency wasn't (re)stamped; a
		// cluster where kube-system is readable enables the backfill.
		p.Hintf("A tracebloc client is already running on this cluster — adopting it. Couldn't read the cluster identity, so its idempotency anchor was left unchanged; point --kubeconfig/--context at a cluster where kube-system is readable to stamp it.")
		ilog.Logf("adopting live client id=%d without anchor backfill (cluster UID unread)", owner.ID)
	case owner.ClusterID == "":
		// The R7 case: backfill the freshly-read anchor onto the live client.
		patched, perr := apiClient.PatchClientClusterID(ctx, owner.ID, clusterID)
		if perr != nil {
			var ae *api.APIError
			switch {
			case errors.As(perr, &ae) && ae.StatusCode == http.StatusConflict:
				// Anchor already taken (write-once / bound elsewhere — R6).
				return nil, false, &exitError{code: 1, err: errors.New(crossAccountConflictMsg)}
			case errors.As(perr, &ae) && ae.StatusCode == http.StatusForbidden:
				return nil, false, askAnAdmin(ctx, p, apiClient, "provision a client", "provisioning")
			}
			return nil, false, &exitError{code: 1, err: fmt.Errorf("backfilling the cluster anchor onto the existing client: %w", perr)}
		}
		ilog.Logf("backfilled cluster_id onto client id=%d", owner.ID)
		owner = patched
	case owner.ClusterID != clusterID:
		// The live client is anchored to a DIFFERENT cluster than the one we're
		// pointed at — the kubeconfig and the in-cluster client disagree. Don't
		// re-anchor (write-once); surface it rather than guess.
		return nil, false, &exitError{code: 1, err: fmt.Errorf(
			"the client running in this namespace is anchored to a different cluster (%s) than --kubeconfig/--context points at (%s) — check you're targeting the right cluster",
			owner.ClusterID, clusterID)}
	}

	return owner, true, nil
}

// resumeCommand reconstructs the `tracebloc client create` invocation to retry a
// failed provision. Re-running is idempotent (RFC-0001 §7.2): on the same cluster
// it adopts the existing client rather than minting a duplicate.
func resumeCommand(opts clientCreateOpts) string {
	parts := []string{"tracebloc client create"}
	if opts.name != "" {
		parts = append(parts, "--name "+shellArg(opts.name))
	}
	if opts.location != "" {
		parts = append(parts, "--location "+shellArg(opts.location))
	}
	if opts.kubeconfigPath != "" {
		parts = append(parts, "--kubeconfig "+shellArg(opts.kubeconfigPath))
	}
	if opts.contextOverride != "" {
		parts = append(parts, "--context "+shellArg(opts.contextOverride))
	}
	if opts.credentialFile != "" {
		parts = append(parts, "--credential-file "+shellArg(opts.credentialFile))
	}
	if opts.yes {
		parts = append(parts, "--yes")
	}
	return strings.Join(parts, " ")
}

// shellArg single-quotes an argument containing whitespace so the resume command
// stays copy-pasteable for values like "Lab One".
func shellArg(s string) string {
	if strings.ContainsAny(s, " \t") {
		return "'" + s + "'"
	}
	return s
}

// writeClientCredential writes the machine credential to path (mode 0600) as a
// shell-sourceable env file — the installer (#838) sources it to feed the chart,
// so the secret lands in a 0600 file, never the terminal (RFC §9 never-show). The
// values are constrained charsets (numeric id, hex password, DNS-1123 slug), so
// no shell-escaping is needed.
//
// Written via a 0600 temp file + atomic rename rather than os.WriteFile: WriteFile
// only applies its perm bits when it *creates* the file, so a pre-existing target
// (a stale file, or one an attacker pre-creates world-readable) would keep its old
// mode and leak the secret — the 0600 guarantee must hold unconditionally. The temp
// also avoids following a symlink at the target and never leaves a half-written
// credential behind.
func writeClientCredential(path string, lines []string) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating credential-file directory: %w", err)
		}
	}
	body := "# tracebloc client credential — written by `tracebloc client create`.\n" +
		"# Mode 0600; sourced by the installer. Do not commit or share.\n" +
		strings.Join(lines, "\n") + "\n"
	// CreateTemp makes the file 0600 by construction, in the target dir so the
	// rename stays on one filesystem.
	f, err := os.CreateTemp(dir, ".cred-*")
	if err != nil {
		return fmt.Errorf("writing credential file %s: %w", path, err)
	}
	tmp := f.Name()
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("writing credential file %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("writing credential file %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("writing credential file %s: %w", path, err)
	}
	return nil
}

// askAnAdmin renders the "you can't do this — here's who can" path (a 403 from
// the backend means no CLIENT_WRITE; backend#836 Q4).
// action is the infinitive of what the caller was denied ("provision a client",
// "offboard this machine"); capability is its noun form for the returned error
// ("provisioning", "offboarding"). Both take CLIENT_WRITE, so the admin list is
// the same — only the copy differs between provisioning and offboarding.
func askAnAdmin(ctx context.Context, p *ui.Printer, client *api.Client, action, capability string) error {
	p.Newline()
	p.Hintf("You don't have permission to %s in this account.", action)
	if admins, err := client.ListClientAdmins(ctx); err == nil && len(admins) > 0 {
		p.Section("Ask one of these admins (or ask them to grant you access)")
		for _, a := range admins {
			label := a.Name
			if label == "" {
				label = a.Email
			}
			p.Field(label, a.Email)
		}
	}
	return &exitError{code: 1, err: fmt.Errorf("%s requires CLIENT_WRITE permission", capability)}
}

func runClientList(ctx context.Context, p *ui.Printer) error {
	client, cfg, err := authedClient()
	if err != nil {
		return &exitError{code: 1, err: err}
	}
	clients, err := client.ListClients(ctx)
	if err != nil {
		return &exitError{code: 1, err: err}
	}
	if len(clients) == 0 {
		p.Hintf("No clients yet. Run `tracebloc client create`.")
		return nil
	}
	p.Section("Clients in your account")
	active := cfg.Current().ActiveClientID
	for _, c := range clients {
		marker := ""
		if strconv.Itoa(c.ID) == active {
			marker = "  (active — this machine)"
		}
		p.Field(strconv.Itoa(c.ID)+marker,
			fmt.Sprintf("%s   state=%s   namespace=%s   location=%s",
				c.Name, clientStateLabel(c.Status), c.Namespace, c.Location))
	}
	// §7.3: separate "selected" (this machine's local pointer) from "connected"
	// (the backend's last-heartbeat state) so a stale pointer is visible.
	p.Hintf("\"active\" is this machine's selected client; state is its last reported status to tracebloc.")
	return nil
}

func newClientStatusCmd() *cobra.Command {
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether tracebloc can see this machine's client (online)",
		Long: `Report tracebloc's view of this machine's active client — online, offline,
or pending. With --wait, poll until tracebloc reports it online (exit 0) or the
timeout elapses (non-zero), to confirm the client connected after setup.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --timeout only governs the --wait poll; accepting it alone would be a
			// silent no-op, so reject it rather than mislead.
			if cmd.Flags().Changed("timeout") && !wait {
				return &exitError{code: 1, err: errors.New("--timeout has no effect without --wait")}
			}
			return runClientStatus(cmd.Context(), printerFor(cmd), wait, timeout)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "poll until tracebloc reports this client online")
	cmd.Flags().DurationVar(&timeout, "timeout", 120*time.Second, "with --wait, give up after this long")
	return cmd
}

// clientStatusPollInterval is how often --wait re-checks the backend. A const,
// not a seam: tests inject through pollAfter (which ignores the duration and
// fires instantly), so the value never needs overriding.
const clientStatusPollInterval = 3 * time.Second

func runClientStatus(ctx context.Context, p *ui.Printer, wait bool, timeout time.Duration) error {
	client, cfg, err := authedClient()
	if err != nil {
		return &exitError{code: 1, err: err}
	}
	active := cfg.Current().ActiveClientID
	if active == "" {
		return &exitError{code: 1, err: errors.New(
			"no active client on this machine — run `tracebloc client create` (or re-run the installer) first")}
	}

	// One-shot: report the current state and exit 0 (informational).
	if !wait {
		st, found, lerr := lookupClientStatus(ctx, client, active)
		if lerr != nil {
			return &exitError{code: 1, err: lerr}
		}
		if !found {
			return &exitError{code: 1, err: fmt.Errorf(
				"active client %s isn't in your account — run `tracebloc client create` "+
					"(or re-run the installer) to provision this machine", active)}
		}
		p.Section("Client status")
		p.Field("state", clientStateLabel(st))
		return nil
	}

	// --wait: poll the same source `client list` renders until online or timeout.
	// The backend-reported status is the honest Online signal (RFC-0001 §8.5); a
	// local rollout check can't tell whether tracebloc can actually see the client.
	sp := p.Spinner("Waiting for tracebloc to confirm…", "")
	defer sp.Stop() // leak-proof net for every return; the online path Stops explicitly before its ✔
	deadline := time.Now().Add(timeout)
	var lastErr error // most recent transient (retryable) error, for the timeout message
	lastState := -1   // most recent successfully-read status; -1 = none read yet
	var ue *api.UpgradeRequiredError
	var apiErr *api.APIError
	for {
		st, found, lerr := lookupClientStatus(ctx, client, active)
		switch {
		case errors.As(lerr, &ue):
			// 426 (CLI too old) won't recover by waiting — surface the upgrade signal.
			return &exitError{code: 1, err: lerr}
		case errors.As(lerr, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden):
			// A revoked/expired/forbidden token (401/403) won't recover by waiting —
			// the client itself may be online. Fail fast, point at sign-in. Note 429
			// and 5xx stay transient (below): those DO recover on retry.
			return &exitError{code: 1, err: errors.New(
				"tracebloc rejected your credentials while waiting — run `tracebloc login`, then retry")}
		case lerr != nil:
			lastErr = lerr // transient (5xx / 429 / network) — keep waiting, remember why
		case !found:
			// The active client isn't in the account (deleted / wrong account) — no
			// amount of waiting surfaces it. Fail fast, matching the one-shot path.
			return &exitError{code: 1, err: fmt.Errorf(
				"active client %s isn't in your account — run `tracebloc client create` "+
					"(or re-run the installer) to provision this machine", active)}
		case st == clientStatusOnline:
			sp.Stop()
			p.Successf("tracebloc can see this client.")
			return nil
		default:
			// A good poll (present, not online yet) supersedes any earlier transient
			// error, so a later timeout reports the real state, not a stale failure.
			lastErr, lastState = nil, st
		}

		// --timeout caps how long we WAIT: stop once the budget is spent, and clamp
		// the sleep to what remains so total runtime doesn't overshoot by a poll
		// cycle. A poll begun within budget is still honored (online → ✔ above).
		remaining := time.Until(deadline)
		if remaining <= 0 {
			switch {
			case lastErr != nil:
				return &exitError{code: 1, err: fmt.Errorf(
					"timed out after %s waiting for tracebloc to report this client online; "+
						"the last status check failed: %v", timeout, lastErr)}
			case lastState >= 0:
				return &exitError{code: 1, err: fmt.Errorf(
					"timed out after %s waiting for tracebloc to report this client online (last state: %s). "+
						"Run `tracebloc cluster doctor` to diagnose, or re-run the installer.", timeout, clientStateLabel(lastState))}
			default:
				return &exitError{code: 1, err: fmt.Errorf(
					"timed out after %s before tracebloc could confirm this client — retry, "+
						"or run `tracebloc cluster doctor`.", timeout)}
			}
		}
		wait := clientStatusPollInterval
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return &exitError{code: 130} // Ctrl-C: exit quietly (no "Error: context canceled")
		case <-pollAfter(wait):
		}
	}
}

// lookupClientStatus finds the account client whose numeric id matches active and
// returns its backend status code. found=false means no such client (deleted, or
// signed into the wrong account). A list error is returned verbatim so --wait can
// treat it as transient and retry.
func lookupClientStatus(ctx context.Context, client *api.Client, active string) (status int, found bool, err error) {
	clients, err := client.ListClients(ctx)
	if err != nil {
		return 0, false, err
	}
	if c := findClientByID(clients, active); c != nil {
		return c.Status, true, nil
	}
	return 0, false, nil
}

// findClientByID returns the account client whose numeric dashboard id equals id
// (the string form stored as the active-client pointer), or nil if none match.
func findClientByID(clients []api.ProvisionedClient, id string) *api.ProvisionedClient {
	for i := range clients {
		if strconv.Itoa(clients[i].ID) == id {
			return &clients[i]
		}
	}
	return nil
}

// EdgeDevice.status codes mirrored from the backend (metaApi User.py).
const (
	clientStatusOffline = 0
	clientStatusOnline  = 1
	clientStatusPending = 2
)

// clientStateLabel maps the backend status code to a TTY/CI-safe word. Plain
// text (not an emoji glyph) on purpose — flag/emoji glyphs mojibake in CI logs
// and Windows consoles (RFC-0001 §12 watch-item).
func clientStateLabel(status int) string {
	switch status {
	case clientStatusOnline:
		return "online"
	case clientStatusOffline:
		return "offline"
	case clientStatusPending:
		return "pending"
	default:
		return "unknown"
	}
}

// setActiveClient points this env's profile at c, caching its namespace and
// display name alongside the id so the data commands can bind to the active
// client's cluster (§7.3) without a backend round-trip. Callers Save() after.
func setActiveClient(p *config.Profile, c *api.ProvisionedClient) {
	p.ActiveClientID = strconv.Itoa(c.ID)
	p.ActiveClientNamespace = c.Namespace
	p.ActiveClientName = c.Name
}

// renderClientReview shows the assembled inputs before the confirm prompt, so
// the user sees the derived namespace and location before anything is created.
func renderClientReview(p *ui.Printer, name, namespace, location, clusterID string) {
	p.Section("Review")
	p.Field("name", name)
	p.Field("namespace", namespace)
	// Location is optional (cli#137) — only show the field when one was given, so a
	// zero-prompt create doesn't render a blank "location:" line.
	if location != "" {
		p.Field("location", location)
	}
	if clusterID != "" {
		p.Field("cluster", clusterID+"  (anchors this client — re-runs adopt it)")
	}
}

// anchoredClient returns the account client already bound to clusterID (the
// kube-system UID), or nil. A non-nil result means a re-run on this cluster will
// adopt that client, so callers should reuse its name rather than mint a new one.
func anchoredClient(clients []api.ProvisionedClient, clusterID string) *api.ProvisionedClient {
	if clusterID == "" {
		return nil
	}
	for i := range clients {
		if clients[i].ClusterID == clusterID {
			return &clients[i]
		}
	}
	return nil
}

// autoClientName derives the client name used when neither --name nor
// TRACEBLOC_CLIENT_NAME was given (cli#137): <base>-NN, where base is the
// signed-in user's first name (slugified), falling back to the email local-part,
// then a generic "client". NN is the lowest two-digit number ≥ 1 not already used
// by an existing client's name OR namespace — so a second machine on the account
// lands on lukas-02 rather than the slug package's -2 collision bump, and the
// derived name is guaranteed collision-free through slug.Derive (name = namespace).
func autoClientName(prof *config.Profile, existing []api.ProvisionedClient) string {
	base := ""
	if prof != nil {
		if base = slug.Slugify(prof.FirstName); base == "" {
			base = slug.Slugify(emailLocalPart(prof.Email))
		}
	}
	if base == "" {
		base = "client"
	}
	// Reserve each existing client's handle in BOTH raw and slugified form: a legacy
	// client stored with a display name like "Lukas 01" (and a blank/legacy
	// namespace) must still block the derived handle "lukas-01", which is what
	// slug.Derive would produce for it.
	taken := make(map[string]struct{}, 4*len(existing))
	reserve := func(s string) {
		if s == "" {
			return
		}
		taken[s] = struct{}{}
		taken[slug.Slugify(s)] = struct{}{}
	}
	for _, c := range existing {
		reserve(c.Name)
		reserve(c.Namespace)
	}
	for n := 1; ; n++ {
		suffix := fmt.Sprintf("-%02d", n)
		// Keep the whole handle within the DNS-1123 label cap so it survives
		// slug.Derive unchanged — otherwise a long first_name yields name != namespace
		// and reintroduces the exact slug -2 bump this numbering exists to avoid.
		b := base
		if len(b)+len(suffix) > slug.MaxLabelLength {
			b = strings.TrimRight(b[:slug.MaxLabelLength-len(suffix)], "-")
		}
		cand := b + suffix
		if _, clash := taken[cand]; !clash {
			return cand
		}
	}
}

// emailLocalPart returns the part of an email before '@' (the whole string when
// there's no '@'), the fallback source for an auto-name when first_name is empty.
func emailLocalPart(email string) string {
	if i := strings.IndexByte(email, '@'); i >= 0 {
		return email[:i]
	}
	return email
}

// mapClientErr turns a cancelled interactive prompt into a clean exit.
func mapClientErr(err error) error {
	if errors.Is(err, errInteractiveCancelled) {
		return nil
	}
	return &exitError{code: 1, err: err}
}

// randHex returns nbytes of crypto-random data hex-encoded.
func randHex(nbytes int) string {
	b := make([]byte, nbytes)
	_, _ = rand.Read(b) // crypto/rand.Read does not fail on a healthy system
	return hex.EncodeToString(b)
}
