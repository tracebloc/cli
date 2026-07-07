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
	"github.com/tracebloc/cli/internal/geo"
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

// newClientCmd wires the `tracebloc client` subtree — provisioning + selecting
// the client (machine) this host enrolls as. Consumes the backend provisioning
// endpoints (backend#836) with the user token from `tracebloc login`.
func newClientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Provision and manage the clients in your account",
		Long: `Provision a tracebloc client for this machine and list/select clients
in your account.  Requires sign-in first (` + "`tracebloc login`" + `).`,
	}
	cmd.AddCommand(newClientCreateCmd(), newClientListCmd(), newClientUseCmd(), newClientStatusCmd())
	return cmd
}

func newClientCreateCmd() *cobra.Command {
	var name, location, kubeconfigPath, contextOverride, credentialFile string
	var yes bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Provision a new client for this machine (--name, --location)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClientCreate(cmd.Context(), printerFor(cmd), clientPrompter(),
				clientCreateOpts{name: name, location: location, kubeconfigPath: kubeconfigPath, contextOverride: contextOverride, credentialFile: credentialFile, yes: yes})
		},
	}
	cmd.Flags().StringVar(&name, "name", "",
		"human-readable client name (shown on your dashboard + carbon reports)")
	cmd.Flags().StringVar(&location, "location", "",
		"location zone for carbon footprint (e.g. DE); prompted if omitted")
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

func newClientListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the clients in your account",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClientList(cmd.Context(), printerFor(cmd))
		},
	}
}

func newClientUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <client-id>",
		Short: "Enroll this machine as an existing client",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClientUse(cmd.Context(), printerFor(cmd), args[0])
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

// detectZone suggests a location zone (cloud metadata → GeoIP). A seam so tests
// stay hermetic (no network).
var detectZone = geo.Detect

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

	// Gather inputs first (flags win; prompt only what's missing, and only on a
	// TTY), then show one review + confirm — matching the dataset-push flow.
	if name == "" {
		if pr == nil {
			return errMissingFlag("--name")
		}
		if name, err = pr.Input("Client name", "shown on your dashboard + carbon reports", "", validateNonEmpty); err != nil {
			return mapClientErr(err)
		}
	}
	if location == "" {
		if pr == nil {
			return errMissingFlag("--location")
		}
		// Auto-detect a suggested zone (cloud metadata → IP geolocation) and
		// pre-fill it as the prompt default; the user confirms with Enter or
		// overrides. Never silent (it's a prompt), never empty (validateNonEmpty).
		suggested := ""
		help := "electricityMaps zone for the carbon footprint (e.g. DE)"
		if z := detectZone(ctx); z != nil {
			suggested = z.Code
			help = fmt.Sprintf("detected %s via %s (%s confidence) — Enter to accept, or type your zone",
				z.Code, z.Source, z.Confidence)
		}
		if location, err = pr.Input("Location zone (e.g. DE)", help, suggested, validateNonEmpty); err != nil {
			return mapClientErr(err)
		}
	}
	// Reflect the resolved (possibly prompted) name + location back into opts, so
	// the failure-path resume command includes them — opts otherwise carries only
	// the flags, omitting anything the user typed at a prompt (Bugbot).
	opts.name, opts.location = name, location

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
	// backfill its anchor onto it. Needs a readable anchor (nothing to stamp
	// otherwise); without one we fall through to a plain create (dual-mode).
	if clusterID != "" {
		adoptedPC, handled, aerr := adoptLiveInClusterClient(ctx, p, ilog, client, opts, accountClients, listErr, clusterID)
		if aerr != nil {
			return aerr
		}
		if handled {
			pc, adopted = adoptedPC, true
		}
	}

	if pc == nil {
		// No live client to adopt → mint (or adopt via the backend's cluster_id
		// get-or-create). Derive the namespace slug, avoiding collisions with the
		// account's OTHER clients — skip the one already anchored here (a re-run
		// adopts it, so its namespace isn't a collision and must not bump the slug).
		var existing []string
		for _, c := range accountClients {
			if clusterID != "" && c.ClusterID == clusterID {
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
					return askAnAdmin(ctx, p, client)
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
			p.Hintf("Couldn't save the active-client pointer (%v) — run `tracebloc client use %d` to set it.", serr, pc.ID)
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
		p.Hintf("Couldn't save the active-client pointer (%v) — run `tracebloc client use %d` to set it.", serr, pc.ID)
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
				return nil, false, askAnAdmin(ctx, p, apiClient)
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

// askAnAdmin renders the "you can't provision — here's who can" path (a 403 from
// the backend means no CLIENT_WRITE; backend#836 Q4).
func askAnAdmin(ctx context.Context, p *ui.Printer, client *api.Client) error {
	p.Newline()
	p.Hintf("You don't have permission to provision a client in this account.")
	if admins, err := client.ListClientAdmins(ctx); err == nil && len(admins) > 0 {
		p.Section("Ask one of these admins to provision it (or grant you access)")
		for _, a := range admins {
			label := a.Name
			if label == "" {
				label = a.Email
			}
			p.Field(label, a.Email)
		}
	}
	return &exitError{code: 1, err: errors.New("provisioning requires CLIENT_WRITE permission")}
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
			"no active client on this machine — run `tracebloc client create` (or `client use <id>`) first")}
	}

	// One-shot: report the current state and exit 0 (informational).
	if !wait {
		st, found, lerr := lookupClientStatus(ctx, client, active)
		if lerr != nil {
			return &exitError{code: 1, err: lerr}
		}
		if !found {
			return &exitError{code: 1, err: fmt.Errorf(
				"active client %s isn't in your account list — run `tracebloc client list` to see "+
					"your clients, or re-run the installer to provision this machine", active)}
		}
		p.Section("Client status")
		p.Field("state", clientStateLabel(st))
		return nil
	}

	// --wait: poll the same source `client list` renders until online or timeout.
	// The backend-reported status is the honest Online signal (RFC-0001 §8.5); a
	// local rollout check can't tell whether tracebloc can actually see the client.
	sp := p.Spinner("Waiting for tracebloc to confirm…", "")
	deadline := time.Now().Add(timeout)
	var lastErr error // most recent transient list error, for an honest timeout message
	var ue *api.UpgradeRequiredError
	var apiErr *api.APIError
	for {
		st, found, lerr := lookupClientStatus(ctx, client, active)
		switch {
		case errors.As(lerr, &ue):
			// A 426 (too-old CLI) is not a transient outage — polling it until timeout
			// just hides the real cause. Fail fast with the upgrade signal.
			sp.Stop()
			return &exitError{code: 1, err: lerr}
		case errors.As(lerr, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden):
			// A revoked/expired/forbidden token (401/403) won't recover by waiting —
			// the client itself may be perfectly online. Fail fast, point at sign-in.
			sp.Stop()
			return &exitError{code: 1, err: errors.New(
				"tracebloc rejected your credentials while waiting — run `tracebloc login`, then retry")}
		case lerr != nil:
			lastErr = lerr // transient — keep polling, but remember why for the timeout
		case !found:
			// The active client isn't in the account (deleted / wrong account). No
			// amount of waiting surfaces it — fail fast, matching the one-shot path.
			sp.Stop()
			return &exitError{code: 1, err: fmt.Errorf(
				"active client %s isn't in your account list — run `tracebloc client list` to see "+
					"your clients, or re-run the installer to provision this machine", active)}
		case st == clientStatusOnline:
			sp.Stop()
			p.Successf("tracebloc can see this client.")
			return nil
		}
		if time.Now().After(deadline) {
			sp.Stop()
			if lastErr != nil {
				// Every check failed — surface the real reason, not a bare "unreachable".
				return &exitError{code: 1, err: fmt.Errorf(
					"timed out after %s waiting for tracebloc to report this client online; "+
						"the last status check failed: %v", timeout, lastErr)}
			}
			// Reached here only via lerr==nil && found (else we'd have returned), so
			// st holds the last real state (offline/pending).
			return &exitError{code: 1, err: fmt.Errorf(
				"timed out after %s waiting for tracebloc to report this client online (last state: %s). "+
					"Run `tracebloc cluster doctor` to diagnose, or re-run the installer.", timeout, clientStateLabel(st))}
		}
		select {
		case <-ctx.Done():
			sp.Stop()
			return ctx.Err()
		case <-pollAfter(clientStatusPollInterval):
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

func runClientUse(ctx context.Context, p *ui.Printer, id string) error {
	client, cfg, err := authedClient()
	if err != nil {
		return &exitError{code: 1, err: err}
	}
	clients, err := client.ListClients(ctx)
	if err != nil {
		return &exitError{code: 1, err: err}
	}
	if c := findClientByID(clients, id); c != nil {
		setActiveClient(cfg.Current(), c)
		if serr := cfg.Save(); serr != nil {
			return &exitError{code: 1, err: serr}
		}
		p.Successf("This machine is now set to enroll as client %s (%s).", id, c.Name)
		return nil
	}
	return &exitError{code: 1, err: fmt.Errorf(
		"no client %s in your account — run `tracebloc client list` to see the ids", id)}
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
	p.Field("location", location)
	if clusterID != "" {
		p.Field("cluster", clusterID+"  (anchors this client — re-runs adopt it)")
	}
}

// errMissingFlag reports a required flag absent in a non-interactive run (no TTY
// to prompt — CI, a pipe, or output redirected).
func errMissingFlag(flag string) error {
	return &exitError{code: 1, err: fmt.Errorf("%s is required (non-interactive — no TTY to prompt)", flag)}
}

// validateNonEmpty rejects blank prompt input.
func validateNonEmpty(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("required")
	}
	return nil
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
