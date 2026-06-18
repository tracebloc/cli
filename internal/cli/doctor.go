package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/doctor"
	"github.com/tracebloc/cli/internal/ui"
)

// newClusterDoctorCmd implements `tracebloc cluster doctor` — the sibling of
// `cluster info` that cluster.go's doc comment anticipated. Where `info`
// answers "is the CLI pointing at the right cluster?", `doctor` answers "is
// this running cluster healthy enough to run an experiment, and if not, what
// do I fix?" — a read-only, post-install health sweep with remedies
// (epic client-runtime#116, WS3).
//
// The three kubeconfig flags match `cluster info` exactly so muscle memory
// carries over; all are zero-value-safe.
func newClusterDoctorCmd() *cobra.Command {
	var (
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose a running tracebloc client cluster (✔/⚠/✖ health checks + remedies)",
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

	cmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	cmd.Flags().StringVar(&contextOverride, "context", "",
		"name of the kubeconfig context to use (default: kubeconfig's current-context)")
	cmd.Flags().StringVarP(&nsOverride, "namespace", "n", "",
		"namespace where the parent tracebloc/client release is installed (default: the context's namespace, or 'default')")

	return cmd
}

func runClusterDoctor(
	ctx context.Context,
	p *ui.Printer,
	kubeconfigPath, contextOverride, nsOverride string,
) error {
	p.Banner("tracebloc", "cluster doctor")

	resolved, err := cluster.Load(cluster.KubeconfigOptions{
		Path:      kubeconfigPath,
		Context:   contextOverride,
		Namespace: nsOverride,
	})
	if err != nil {
		// 3 = kubeconfig file/parse problem (same class as cluster info).
		return &exitError{code: 3, err: fmt.Errorf("loading kubeconfig: %w", err)}
	}

	cs, err := cluster.NewClientset(resolved)
	if err != nil {
		return &exitError{code: 3, err: err}
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
	switch doctor.Worst(results) {
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
		p.Successf("All checks passed — the cluster looks healthy.")
		return nil
	}
}
