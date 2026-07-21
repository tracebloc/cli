package cli

import (
	"context"

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
	p.Newline()

	binding := bindActiveClientNamespace(&opts)
	target, err := resolveClusterTarget(ctx, p, opts, binding, false)
	if err != nil {
		return binding.explain(err)
	}
	return renderResources(ctx, p, target)
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
	var machine resources.Machine
	nodeErr := error(nil)
	if nodes, lerr := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); lerr != nil {
		nodeErr = lerr
	} else {
		machine = resources.MachineCapacity(nodes.Items)
	}

	env := resources.JobsManagerEnv(ctx, cs, resolved.Namespace, release.ReleaseName)
	train := resources.ParseTraining(env)

	// The chart stamps GPU_REQUESTS/GPU_LIMITS=nvidia.com/gpu=1 as literal env on
	// every install — even CPU-only hosts — so ParseTraining reports a phantom
	// HasGPU=true. When we've confirmed the node exposes no GPU, drop it so the
	// per-run line doesn't advertise a GPU the machine can't provide (and doesn't
	// contradict the "gpu: none detected" detail below). Mirrors the set path's
	// phantom-GPU normalization (Bugbot #241). A node-read failure leaves it as-is
	// (we can't confirm absence — the capacity line already says "unavailable").
	if nodeErr == nil && len(machine.GPU) == 0 {
		train.HasGPU = false
	}

	if nodeErr != nil {
		p.Stat("Your secure environment is equipped with:", "unavailable")
		p.Hintf("     couldn't read capacity: %v", nodeErr)
	} else {
		p.Stat("Your secure environment is equipped with:", machineLine(machine))
	}
	// The per-run ceiling is cluster-wide — jobs-manager stamps it on EVERY
	// training run (there is no per-run override today). perRunSize is the same
	// "CPU · mem" string `resources set` shows; the label carries the "up to".
	p.Stat("A training run is allocated up to:", perRunSize(train))

	if p.Verbose() {
		p.Section("Details")
		p.Field("namespace", resolved.Namespace)
		p.Field("client", release.ReleaseName)
		if raw := firstNonEmptyEnv(env, "RESOURCE_LIMITS", "RESOURCE_REQUESTS"); raw != "" {
			p.Field("resource env", raw)
		} else {
			p.Field("resource env", "(unset — using chart default "+resources.DefaultTraining+")")
		}
		if nodeErr == nil && len(machine.GPU) == 0 {
			p.Field("gpu", "none detected")
		}
	}

	p.Newline()
	// Match the home screen's launcher resolution so the hint reads `tb …` on a
	// real install (where the `tb` alias exists) and `tracebloc …` otherwise.
	cmd := invokedName()
	if tbAliasAvailable() {
		cmd = binTB
	}
	p.Hintf("Do you want to change the allocation? Run `%s resources set` (guided walkthrough on a terminal).", cmd)
	return nil
}

// machineLine renders the machine-capacity value: "8 CPU · 32 GiB" (+ " · 1 GPU"
// when a device is present).
func machineLine(m resources.Machine) string {
	line := resources.FormatCPU(m.CPU) + " · " + resources.FormatMem(m.Mem)
	for name, qty := range m.GPU {
		line += " · " + resources.FormatGPU(name, qty)
	}
	return line
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
