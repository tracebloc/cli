package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/cluster"
)

// newClusterCmd wires the `tracebloc cluster` subtree. Today it has
// a single verb — `info` — which is the customer's "is the CLI
// pointing at the right cluster?" pre-flight before running
// `dataset push`. Future verbs (e.g. `cluster doctor` for
// diagnostics, `cluster contexts` for switching) hang off this
// parent in later phases.
func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Inspect the cluster the CLI is currently targeting",
		Long: `Commands for inspecting the Kubernetes cluster the CLI is
configured to talk to.

Use ` + "`cluster info`" + ` to verify which cluster, namespace, and parent
tracebloc release the next ` + "`dataset push`" + ` will target. Useful as a
pre-flight before doing anything destructive (e.g. ingesting into
the wrong cluster).`,
	}

	cmd.AddCommand(newClusterInfoCmd())
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
		ingestorSAName  string
		tokenExpiry     int64
	)

	cmd := &cobra.Command{
		Use:   "info",
		Short: "Show the cluster, namespace, parent release, and ingestor SA token state",
		Long: `Discovers the tracebloc parent client release in the configured
cluster + namespace and prints:

  • Which kubeconfig context the CLI used
  • The namespace it resolved to
  • The parent release name + chart version + appVersion
  • The jobs-manager Service the next dataset push would POST to
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
  4   cluster reachable but no tracebloc parent release found
  5   cluster reachable + release found but no usable SA token`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterInfo(
				cmd.Context(),
				cmd.OutOrStdout(),
				kubeconfigPath, contextOverride, nsOverride,
				ingestorSAName,
				tokenExpiry,
			)
		},
	}

	cmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	cmd.Flags().StringVar(&contextOverride, "context", "",
		"name of the kubeconfig context to use (default: kubeconfig's current-context)")
	cmd.Flags().StringVarP(&nsOverride, "namespace", "n", "",
		"namespace where the parent tracebloc/client release is installed (default: the context's namespace, or 'default')")
	cmd.Flags().StringVar(&ingestorSAName, "ingestor-sa", "",
		"override the ingestor ServiceAccount name (default: \"ingestor\", the chart default; "+
			"set this if you customized `ingestionAuthz.serviceAccountName` in the parent client chart)")
	cmd.Flags().Int64Var(&tokenExpiry, "token-expiry-seconds", 600,
		"requested SA token expiration in seconds (default 600 = 10 min; ignored for static-secret fallback)")

	return cmd
}

func runClusterInfo(
	ctx context.Context,
	out interface{ Write([]byte) (int, error) },
	kubeconfigPath, contextOverride, nsOverride string,
	ingestorSAOverride string,
	tokenExpiry int64,
) error {
	resolved, err := cluster.Load(cluster.KubeconfigOptions{
		Path:      kubeconfigPath,
		Context:   contextOverride,
		Namespace: nsOverride,
	})
	if err != nil {
		// Kubeconfig errors are exit-code-3 territory (file/parse
		// problem, same conceptual class as `ingest validate`'s
		// unreadable-input).
		return &exitError{code: 3, err: fmt.Errorf("loading kubeconfig: %w", err)}
	}

	cs, err := cluster.NewClientset(resolved)
	if err != nil {
		return &exitError{code: 3, err: err}
	}

	// Explicit-discard the writer errors: same rationale as
	// internal/cli/ingest.go — the exit code is the contract, and a
	// pipe-write failure shouldn't convert success into failure. If
	// the downstream consumer disappears mid-write we still finish
	// cleanly. errcheck wants this acknowledged.
	_, _ = fmt.Fprintf(out, "Kubeconfig:\n")
	_, _ = fmt.Fprintf(out, "  context:     %s\n", resolved.Context)
	_, _ = fmt.Fprintf(out, "  server:      %s\n", resolved.ServerURL)
	_, _ = fmt.Fprintf(out, "  namespace:   %s\n", resolved.Namespace)
	_, _ = fmt.Fprintln(out)

	// Discover the parent release.
	release, err := cluster.DiscoverParentRelease(ctx, cs, resolved.Namespace)
	if err != nil {
		// 4 = "cluster reachable, but no tracebloc release here."
		// Distinct from the kubeconfig error (3) so callers can
		// branch: 3 means "fix your kubeconfig", 4 means "install
		// the parent chart first".
		return &exitError{code: 4, err: err}
	}

	// Apply the SA-name override here. Discovery doesn't read the
	// name from the cluster (see #7); customers with a non-default
	// name pass --ingestor-sa.
	if ingestorSAOverride != "" {
		release.IngestorSAName = ingestorSAOverride
	}

	_, _ = fmt.Fprintf(out, "Parent release:\n")
	_, _ = fmt.Fprintf(out, "  name:          %s\n", release.ReleaseName)
	_, _ = fmt.Fprintf(out, "  chart version: %s\n", release.ChartVersion)
	_, _ = fmt.Fprintf(out, "  app version:   %s\n", release.AppVersion)
	_, _ = fmt.Fprintf(out, "  jobs-manager:  %s\n", release.JobsManagerService)
	_, _ = fmt.Fprintf(out, "  ingestor SA:   %s/%s\n", resolved.Namespace, release.IngestorSAName)
	digest := release.IngestorImageDigest
	if digest == "" {
		digest = "<not configured — admin must set images.ingestor.digest>"
	}
	_, _ = fmt.Fprintf(out, "  ingestor img:  %s\n", digest)
	_, _ = fmt.Fprintln(out)

	// Mint a token (or fall back). The audience is intentionally
	// nil today — jobs-manager's TokenReview accepts the default
	// API-server audience. A future jobs-manager audience plugs in
	// here.
	tok, err := cluster.MintIngestorToken(ctx, cs, resolved.Namespace, release.IngestorSAName, tokenExpiry, nil)
	if err != nil {
		// 5 = "release found but no usable token." Distinct from
		// 4 (no release) so customers can RBAC-debug separately
		// from install issues.
		return &exitError{code: 5, err: err}
	}

	hash := sha256.Sum256([]byte(tok.Token))
	_, _ = fmt.Fprintf(out, "Ingestor SA token:\n")
	_, _ = fmt.Fprintf(out, "  source:        %s\n", tok.Source)
	_, _ = fmt.Fprintf(out, "  sha256[:8]:    %s\n", hex.EncodeToString(hash[:8]))
	if tok.ExpirationSeconds > 0 {
		exp := time.Duration(tok.ExpirationSeconds) * time.Second
		_, _ = fmt.Fprintf(out, "  expires in:    ~%s (server may cap shorter)\n", exp)
	} else {
		_, _ = fmt.Fprintf(out, "  expires in:    never (static-secret fallback)\n")
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Ready for `tracebloc dataset push`.")
	return nil
}
