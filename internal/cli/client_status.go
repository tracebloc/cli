// `tracebloc client status` — tracebloc's view of this machine's client
// (online / offline / pending, with --wait to poll), plus the --seal mode that
// verifies the environment's protections via the chart's conformance checks
// (seal.go). Split out of client.go, which sits at its file-budget ceiling.

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/ui"
)

func newClientStatusCmd() *cobra.Command {
	var wait, seal bool
	var timeout time.Duration
	var kubeconfigPath, contextOverride, nsOverride string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether tracebloc can see this machine's client (online)",
		Long: `Report tracebloc's view of this machine's active client — online, offline,
or pending. With --wait, poll until tracebloc reports it online (exit 0) or the
timeout elapses (non-zero), to confirm the client connected after setup.

With --seal, verify the environment's protections instead: run the chart's
conformance checks (its helm tests — the seal check, RFC-0003) against this
machine's secure environment and report the verdict with per-check detail:

  sealed     every conformance check passed
  unsealed   a check failed or couldn't run — a protection is not enforced
  unknown    the chart ships no conformance checks, so nothing was verified

Only a fully-passed suite exits 0; an environment that can't be verified is
never claimed sealed.

Exit codes with --seal:
  0   sealed — every conformance check passed
  2   unsealed (a check failed), or unknown (the chart has no checks)
  3   kubeconfig could not be loaded / cluster unreachable
  4   cluster reachable but no tracebloc client found there`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --wait watches the backend's view; --seal interrogates the cluster.
			// Two different questions — refuse the ambiguous combination.
			if wait && seal {
				return &exitError{code: exitFailure, err: errors.New("--wait and --seal are separate modes — run them one at a time")}
			}
			// --timeout governs the --wait poll and the per-check budget under
			// --seal; accepting it alone would be a silent no-op, so reject it
			// rather than mislead. Same for the cluster-targeting flags, which
			// only the seal check (a cluster-side operation) consumes.
			if cmd.Flags().Changed("timeout") && !wait && !seal {
				return &exitError{code: exitFailure, err: errors.New("--timeout has no effect without --wait or --seal")}
			}
			for _, name := range []string{"kubeconfig", "context", "namespace"} {
				if cmd.Flags().Changed(name) && !seal {
					return &exitError{code: exitFailure, err: fmt.Errorf("--%s has no effect without --seal", name)}
				}
			}
			if seal {
				return runSealCheck(cmd.Context(), printerFor(cmd),
					cluster.KubeconfigOptions{Path: kubeconfigPath, Context: contextOverride, Namespace: nsOverride},
					timeout)
			}
			return runClientStatus(cmd.Context(), printerFor(cmd), wait, timeout)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "poll until tracebloc reports this client online")
	cmd.Flags().DurationVar(&timeout, "timeout", 120*time.Second,
		"with --wait, give up after this long; with --seal, the time budget per check")
	cmd.Flags().BoolVar(&seal, "seal", false,
		"verify this environment's protections: run the chart's conformance checks and report sealed / unsealed")
	addKubeconfigFlags(cmd, &kubeconfigPath, &contextOverride,
		"with --seal: "+kubeconfigFlagUsage,
		"with --seal: "+contextFlagUsage)
	addNamespaceFlag(cmd, &nsOverride, "with --seal: "+namespaceFlagUsage)
	return cmd
}

// clientStatusPollInterval is how often --wait re-checks the backend. A const,
// not a seam: tests inject through pollAfter (which ignores the duration and
// fires instantly), so the value never needs overriding.
const clientStatusPollInterval = 3 * time.Second

func runClientStatus(ctx context.Context, p *ui.Printer, wait bool, timeout time.Duration) error {
	client, cfg, err := authedClient()
	if err != nil {
		return &exitError{code: exitFailure, err: err}
	}
	active := cfg.Current().ActiveClientID
	if active == "" {
		return &exitError{code: exitFailure, err: errors.New(
			"no active client on this machine — run `tracebloc client create` (or re-run the installer) first")}
	}

	// One-shot: report the current state and exit 0 (informational).
	if !wait {
		st, found, lerr := lookupClientStatus(ctx, client, active)
		if lerr != nil {
			return &exitError{code: exitFailure, err: lerr}
		}
		if !found {
			return &exitError{code: exitFailure, err: fmt.Errorf(
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
			return &exitError{code: exitFailure, err: lerr}
		case errors.As(lerr, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden):
			// A revoked/expired/forbidden token (401/403) won't recover by waiting —
			// the client itself may be online. Fail fast, point at sign-in. Note 429
			// and 5xx stay transient (below): those DO recover on retry.
			return &exitError{code: exitFailure, err: errors.New(
				"tracebloc rejected your credentials while waiting — run `tracebloc login`, then retry")}
		case lerr != nil:
			lastErr = lerr // transient (5xx / 429 / network) — keep waiting, remember why
		case !found:
			// The active client isn't in the account (deleted / wrong account) — no
			// amount of waiting surfaces it. Fail fast, matching the one-shot path.
			return &exitError{code: exitFailure, err: fmt.Errorf(
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
				return &exitError{code: exitFailure, err: fmt.Errorf(
					"timed out after %s waiting for tracebloc to report this client online; "+
						"the last status check failed: %v", timeout, lastErr)}
			case lastState >= 0:
				return &exitError{code: exitFailure, err: fmt.Errorf(
					"timed out after %s waiting for tracebloc to report this client online (last state: %s). "+
						"Run `tracebloc doctor` to diagnose, or re-run the installer.", timeout, clientStateLabel(lastState))}
			default:
				return &exitError{code: exitFailure, err: fmt.Errorf(
					"timed out after %s before tracebloc could confirm this client — retry, "+
						"or run `tracebloc doctor`.", timeout)}
			}
		}
		wait := clientStatusPollInterval
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return &exitError{code: exitInterrupted} // Ctrl-C: exit quietly (no "Error: context canceled")
		case <-pollAfter(wait):
		}
	}
}

// lookupClientStatus fetches the active client directly and returns its backend
// status code. found=false means no such client (deleted, or signed into the
// wrong account). A lookup error is returned verbatim so --wait can treat it as
// transient and retry. Fetches the single client by id (GET /edge-device/{id}/)
// rather than listing the whole account — the home-screen heartbeat runs this
// under a ~1.2s budget, and paging every client blew it (cli#338).
func lookupClientStatus(ctx context.Context, client *api.Client, active string) (status int, found bool, err error) {
	id, err := strconv.Atoi(active)
	if err != nil {
		// A non-numeric active id can never match a backend client, so report it
		// as not-found — exactly what the old ListClients+match path did — rather
		// than a permanent error. A --wait loop fail-fasts on a missing client but
		// treats errors as transient, so returning an error here would make it
		// poll a permanent parse failure to the timeout (Bugbot: poll/retry loops
		// must fail-fast on non-transient errors).
		return 0, false, nil
	}
	c, err := client.GetClient(ctx, id)
	if err != nil {
		return 0, false, err
	}
	if c == nil {
		return 0, false, nil // 404 — no such client
	}
	return c.Status, true, nil
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
