package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

// newLoginCmd implements `tracebloc login` — browser sign-in via the OAuth 2.0
// Device Authorization Grant (RFC 8628). Works on a headless box: the CLI shows
// a URL + short code, the human approves in a browser on any device, and the
// CLI polls until a user token is issued and stored in ~/.tracebloc (0600).
// The backend endpoints land in backend#835; until then login reports that the
// backend doesn't support browser sign-in yet.
func newLoginCmd() *cobra.Command {
	var envFlag string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to tracebloc in your browser (device flow)",
		Long: `Sign in to tracebloc. The CLI prints a URL + short code; open the URL
on any device (your laptop or phone), sign in the way you already do
(password, Google, or GitHub), and approve the code. The CLI stores a
user token in ~/.tracebloc (mode 0600).

Works on a headless / SSH box — the browser and the CLI need not share a
machine. Honors HTTP(S)_PROXY / NO_PROXY for corporate-proxy networks.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd.Context(), printerFor(cmd), envFlag)
		},
	}
	cmd.Flags().StringVar(&envFlag, "env", "",
		"backend environment: dev|stg|prod (default: $CLIENT_ENV, then prod)")
	return cmd
}

// Test seams: the device flow makes real HTTP calls on a timer, so tests
// override the client factory (point it at an httptest server) and the poll
// clock (fire immediately) rather than hitting the network / wall clock.
var (
	newAPIClient = api.New
	pollAfter    = time.After
)

func runLogin(ctx context.Context, p *ui.Printer, envFlag string) error {
	cfg, err := config.Load()
	if err != nil {
		return &exitError{code: 1, err: err}
	}
	env := api.ResolveEnv(envFlag)
	client := newAPIClient(env)
	p.Detailf("backend %s — requesting a device code …", client.BaseURL)

	dc, err := client.RequestDeviceCode(ctx)
	if err != nil {
		var ae *api.APIError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return &exitError{code: 1, err: fmt.Errorf(
				"this backend (%s) doesn't support browser login yet — the device-grant "+
					"endpoints land in backend#835: %w", env, err)}
		}
		return &exitError{code: 1, err: err}
	}

	p.Section("Sign in to tracebloc")
	uri := dc.VerificationURIComplete
	if uri == "" {
		uri = dc.VerificationURI
	}
	p.Field("open", uri)
	p.Field("code", dc.UserCode)
	p.Newline()
	p.Hintf("Waiting for you to approve in the browser… (Ctrl-C to cancel)")

	interval := dc.Interval
	if interval <= 0 {
		interval = 5
	}
	var deadline time.Time
	if dc.ExpiresIn > 0 {
		deadline = time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	}

	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return &exitError{code: 1, err: errors.New("login timed out — re-run `tracebloc login`")}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pollAfter(time.Duration(interval) * time.Second):
		}

		tok, err := client.PollToken(ctx, dc.DeviceCode)
		switch {
		case err == nil:
			// Switch the active env and write into THAT env's profile, leaving the
			// other envs' tokens + active-client pointers intact (R10). Profile()
			// returns env's existing profile, so a re-login preserves its
			// active_client_id rather than clobbering it.
			cfg.CurrentEnv = env
			prof := cfg.Profile(env)
			prof.Token = tok
			// Confirm the freshly-issued token actually authenticates, and
			// capture the account to show + store. Best-effort: don't fail a
			// successful sign-in just because this lookup couldn't run.
			client.Token = tok
			p.Detailf("authorized — confirming the token with the backend …")
			if id, werr := client.WhoAmI(ctx); werr == nil {
				prof.Email = id.Email
				prof.FirstName = id.FirstName
			}
			if err := cfg.Save(); err != nil {
				return &exitError{code: 1, err: err}
			}
			p.Newline()
			if prof.Email != "" {
				p.Successf("Signed in as %s. Token saved to ~/.tracebloc (0600).", prof.Email)
			} else {
				p.Successf("Signed in. Token saved to ~/.tracebloc (0600).")
			}
			return nil
		case errors.Is(err, api.ErrAuthorizationPending):
			// not approved yet — keep polling
		case errors.Is(err, api.ErrSlowDown):
			// RFC 8628 §3.5: on slow_down the client MUST increase the poll
			// interval by 5 seconds for this and all subsequent polls.
			interval += 5
		case errors.Is(err, api.ErrExpiredToken):
			return &exitError{code: 1, err: errors.New("the sign-in code expired — re-run `tracebloc login`")}
		case errors.Is(err, api.ErrAccessDenied):
			return &exitError{code: 1, err: errors.New("sign-in was denied in the browser")}
		default:
			return &exitError{code: 1, err: err}
		}
	}
}

// newLogoutCmd implements `tracebloc logout` — revokes the token server-side
// (so a copied/leaked credential stops working) and clears it locally.
func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign out (revoke the token server-side and clear it locally)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := printerFor(cmd)
			cfg, err := config.Load()
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			if !cfg.SignedIn() {
				p.Hintf("Already signed out.")
				return nil
			}

			// Capture what the server-side revoke needs BEFORE clearing local
			// state. Resolve the env the same way authedClient does (current env,
			// else $CLIENT_ENV, else prod) so revoke hits the host the token was
			// issued for, not a hardcoded prod.
			prof := cfg.Current()
			token := prof.Token
			env := sessionEnv(cfg)

			// Clear and persist local state FIRST — it's logout's primary job and
			// the always-safe step. Saving before the network call means a failed
			// Save can't leave a token that's already been revoked server-side
			// sitting on disk as a broken "signed in" state. Only THIS env's
			// profile is cleared; other envs' sessions are untouched (R10). The
			// active-client pointer goes too — it's account-scoped, so leaving it
			// would bleed into the next sign-in on this env.
			*prof = config.Profile{}
			if err := cfg.Save(); err != nil {
				return &exitError{code: 1, err: err}
			}

			// Then revoke the token server-side so a copied/leaked credential stops
			// authenticating after sign-out (RFC-0001 §7.5 / R2, backend#887).
			// Best-effort by contract: on failure (offline / already-revoked) the
			// local session is already cleared — the user is logged out (cli#112).
			client := newAPIClient(env)
			client.Token = token
			if rerr := client.RevokeToken(cmd.Context()); rerr != nil {
				p.Hintf("Signed out locally, but couldn't revoke the token server-side (%v). Revoke from the dashboard if this was a shared machine.", rerr)
				return nil
			}
			p.Successf("Signed out.")
			return nil
		},
	}
}

// newAuthCmd is the `tracebloc auth` parent; today it carries `auth status`.
func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Inspect tracebloc authentication state",
	}
	cmd.AddCommand(newAuthStatusCmd())
	return cmd
}

// newAuthStatusCmd implements `tracebloc auth status`.
func newAuthStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether you're signed in, and to which backend",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			p := printerFor(cmd)
			if !cfg.SignedIn() {
				p.Hintf("Not signed in. Run `tracebloc login`.")
				return nil
			}
			prof := cfg.Current()
			p.Section("tracebloc auth")
			p.Field("status", "signed in")
			p.Field("backend", cfg.CurrentEnv)
			if prof.Email != "" {
				p.Field("account", prof.Email)
			}
			if prof.ActiveClientID != "" {
				p.Field("active client", prof.ActiveClientID)
			}
			if prof.ExpiresAt != "" {
				p.Field("expires", prof.ExpiresAt)
			}
			return nil
		},
	}
}
