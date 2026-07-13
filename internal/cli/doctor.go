package cli

import (
	"context"
	"errors"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/doctor"
	"github.com/tracebloc/cli/internal/ui"
)

// newDoctorCmd builds the `doctor` command. The SAME command is registered two
// ways: as the top-level `tracebloc doctor` (visible — the home screen and its
// env-status lines point here), and, hidden, as `tracebloc cluster doctor` (its
// original path, kept working so existing docs, muscle memory, and scripts don't
// break). Pass hidden=true for the cluster-subtree alias. Both entry points
// share one RunE (runClusterDoctor), so there is a single diagnostic code path.
//
// It answers "is this running cluster healthy enough to run an experiment, and
// if not, what do I fix?" — a read-only, post-install health sweep with remedies
// (epic client-runtime#116, WS3). The three kubeconfig flags match `cluster
// info` exactly so muscle memory carries over; all are zero-value-safe.
func newDoctorCmd(hidden bool) *cobra.Command {
	var (
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
	)

	cmd := &cobra.Command{
		Use:    "doctor",
		Hidden: hidden,
		Short:  "Diagnose a running tracebloc client cluster (✔/⚠/✖ health checks + remedies)",
		Long: `Runs a read-only health sweep over the tracebloc client release in the
configured cluster + namespace and prints a ✔/⚠/✖ line per check with a
remedy for anything that isn't green:

  • Cluster reachable — the API answers and the client chart is installed
  • Pod health — nothing crash-looping or stuck Pending
  • Dataset volume — the shared PVC exists and is Bound
  • Proxy configuration — the in-cluster requests/egress proxy wiring
  • Backend egress — the tracebloc backend is reachable (from this machine)
  • Service Bus egress — the requests-proxy that brokers experiment egress is Ready

For a full redacted support bundle to send to tracebloc, use the installer's
` + "`./install-k8s.sh --diagnose`" + ` instead.

Exit codes:
  0   all checks passed (or warnings only)
  2   one or more checks failed
  3   kubeconfig could not be loaded`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterDoctor(
				cmd.Context(),
				printerFor(cmd),
				kubeconfigPath, contextOverride, nsOverride,
			)
		},
	}

	addKubeconfigFlags(cmd, &kubeconfigPath, &contextOverride, kubeconfigFlagUsage, contextFlagUsage)
	addNamespaceFlag(cmd, &nsOverride,
		"namespace where your tracebloc client is installed (default: the context's namespace, or 'default')")

	return cmd
}

func runClusterDoctor(
	ctx context.Context,
	p *ui.Printer,
	kubeconfigPath, contextOverride, nsOverride string,
) error {
	p.Banner("tracebloc", "doctor")

	// Auth / config checks run FIRST and don't need a cluster — so `doctor` can
	// diagnose a failed provision (bad/expired token, wrong env, no active
	// client) even before any cluster is reachable (RFC-0001 §8.5).
	authStatus := runAuthChecks(ctx, p)

	resolved, err := cluster.Load(cluster.KubeconfigOptions{
		Path:      kubeconfigPath,
		Context:   contextOverride,
		Namespace: nsOverride,
	})
	if err != nil {
		// 3 = kubeconfig file/parse problem (same class as cluster info). The
		// auth section above already ran; if IT also failed, escalate to 2 so
		// automation doesn't read a real auth failure as a kubeconfig-only one.
		p.Section("Cluster")
		p.Errorf("Kubeconfig — couldn't load it: %v", err)
		p.Hintf("     point --kubeconfig / --context at your cluster, or fix ~/.kube/config")
		return &exitError{code: kubeconfigExitCode(authStatus), err: nil}
	}

	cs, err := cluster.NewClientset(resolved)
	if err != nil {
		p.Section("Cluster")
		p.Errorf("Kubeconfig — %v", err)
		return &exitError{code: kubeconfigExitCode(authStatus), err: nil}
	}

	p.Section("Kubeconfig")
	p.Field("context", resolved.Context)
	p.Field("server", resolved.ServerURL)
	p.Field("namespace", resolved.Namespace)

	results := doctor.Run(ctx, cs, doctor.Options{Namespace: resolved.Namespace})

	p.Section("Checks")
	for _, r := range results {
		switch r.Status {
		case doctor.StatusOK:
			p.Successf("%s — %s", r.Name, r.Detail)
		case doctor.StatusWarn:
			p.Warnf("%s — %s", r.Name, r.Detail)
			if r.Remedy != "" {
				p.Hintf("     %s", r.Remedy)
			}
		case doctor.StatusFail:
			p.Errorf("%s — %s", r.Name, r.Detail)
			if r.Remedy != "" {
				p.Hintf("     %s", r.Remedy)
			}
		}
	}

	p.Newline()
	// Overall verdict folds in the auth section, so an auth ✖/⚠ counts even when
	// the cluster itself is healthy.
	switch worseStatus(authStatus, doctor.Worst(results)) {
	case doctor.StatusFail:
		p.Errorf("Problems found — fix the ✖ items above.")
		p.Hintf("For deeper triage, send tracebloc a support bundle: ./install-k8s.sh --diagnose")
		// Silent (err == nil): the per-check lines above already explained it,
		// so main() shouldn't print a redundant "Error:" line.
		return &exitError{code: 2, err: nil}
	case doctor.StatusWarn:
		p.Warnf("Completed with warnings — review the ⚠ items above.")
		return nil
	default:
		p.Successf("All checks passed — auth and cluster look healthy.")
		return nil
	}
}

// runAuthChecks reports on the CLI's own auth/config state (~/.tracebloc): are
// we signed in, to which env, is an active client selected, and does the backend
// still accept the token. It's the half of `cluster doctor` that diagnoses a
// failed *provision* rather than a sick cluster (RFC-0001 §8.5). Returns the
// worst status seen so the caller can fold it into the overall verdict.
func runAuthChecks(ctx context.Context, p *ui.Printer) doctor.Status {
	p.Section("Auth & config")

	cfg, err := config.Load()
	if err != nil {
		p.Errorf("Config — couldn't read the CLI config: %v", err)
		p.Hintf("     check ~/.tracebloc/config.json, or run `tracebloc login` to recreate it")
		return doctor.StatusFail
	}
	if !cfg.SignedIn() {
		p.Errorf("Sign-in — not signed in")
		p.Hintf("     run `tracebloc login` (add --env dev|stg|prod for a non-prod backend)")
		return doctor.StatusFail
	}

	env := cfg.CurrentEnv
	prof := cfg.Current()
	if prof.Email != "" {
		p.Successf("Sign-in — signed in to %s as %s", env, prof.Email)
	} else {
		p.Successf("Sign-in — signed in to %s", env)
	}

	worst := doctor.StatusOK
	if prof.ActiveClientID == "" {
		p.Warnf("Active client — none selected for %s", env)
		p.Hintf("     run `tracebloc client create` (or re-run the installer) to set the client this machine enrolls as")
		worst = doctor.StatusWarn
	} else {
		p.Successf("Active client — %s", prof.ActiveClientID)
	}

	// Live token check. Best-effort: an explicit 401 is a failure (expired /
	// revoked → must re-login); a network/proxy error is only a warning, since
	// we can't conclude the token itself is bad.
	p.Detailf("verifying the token against %s …", api.BaseURL(env))
	client := newAPIClient(env)
	client.Token = prof.Token
	if _, werr := client.WhoAmI(ctx); werr != nil {
		var ae *api.APIError
		var ue *api.UpgradeRequiredError
		switch {
		case errors.As(werr, &ae) && ae.StatusCode == http.StatusUnauthorized:
			p.Errorf("Backend auth — %s rejected the token (401)", api.BaseURL(env))
			p.Hintf("     your session expired or was revoked — run `tracebloc login`")
			return doctor.StatusFail
		case errors.As(werr, &ue):
			// 426: the server enforces a newer CLI. That's a hard, actionable
			// failure ("upgrade"), not a transient "couldn't verify" warning.
			p.Errorf("Backend auth — this CLI is too old for %s (HTTP 426)", api.BaseURL(env))
			p.Hintf("     %s", ue.Error())
			return doctor.StatusFail
		default:
			p.Warnf("Backend auth — couldn't verify the token: %v", werr)
			p.Hintf("     the backend may be unreachable from here — check your network / HTTP(S)_PROXY")
			return worseStatus(worst, doctor.StatusWarn)
		}
	}
	p.Successf("Backend auth — token valid at %s", api.BaseURL(env))
	return worst
}

// worseStatus returns the more severe of two doctor statuses (Fail > Warn > OK).
func worseStatus(a, b doctor.Status) doctor.Status {
	if a == doctor.StatusFail || b == doctor.StatusFail {
		return doctor.StatusFail
	}
	if a == doctor.StatusWarn || b == doctor.StatusWarn {
		return doctor.StatusWarn
	}
	return doctor.StatusOK
}

// kubeconfigExitCode is 3 ("kubeconfig could not be loaded") unless the auth
// section also failed — then it escalates to 2 ("a check failed"), so a bad
// token isn't masked behind a kubeconfig-only exit code (Bugbot).
func kubeconfigExitCode(authStatus doctor.Status) int {
	if authStatus == doctor.StatusFail {
		return 2
	}
	return 3
}
