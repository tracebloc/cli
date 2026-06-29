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

// newClientCmd wires the `tracebloc client` subtree — provisioning + selecting
// the client (machine) this host enrolls as. Consumes the backend provisioning
// endpoints (backend#836) with the user token from `tracebloc login`.
func newClientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Provision and manage tracebloc clients (machines)",
		Long: `Provision a tracebloc client for this machine and list/select clients
in your account.  Requires sign-in first (` + "`tracebloc login`" + `).`,
	}
	cmd.AddCommand(newClientCreateCmd(), newClientListCmd(), newClientUseCmd())
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
		// Never silent-empty: the prompt requires a non-empty zone. (Cloud /
		// GeoIP auto-detect of a suggested default is a fast-follow.)
		if location, err = pr.Input("Location zone (e.g. DE)", "physical zone, for the carbon footprint", "", validateNonEmpty); err != nil {
			return mapClientErr(err)
		}
	}

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

	// Derive the namespace slug from the name, avoiding collisions with existing
	// clients (best-effort: if the list call fails we still derive a base slug).
	var existing []string
	if clients, lerr := client.ListClients(ctx); lerr == nil {
		for _, c := range clients {
			// Skip the client already anchored to this cluster: a re-run adopts it
			// (the backend keys on cluster_id), so its namespace isn't a collision.
			// Counting it would bump the derived slug and show a namespace in the
			// review that doesn't match the one actually adopted.
			if clusterID != "" && c.ClusterID == clusterID {
				continue
			}
			if c.Namespace != "" {
				existing = append(existing, c.Namespace)
			}
		}
	}
	namespace, err := slug.Derive(name, existing, "client-"+randHex(4))
	if err != nil {
		return &exitError{code: 1, err: err}
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
	password := randHex(24)
	pc, adopted, err := client.CreateClient(ctx, api.CreateClientRequest{
		Name:      name,
		Namespace: namespace,
		Location:  location,
		Password:  password,
		ClusterID: clusterID,
	})
	if err != nil {
		var ae *api.APIError
		if errors.As(err, &ae) {
			switch ae.StatusCode {
			case http.StatusForbidden:
				return askAnAdmin(ctx, p, client)
			case http.StatusConflict:
				// Per RFC C.3 the only 409 on POST /edge-device/ is cluster_conflict
				// (R6): this cluster_id is bound to another account.
				return &exitError{code: 1, err: errors.New(
					"this cluster is already registered to a different tracebloc account — " +
						"sign in to that account, or ask your admin (cluster_conflict)")}
			}
		}
		return &exitError{code: 1, err: err}
	}

	cfg.Current().ActiveClientID = strconv.Itoa(pc.ID)

	p.Newline()
	if adopted {
		// Idempotent re-run: the backend matched this cluster_id to an existing
		// client and returned it — no new credential. (The existing-fleet R7 case,
		// where the backend instead matches a live in-cluster TB_CLIENT_ID whose
		// cluster_id is still null and the CLI backfills it via PATCH, is the
		// installer's orchestration — #838 — not done here.)
		ilog.Logf("adopted existing client id=%d namespace=%s", pc.ID, pc.Namespace)
		p.Successf("This cluster is already registered as client %q (namespace %s) — adopted it.", pc.Name, pc.Namespace)
		p.Hintf("No new credential issued; the existing one stands. This machine is set to enroll as client %d.", pc.ID)
		if opts.credentialFile != "" {
			// No password to hand over on adopt (it's write-only on the backend and
			// the existing one stands). Emit id + namespace + an ADOPTED marker so the
			// installer reconciles the existing release rather than expecting a fresh
			// credential (#838).
			if werr := writeClientCredential(opts.credentialFile, []string{
				"TRACEBLOC_CLIENT_ID=" + strconv.Itoa(pc.ID),
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
			"TRACEBLOC_CLIENT_ID=" + strconv.Itoa(pc.ID),
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
		p.Field("client id", strconv.Itoa(pc.ID))
		p.Field("username", pc.Username)
		p.Field("password", password)
	}
	ilog.Logf("minted client id=%d namespace=%s", pc.ID, pc.Namespace)
	if serr := cfg.Save(); serr != nil {
		p.Hintf("Couldn't save the active-client pointer (%v) — run `tracebloc client use %d` to set it.", serr, pc.ID)
	}
	return nil
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
	for _, c := range clients {
		marker := ""
		if strconv.Itoa(c.ID) == cfg.Current().ActiveClientID {
			marker = "  (active)"
		}
		p.Field(strconv.Itoa(c.ID)+marker,
			fmt.Sprintf("%s   namespace=%s   location=%s", c.Name, c.Namespace, c.Location))
	}
	return nil
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
	for _, c := range clients {
		if strconv.Itoa(c.ID) == id {
			cfg.Current().ActiveClientID = id
			if serr := cfg.Save(); serr != nil {
				return &exitError{code: 1, err: serr}
			}
			p.Successf("This machine is now set to enroll as client %s (%s).", id, c.Name)
			return nil
		}
	}
	return &exitError{code: 1, err: fmt.Errorf(
		"no client %s in your account — run `tracebloc client list` to see the ids", id)}
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
