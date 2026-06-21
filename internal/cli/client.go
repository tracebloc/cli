package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/slug"
	"github.com/tracebloc/cli/internal/ui"
)

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
	var name, location string
	var yes bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Provision a new client for this machine (--name, --location)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClientCreate(cmd.Context(), printerFor(cmd), clientPrompter(), name, location, yes)
		},
	}
	cmd.Flags().StringVar(&name, "name", "",
		"human-readable client name (shown on your dashboard + carbon reports)")
	cmd.Flags().StringVar(&location, "location", "",
		"location zone for carbon footprint (e.g. DE); prompted if omitted")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
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
	env := cfg.Env
	if env == "" {
		env = api.ResolveEnv("")
	}
	client := newAPIClient(env)
	client.Token = cfg.Token
	return client, cfg, nil
}

func runClientCreate(ctx context.Context, p *ui.Printer, pr prompter, name, location string, yes bool) error {
	client, cfg, err := authedClient()
	if err != nil {
		return &exitError{code: 1, err: err}
	}

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

	// Derive the namespace slug from the name, avoiding collisions with existing
	// clients (best-effort: if the list call fails we still derive a base slug).
	var existing []string
	if clients, lerr := client.ListClients(ctx); lerr == nil {
		for _, c := range clients {
			if c.Namespace != "" {
				existing = append(existing, c.Namespace)
			}
		}
	}
	namespace, err := slug.Derive(name, existing, "client-"+randHex(4))
	if err != nil {
		return &exitError{code: 1, err: err}
	}

	if pr != nil && !yes {
		renderClientReview(p, name, namespace, location)
		ok, cerr := pr.Confirm("Provision this client?", true)
		if cerr != nil {
			return mapClientErr(cerr)
		}
		if !ok {
			p.Hintf("Cancelled.")
			return nil
		}
	}

	// The machine credential: the CLI generates the password, the backend stores
	// it (write-only). The client-runtime authenticates with username+password.
	password := randHex(24)
	pc, err := client.CreateClient(ctx, api.CreateClientRequest{
		Name:      name,
		Namespace: namespace,
		Location:  location,
		Password:  password,
	})
	if err != nil {
		var ae *api.APIError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusForbidden {
			return askAnAdmin(ctx, p, client)
		}
		return &exitError{code: 1, err: err}
	}

	cfg.ActiveClientID = strconv.Itoa(pc.ID)
	if serr := cfg.Save(); serr != nil {
		return &exitError{code: 1, err: serr}
	}

	p.Newline()
	p.Successf("Provisioned client %q (namespace %s).", pc.Name, pc.Namespace)
	p.Section("Machine credential — needed by the installer to connect this client")
	p.Field("client id", strconv.Itoa(pc.ID))
	p.Field("username", pc.Username)
	p.Field("password", password)
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
		if strconv.Itoa(c.ID) == cfg.ActiveClientID {
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
			cfg.ActiveClientID = id
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
func renderClientReview(p *ui.Printer, name, namespace, location string) {
	p.Section("Review")
	p.Field("name", name)
	p.Field("namespace", namespace)
	p.Field("location", location)
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
