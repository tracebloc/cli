package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
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
		return &exitError{code: exitFailure, err: err}
	}
	env := api.ResolveEnv(envFlag)
	// login PICKS the session env and persists it (cfg.CurrentEnv below), so a
	// typo must fail HERE, not silently resolve to prod. BaseURL's lenient
	// unknown→prod fallback would otherwise route `--env staging` / `CLIENT_ENV=prd`
	// to production and store it as the active env for every later command.
	if !api.IsKnownEnv(env) {
		return &exitError{code: exitFailure, err: fmt.Errorf(
			"unknown backend environment %q — valid values are dev, stg, prod (default). "+
				"Check --env / $CLIENT_ENV", env)}
	}
	client := newAPIClient(env)
	p.Detailf("backend %s — requesting a device code …", client.BaseURL)

	dc, err := client.RequestDeviceCode(ctx)
	if err != nil {
		var ae *api.APIError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return &exitError{code: exitFailure, err: fmt.Errorf(
				"this backend (%s) doesn't support browser login yet — the device-grant "+
					"endpoints land in backend#835: %w", env, err)}
		}
		return &exitError{code: exitFailure, err: err}
	}

	p.Section("Sign in to tracebloc")
	uri := dc.VerificationURIComplete
	if uri == "" {
		uri = dc.VerificationURI
	}
	p.Action("Open", uri)
	p.Action("Enter", dc.UserCode)
	p.Newline()

	// Poll the device-token endpoint behind a live "Waiting…" spinner (static on
	// a pipe / --plain). The spinner line is cleared on return, so the ✔ / error
	// below prints in its place.
	tok, err := pollForToken(ctx, p, client, dc)
	if err != nil {
		return err
	}

	// Switch the active env and write into THAT env's profile, leaving the other
	// envs' tokens + active-client pointers intact (R10). Profile() returns env's
	// existing profile, so a re-login preserves its active_client_id.
	cfg.CurrentEnv = env
	prof := cfg.Profile(env)
	prof.Token = tok
	// Clear any identity carried over from a PREVIOUS sign-in on this env before the
	// best-effort lookup: if WhoAmI fails, a re-login as a different user on a shared
	// box would otherwise keep the prior user's email/first name — and cli#137 would
	// then auto-name the new client after the wrong person. Only a successful WhoAmI
	// repopulates these. (Preserved from cli#137 across the cli#138 refactor.)
	prof.Email, prof.FirstName = "", ""
	// Confirm the freshly-issued token authenticates and capture the account to
	// show + store. Best-effort: don't fail a successful sign-in if this can't run.
	client.Token = tok
	p.Detailf("authorized — confirming the token with the backend …")
	if id, werr := client.WhoAmI(ctx); werr == nil {
		prof.Email = id.Email
		prof.FirstName = id.FirstName
	}
	if err := cfg.Save(); err != nil {
		return &exitError{code: exitFailure, err: err}
	}
	if prof.Email != "" {
		p.Successf("Signed in as %s.", prof.Email)
	} else {
		p.Successf("Signed in.")
	}
	// The credential detail is demoted to a dim, verbose-only line — the ✔ above is
	// the headline (RFC-0001 §8.1: the happy path stays quiet).
	p.Detailf("token saved to ~/.tracebloc (0600)")
	return nil
}

// pollDisposition is what the poll loop does with a failed PollToken call.
type pollDisposition int

const (
	// pollStop — the sign-in cannot succeed. Report it and exit.
	pollStop pollDisposition = iota
	// pollAgain — an expected non-answer (not approved yet). Poll again unchanged.
	pollAgain
	// pollSlower — the server asked us to back off (RFC 8628 §3.5).
	pollSlower
	// pollRetry — an infrastructure failure, not a verdict on the sign-in. Poll
	// again, but count it: a backend that never answers must say so eventually.
	pollRetry
)

// maxPollFailures bounds how many CONSECUTIVE pollRetry outcomes the loop rides
// out before giving up and naming the last one. At the RFC's 5-second floor
// that is a minute of unbroken failure — long enough to cross a wifi handover,
// a DNS blip, or a backend restart; short enough that a genuinely unreachable
// backend reports itself instead of silently burning the code's whole window.
// The code's own expiry still bounds the loop; the counter only makes the
// give-up message honest when the network, not the human, is at fault.
const maxPollFailures = 12

// classifyPollError decides whether a PollToken failure ends the sign-in or is
// worth another poll inside the code's remaining window (cli#517).
//
// The default used to be "stop", which made one DNS hiccup fatal to an
// installer run that had already built a cluster. The default is now "retry",
// so every genuinely terminal state is enumerated HERE rather than being the
// leftover case — a refusal must never become an infinite loop:
//
//   - the four RFC 8628 §3.5 sentinels are terminal or not by the spec;
//   - a 426 means this CLI is below the server's version floor — polling can't
//     make it newer;
//   - an *APIError carries a server VERDICT: 5xx / 408 / 429 are the server or a
//     proxy failing temporarily, every other status is a refusal we must respect;
//   - a cancelled context is the operator, not a blip;
//   - everything left never reached a server verdict at all — DNS, refused
//     connection, TLS, a truncated read, a body we couldn't decode — and is the
//     class this function exists to keep alive.
func classifyPollError(err error) pollDisposition {
	switch {
	case errors.Is(err, api.ErrAuthorizationPending):
		return pollAgain
	case errors.Is(err, api.ErrSlowDown):
		return pollSlower
	case errors.Is(err, api.ErrExpiredToken), errors.Is(err, api.ErrAccessDenied):
		return pollStop
	case errors.Is(err, context.Canceled):
		return pollStop
	}
	var ue *api.UpgradeRequiredError
	if errors.As(err, &ue) {
		return pollStop
	}
	var ae *api.APIError
	if errors.As(err, &ae) {
		switch {
		case ae.StatusCode >= 500,
			ae.StatusCode == http.StatusRequestTimeout,
			ae.StatusCode == http.StatusTooManyRequests:
			return pollRetry
		default:
			return pollStop
		}
	}
	return pollRetry
}

// withSignInAdvice appends the command that starts a fresh sign-in — or leaves
// the error exactly as it is when the installer is driving us (cli#517).
// "`tracebloc login`" is right when a human typed it and WRONG under the
// installer, where a bare login leaves the client mint and the Helm install
// undone; the installer prints its own, correct next step, and two contradicting
// instructions are worse than one. The installer announces itself with
// TRACEBLOC_INSTALLER.
//
// The advice is appended by wrapping rather than composed into each message, so
// every sentence stays a literal argument of an errors.New / fmt.Errorf call —
// which is what keeps this copy visible to the copy catalog's AST harvest.
func withSignInAdvice(err error) error {
	if os.Getenv("TRACEBLOC_INSTALLER") != "" {
		return err
	}
	return fmt.Errorf("%w. Run `tracebloc login` to start a new one", err)
}

// signInWindow renders the code's advertised lifetime as a clause for the
// expiry copy, or "" when the server didn't say. Named in the message because
// without it a ten-minute timeout reads as an instant failure to anyone who
// stepped away — which is exactly how cli#517 was first reported. Derived from
// expires_in rather than hardcoded, so the sentence cannot outlive a change to
// the backend's DEVICE_CODE_TTL.
func signInWindow(expiresIn int) string {
	if expiresIn <= 0 {
		return ""
	}
	d := time.Duration(expiresIn) * time.Second
	human := d.Round(time.Second).String()
	switch {
	case d == time.Minute:
		human = "1 minute"
	case d%time.Minute == 0:
		human = fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	return fmt.Sprintf(" — sign-in codes are valid for %s", human)
}

// terminalSignInError renders the user-facing copy for a poll outcome the loop
// must stop on. An error we have no bespoke copy for is surfaced verbatim: a
// vague "sign-in failed" would hide the only diagnostic we have.
func terminalSignInError(err error, window string) error {
	switch {
	case errors.Is(err, api.ErrExpiredToken):
		return withSignInAdvice(fmt.Errorf("the sign-in code expired%s", window))
	case errors.Is(err, api.ErrAccessDenied):
		return withSignInAdvice(errors.New("sign-in was denied in the browser"))
	}
	return err
}

// pollForToken runs the RFC 8628 device-token poll loop behind a live wait
// spinner, returning the issued token or an *exitError. The spinner is cleared
// on every return path (deferred Stop), so the caller prints the ✔ / error line
// on the freed line.
func pollForToken(ctx context.Context, p *ui.Printer, client *api.Client, dc *api.DeviceCodeResponse) (string, error) {
	interval := dc.Interval
	if interval <= 0 {
		interval = 5
	}
	var deadline time.Time
	if dc.ExpiresIn > 0 {
		deadline = time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	}
	window := signInWindow(dc.ExpiresIn)

	sp := p.Spinner("Waiting for your browser…", "Ctrl-C to cancel")
	defer sp.Stop()

	// Consecutive infrastructure failures. Reset by any answer from the server —
	// a blip mid-way through a long wait must not accumulate toward the cap.
	var failures int
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return "", &exitError{code: exitFailure, err: withSignInAdvice(
				fmt.Errorf("the sign-in code expired before it was approved%s", window))}
		}
		select {
		case <-ctx.Done():
			return "", &exitError{code: exitInterrupted} // Ctrl-C: exit quietly (no "Error: context canceled")
		case <-pollAfter(time.Duration(interval) * time.Second):
		}

		tok, err := client.PollToken(ctx, dc.DeviceCode)
		if err == nil {
			return tok, nil
		}
		// Ctrl-C landing DURING the request surfaces as a cancelled context on the
		// HTTP call, not on the select above — exit quietly there too, rather than
		// reporting the operator's own interrupt as a sign-in failure.
		if ctx.Err() != nil {
			return "", &exitError{code: exitInterrupted}
		}
		switch classifyPollError(err) {
		case pollAgain:
			failures = 0 // not approved yet — keep polling
		case pollSlower:
			// RFC 8628 §3.5: on slow_down the client MUST increase the poll
			// interval by 5 seconds for this and all subsequent polls.
			failures = 0
			interval += 5
		case pollRetry:
			failures++
			if failures >= maxPollFailures {
				// One literal, not a concatenation: the copy catalog's AST harvest
				// only sees whole string literals, so a "+"-joined message is copy
				// nothing inventories.
				return "", &exitError{code: exitFailure, err: fmt.Errorf(
					"couldn't reach the backend to finish signing in — %d attempts failed in a row (check your network / HTTPS_PROXY): %w",
					failures, err)}
			}
		default: // pollStop
			return "", &exitError{code: exitFailure, err: terminalSignInError(err, window)}
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
				return &exitError{code: exitFailure, err: err}
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
				return &exitError{code: exitFailure, err: err}
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
		// Bare `tracebloc auth` prints help; a mistyped subcommand errors with a
		// suggestion instead of silently exiting 0 (#75).
		RunE:                       runGroup,
		SuggestionsMinimumDistance: 2,
	}
	cmd.AddCommand(newAuthStatusCmd())
	return cmd
}

// newAuthStatusCmd implements `tracebloc auth status`.
func newAuthStatusCmd() *cobra.Command {
	var check bool
	var envFlag string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether you're signed in, and to which backend",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if check {
				return runAuthCheck(cmd.Context(), printerFor(cmd), envFlag)
			}
			cfg, err := config.Load()
			if err != nil {
				return &exitError{code: exitFailure, err: err}
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
	// --check is the installer's session probe: `auth status` alone exits 0 whether
	// signed in or not (it's a human display), so scripts had to grep its prose.
	// --check makes the exit CODE the contract instead.
	cmd.Flags().BoolVar(&check, "check", false,
		"exit 0 only if signed in with a backend-valid token, else 1; silent unless --verbose")
	cmd.Flags().StringVar(&envFlag, "env", "",
		"backend environment the check targets: dev|stg|prod (default: $CLIENT_ENV, then prod)")
	return cmd
}

// runAuthCheck is `auth status --check`: a machine-readable session probe for the
// installer. Exit 0 = the machine is signed in to the TARGET environment with a
// token the backend accepts (a live WhoAmI); exit 1 = signed out, signed in to a
// different env, or the token was rejected/unreachable. Silent by default;
// --verbose narrates the verdict. The exit-1 paths return a nil-inner *exitError
// (IsSilentError) so main() prints nothing.
//
// The target env is resolved exactly like `login` (--env, then $CLIENT_ENV, then
// prod), and must match the signed-in env as sessionEnv resolves it — otherwise the probe would OK a
// stale session for the wrong backend and the installer would skip the very
// `login` that switches env, provisioning into the wrong account (RFC-0001 §10).
func runAuthCheck(ctx context.Context, p *ui.Printer, envFlag string) error {
	cfg, err := config.Load()
	if err != nil {
		if p.Verbose() {
			p.Hintf("Not signed in. Run `tracebloc login`.")
		}
		return &exitError{code: exitFailure}
	}
	target := api.ResolveEnv(envFlag)
	// Compare the RESOLVED session env, not the raw cfg.CurrentEnv: target comes
	// out of api.ResolveEnv already normalised, so comparing it against the stored
	// string made this the one place a `"current_env": "Dev"` config failed a probe
	// for the session it is actually signed in to.
	signedIn := sessionEnv(cfg)
	if !cfg.SignedIn() || signedIn != target {
		if p.Verbose() {
			if cfg.SignedIn() && signedIn != target {
				p.Hintf("Signed in to %q, but this run targets %q — run `tracebloc login`.", signedIn, target)
			} else {
				p.Hintf("Not signed in. Run `tracebloc login`.")
			}
		}
		return &exitError{code: exitFailure}
	}
	// Signed in AND CurrentEnv == target: probe it. authedClient() builds the client
	// for sessionEnv (== CurrentEnv == target) with the stored token — reuse it and
	// discard its message (the exit code is the contract here).
	client, _, err := authedClient()
	if err != nil {
		if p.Verbose() {
			p.Hintf("Not signed in. Run `tracebloc login`.")
		}
		return &exitError{code: exitFailure}
	}
	if _, err := client.WhoAmI(ctx); err != nil {
		// A 426 means the CLI is too old, not that the session is invalid — surface
		// the upgrade instruction (non-silent, so it shows even without --verbose)
		// instead of the "re-login" advice, which wouldn't help.
		var ue *api.UpgradeRequiredError
		if errors.As(err, &ue) {
			return &exitError{code: exitFailure, err: ue}
		}
		if p.Verbose() {
			// Only a 401/403 is genuinely a rejected token (where re-login helps); a
			// network/DNS/5xx failure means we couldn't verify, not that the session
			// is invalid — don't send the user to re-login for an outage.
			var apiErr *api.APIError
			if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
				p.Hintf("Signed-in token was rejected by the backend — run `tracebloc login`.")
			} else {
				p.Hintf("Couldn't verify your session with the backend (%v).", err)
			}
		}
		return &exitError{code: exitFailure}
	}
	if p.Verbose() {
		if email := cfg.Current().Email; email != "" {
			p.Successf("Signed in as %s.", email)
		} else {
			p.Successf("Signed in.")
		}
	}
	return nil
}
