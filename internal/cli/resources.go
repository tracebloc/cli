package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"runtime"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/resources"
	"github.com/tracebloc/cli/internal/ui"
)

// newResourcesCmd wires the top-level `tracebloc resources` command (cli#143):
// the one-knob view of "how much of this machine tracebloc may use." Bare
// `tracebloc resources` SHOWS the current picture (P1) — machine capacity and
// the ceiling a single training run may use — with no mutations.
//
// It is deliberately top-level, NOT under `cluster`: the user concept is about
// THIS MACHINE, not Kubernetes internals, and the design locked "top-level
// command" (issue #143, approved 2026-07-06).
//
// Raising the allowance (`set --cores/--memory`, `set max`) is built in
// newResourcesSetCmd (cli#143 P2). The macOS Docker-VM auto-raise stays deferred
// (P3): on macOS the fit-check gives an honest "raise it in Docker Desktop"
// message rather than mutating the VM.
func newResourcesCmd() *cobra.Command {
	var (
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
	)

	cmd := &cobra.Command{
		Use:   "resources",
		Short: "Show how much of this machine tracebloc may use",
		Long: `Shows, in plain terms, how much of this machine tracebloc may use:

  • Your secure environment — the CPU and memory it can schedule
  • Each training run — the per-run ceiling every run may use (cluster-wide)

No Kubernetes concepts, no YAML — one number for your environment and one for
each training run's share of it.

Raise the share with ` + "`tracebloc resources set`" + `. Run with --verbose for the
per-node breakdown and the raw values.

Exit codes:
  0   shown
  3   kubeconfig could not be loaded / cluster unreachable
  4   cluster reachable but no tracebloc client found here`,
		// Bare `tracebloc resources` SHOWS. Raising the share lives in the `set`
		// subcommand (newResourcesSetCmd). NoArgs so a stray token (`resources
		// bogus`) gets cobra's "unknown command", not a silently-ignored SHOW.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := cluster.KubeconfigOptions{
				Path:      kubeconfigPath,
				Context:   contextOverride,
				Namespace: nsOverride,
			}
			return runResourcesShow(cmd.Context(), printerFor(cmd), opts)
		},
	}

	cmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	cmd.Flags().StringVar(&contextOverride, "context", "",
		"name of the kubeconfig context to use (default: kubeconfig's current-context)")
	cmd.Flags().StringVarP(&nsOverride, "namespace", "n", "",
		"namespace where your tracebloc client is installed (default: the context's namespace, or 'default')")

	// `set` (P2): raise how much of this machine a training run may use.
	cmd.AddCommand(newResourcesSetCmd())

	return cmd
}

// runResourcesShow renders the read-only allocation view. It resolves the
// cluster exactly like the data commands (active-client binding + cluster-wide
// fallback scan, exit 3 for kubeconfig / 4 for no-release), reads the machine's
// capacity from node allocatable, and reads the per-run training ceiling from
// the jobs-manager env — the same source `cluster doctor` parses, so the two
// never disagree.
func runResourcesShow(ctx context.Context, p *ui.Printer, opts cluster.KubeconfigOptions) error {
	// No banner: the user typed `resources` — don't echo the tool name, a rule,
	// and "resources" back. Start straight with the numbers.
	binding := bindActiveClientNamespace(&opts)
	target, err := resolveClusterTarget(ctx, p, opts, binding, false)
	if err != nil {
		return binding.explain(err)
	}
	if err := renderResources(ctx, p, target); err != nil {
		return err
	}

	// The one thing the CLI can change is the per-training ceiling. On a terminal,
	// offer it inline (→ the guided wizard); off a terminal, point at the
	// scriptable command instead of prompting into the void.
	p.Newline()
	if isInteractiveTTY() {
		pr := surveyPrompter{}
		ok, cerr := pr.Confirm("Change how much each training run gets?", false)
		if cerr != nil {
			// Ctrl-C / abort at the prompt is a choice, not a failure (exit 0);
			// a genuine prompt error must surface, not be swallowed.
			if errors.Is(cerr, errInteractiveCancelled) {
				return nil
			}
			return cerr
		}
		if !ok {
			return nil
		}
		return runResourcesSet(ctx, p, pr, opts, setReq{})
	}
	p.Hintf("To change a training run's allocation: %s resources set --cores N --memory NGi", launcherName())
	return nil
}

// renderResources is the post-resolution half of `resources show`: given an
// already-resolved cluster target, it reads capacity + the training ceiling and
// prints the view. Split out so it's exercisable with a fake clientset without
// going through the real kubeconfig-load path (same seam ingestion_run_test uses).
func renderResources(ctx context.Context, p *ui.Printer, target *clusterTarget) error {
	resolved, cs, release := target.Resolved, target.Clientset, target.Release

	// Machine capacity: sum of Ready nodes' allocatable. A node-list failure is
	// not fatal to the whole view — we still show the training ceiling — but it
	// is called out so the machine line isn't silently zero.
	var envCap resources.Machine
	nodeErr := error(nil)
	nodeCount := 0
	if nodes, lerr := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); lerr != nil {
		nodeErr = lerr
	} else {
		envCap = resources.MachineCapacity(nodes.Items)
		// Count only Ready nodes — the same set MachineCapacity sums, so the
		// "(across N nodes)" suffix can't disagree with the capacity it annotates.
		nodeCount = resources.ReadyNodes(nodes.Items)
	}

	env := resources.JobsManagerEnv(ctx, cs, resolved.Namespace, release.ReleaseName)
	train := resources.ParseTraining(env)

	// Phantom-GPU: the chart stamps GPU_*=nvidia.com/gpu=1 on every install, even
	// CPU-only hosts, so drop it when the node exposes no GPU (mirrors the set
	// path's normalization, Bugbot #241). A node-read failure leaves it as-is.
	if nodeErr == nil && len(envCap.GPU) == 0 {
		train.HasGPU = false
	}

	// Layer 1 — Your machine: shown only on a LOCAL install (cluster API is
	// loopback), where the cluster runs on THIS host and its capacity is a slice
	// of the machine. On a remote cluster the operator's laptop is irrelevant, so
	// the line is dropped.
	local := isLoopbackServer(resolved.ServerURL)
	if local && nodeErr == nil {
		p.Stat("Your machine has:", resources.DetectHost().Line(envCap.GPU))
	}

	// Layer 2 — Your secure environment: what the cluster can schedule, with a
	// per-OS / remote pointer to where its size is actually changed (never a
	// dead-end CLI prompt — the CLI can't resize Docker or a node pool).
	if nodeErr != nil {
		// A node-list failure isn't necessarily unreachable (it can be an RBAC
		// restriction on a reachable cluster) — say what we know and let doctor
		// diagnose which.
		p.Stat("Your secure environment:", "couldn't read its capacity — run `"+launcherName()+" doctor`")
	} else {
		val := envCap.Line()
		if !local && nodeCount > 1 {
			val += fmt.Sprintf("   (across %d nodes)", nodeCount)
		}
		p.Stat("Your secure environment has:", val)
		p.Hintf("     %s", envChangeHint(local))
	}

	// Layer 3 — Each training run: the one dial the CLI owns. Cluster-wide —
	// jobs-manager stamps it on EVERY training run (no per-run override today).
	p.Stat("Each training run may use up to:", perRunSize(train))

	if p.Verbose() {
		p.Section("Details")
		p.Field("namespace", resolved.Namespace)
		p.Field("client", release.ReleaseName)
		if raw := firstNonEmptyEnv(env, "RESOURCE_LIMITS", "RESOURCE_REQUESTS"); raw != "" {
			p.Field("resource env", raw)
		} else {
			p.Field("resource env", "(unset — using chart default "+resources.DefaultTraining+")")
		}
		if nodeErr == nil && len(envCap.GPU) == 0 {
			p.Field("gpu", "none detected")
		}
	}
	return nil
}

// launcherName is the command to print in hints — `tb` when the alias is
// installed (a real install), else the invoked name. Mirrors the home screen so
// hints match what the user actually runs.
func launcherName() string {
	if tbAliasAvailable() {
		return binTB
	}
	return invokedName()
}

// isLoopbackServer reports whether the cluster API server is on this machine
// (127.0.0.1 / localhost / ::1) — a local k3d/kind/Docker-Desktop install, where
// the cluster's capacity is a slice of THIS host.
func isLoopbackServer(serverURL string) bool {
	if serverURL == "" {
		return false
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// envChangeHint is the per-setup pointer to where the environment's size is
// actually changed — the CLI can't mutate Docker Desktop or a node pool itself.
func envChangeHint(local bool) string {
	if !local {
		return "to give it more, resize your cluster's node pool"
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		return "to give it more, open Docker Desktop → Resources"
	case "linux":
		return "to reserve part of the machine for other work, cap the environment (docs TBD)"
	default:
		return "to change how much it can use, adjust your container runtime's limits"
	}
}

// firstNonEmptyEnv returns the first present, non-empty value among keys.
func firstNonEmptyEnv(env map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := env[k]; v != "" {
			return v
		}
	}
	return ""
}
