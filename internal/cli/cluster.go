package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/ui"
)

// newClusterCmd wires the `tracebloc cluster` subtree:
//   - `info`   — the customer's "is the CLI pointing at the right
//     cluster?" pre-flight before running `dataset push`.
//   - `doctor` — a read-only health sweep of the running release with
//     ✔/⚠/✖ checks + remedies (epic client-runtime#116, WS3).
//
// Future verbs (e.g. `cluster contexts` for switching) hang off this
// parent in later phases.
func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Inspect the cluster the CLI is currently targeting",
		Long: `Commands for inspecting the Kubernetes cluster the CLI is
configured to talk to.

Use ` + "`cluster info`" + ` to verify which cluster, namespace, and
client the next ` + "`data ingest`" + ` will target. Useful as a
pre-flight before doing anything destructive (e.g. ingesting into
the wrong cluster).`,
		// Bare `tracebloc cluster` prints help; a mistyped subcommand errors with a
		// suggestion instead of silently exiting 0 (#75).
		RunE:                       runGroup,
		SuggestionsMinimumDistance: 2,
	}

	cmd.AddCommand(newClusterInfoCmd())
	// `cluster doctor` stays as a hidden alias of the top-level `doctor` (both
	// share one RunE — see newDoctorCmd) so existing docs / muscle memory keep
	// working while the home screen points at the shorter top-level command.
	cmd.AddCommand(newDoctorCmd(true))
	return cmd
}

// newClusterInfoCmd implements `tracebloc cluster info`. Discovers
// the parent client release in the configured namespace, mints (or
// looks up) an ingestor SA token, and prints a single-screen
// summary the customer can sanity-check before running a real
// ingestion.
//
// Three kubeconfig flags follow kubectl conventions:
//
//	--kubeconfig=PATH   override KUBECONFIG / ~/.kube/config
//	--context=NAME      override the kubeconfig's current-context
//	--namespace=NAME    override the context's default namespace
//
// All three are zero-value-safe — running `tracebloc cluster info`
// with no flags uses the customer's normal kubectl defaults.
func newClusterInfoCmd() *cobra.Command {
	var (
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
		tokenExpiry     int64
	)

	cmd := &cobra.Command{
		Use:   "info",
		Short: "Show the cluster, namespace, client install, and ingestor token state",
		Long: `Discovers the tracebloc client installed in the configured
cluster + namespace and prints:

  • Which kubeconfig context the CLI used
  • The namespace it resolved to
  • The client's release name + chart version + appVersion
  • The jobs-manager Service the next data ingest would POST to
  • The ingestor ServiceAccount the post-install hook would auth as
  • The cluster's configured INGESTOR_IMAGE_DIGEST default
  • Whether the user's kubeconfig can mint short-lived SA tokens
    via TokenRequest, or has to fall back to a static
    service-account-token Secret

The actual token bytes are never printed; the diagnostic shows
SHA256(token)[:8] so the customer can verify "yes, that's the
token I expect" without leaking it to terminal scrollback.

Exit codes:
  0   cluster discovered + token mintable; CLI is ready
  4   cluster reachable but no tracebloc client found
  5   cluster reachable + release found but no usable SA token`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterInfo(
				cmd.Context(),
				printerFor(cmd),
				kubeconfigPath, contextOverride, nsOverride,
				tokenExpiry,
			)
		},
	}

	addKubeconfigFlags(cmd, &kubeconfigPath, &contextOverride, kubeconfigFlagUsage, contextFlagUsage)
	addNamespaceFlag(cmd, &nsOverride,
		"namespace where your tracebloc client is installed (default: the context's namespace, or 'default')")
	cmd.Flags().Int64Var(&tokenExpiry, "token-expiry-seconds", 600,
		"requested SA token expiration in seconds (default 600 = 10 min; ignored for static-secret fallback)")

	return cmd
}

func runClusterInfo(
	ctx context.Context,
	p *ui.Printer,
	kubeconfigPath, contextOverride, nsOverride string,
	tokenExpiry int64,
) error {
	// Bind the active client's namespace exactly like the data commands do,
	// so this pre-flight targets what `data ingest` will actually target —
	// and so the multi-client "set your active client" remediation works
	// here too, not just on the data path.
	opts := cluster.KubeconfigOptions{
		Path:      kubeconfigPath,
		Context:   contextOverride,
		Namespace: nsOverride,
	}
	binding := bindActiveClientNamespace(&opts)
	// Through the loadClusterFn/newClientsetFn seams (not cluster.Load /
	// cluster.NewClientset directly) so the post-discovery path — token minting,
	// the expiry switch, the install-print block — is testable without a real
	// kubeconfig or apiserver, exactly like resolveClusterTarget.
	resolved, err := loadClusterFn(opts)
	if err != nil {
		// Kubeconfig errors are exit-code-3 territory (file/parse
		// problem, same conceptual class as `ingest validate`'s
		// unreadable-input).
		return &exitError{code: exitLocalEnv, err: fmt.Errorf("loading kubeconfig: %w", err)}
	}

	cs, err := newClientsetFn(resolved)
	if err != nil {
		return &exitError{code: exitLocalEnv, err: err}
	}

	p.Section("Kubeconfig")
	p.Field("context", resolved.Context)
	p.Field("server", resolved.ServerURL)

	// Discover the client's release — with the cluster-wide fallback scan
	// when the namespace is just the kubeconfig default, so diagnostics find
	// the client in its slug namespace instead of dead-ending on "default".
	release, nsUsed, err := discoverRelease(ctx, p, cs, resolved.Namespace, binding.allowScan())
	if err != nil {
		// 4 = "cluster reachable, but no tracebloc client here."
		// Distinct from the kubeconfig error (3) so callers can
		// branch: 3 means "fix your kubeconfig", 4 means "no client
		// installed on this cluster". A binding miss gets the §7.3
		// "runs elsewhere" explanation, same as the data commands.
		if errors.Is(err, cluster.ErrNoParentRelease) {
			return binding.explain(&exitError{code: exitNoWorkspace, err: &noParentReleaseError{err}})
		}
		return &exitError{code: exitNoWorkspace, err: err}
	}
	resolved.Namespace = nsUsed
	// Printed after discovery so it reflects the namespace the scan actually
	// retargeted to — this pre-flight's whole job is to report what the next
	// `data ingest` will target, so it must not show the pre-scan default.
	p.Field("namespace", resolved.Namespace)

	// release.IngestorSAName is discovered from the ingestionAuthz ConfigMap by
	// DiscoverParentRelease (#7) — no --ingestor-sa override needed.

	p.Section("Client install")
	p.Field("name", release.ReleaseName)
	p.Field("chart version", release.ChartVersion)
	p.Field("app version", release.AppVersion)
	p.Field("jobs-manager", release.JobsManagerService)
	p.Field("ingestor SA", fmt.Sprintf("%s/%s", resolved.Namespace, release.IngestorSAName))
	digest := release.IngestorImageDigest
	if digest == "" {
		digest = "<not configured — admin must set images.ingestor.digest>"
	}
	p.Field("ingestor img", digest)

	// Mint a token (or fall back). The audience is intentionally
	// nil today — jobs-manager's TokenReview accepts the default
	// API-server audience. A future jobs-manager audience plugs in
	// here.
	tok, err := cluster.MintIngestorToken(ctx, cs, resolved.Namespace, release.IngestorSAName, tokenExpiry, nil)
	if err != nil {
		// 5 = "release found but no usable token." Distinct from
		// 4 (no release) so customers can RBAC-debug separately
		// from install issues.
		return &exitError{code: exitAuth, err: err}
	}

	hash := sha256.Sum256([]byte(tok.Token))
	p.Section("Ingestor SA token")
	p.Field("source", tok.Source.String())
	p.Field("sha256[:8]", hex.EncodeToString(hash[:8]))
	switch {
	case !tok.ExpiresAt.IsZero():
		// The server's authoritative, policy-capped expiry (TokenRequest path).
		// "~" hedges the client/server clock skew in time.Until (#4).
		remaining := time.Until(tok.ExpiresAt).Round(time.Second)
		p.Field("expires in", fmt.Sprintf("~%s", remaining))
	case tok.ExpirationSeconds > 0:
		// No server timestamp — fall back to the requested lifetime.
		exp := time.Duration(tok.ExpirationSeconds) * time.Second
		p.Field("expires in", fmt.Sprintf("~%s (requested; server may cap shorter)", exp))
	default:
		p.Field("expires in", "never (static-secret fallback)")
	}

	p.Newline()
	p.Successf("Ready for `tracebloc data ingest`.")
	return nil
}
