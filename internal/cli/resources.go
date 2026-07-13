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
// Raising the allowance (`set --cpu/--memory`, `set max`) and the macOS VM raise
// are approved later phases (P2/P3) — see newResourcesSetCmd and the deferral
// note in runResourcesSetDeferred.
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

  • This machine   — the CPU and memory the cluster can schedule
  • tracebloc uses — the ceiling a single training run may use right now

No Kubernetes concepts, no YAML — one number for the machine and one for
tracebloc's share of it.

Raising the share (` + "`tracebloc resources set`" + `) is a later phase; today it's
set when this machine is first connected. Run with --verbose for the
per-node breakdown and the raw values.

Exit codes:
  0   shown
  3   kubeconfig could not be loaded / cluster unreachable
  4   cluster reachable but no tracebloc client found here`,
		// Bare `tracebloc resources` SHOWS (P1). Raising the share lives in the
		// `set` subcommand, whose flags + optional `max` positional parse cleanly
		// and reach an honest deferral (P2 not built) — see newResourcesSetCmd.
		// NoArgs so a stray token (`resources bogus`) gets cobra's "unknown
		// command", not a silently-ignored SHOW.
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

	// `set` (P2, deferred): wired now so the approved invocation shape parses and
	// reaches the honest deferral instead of a cobra flag error.
	cmd.AddCommand(newResourcesSetCmd())

	return cmd
}

// newResourcesSetCmd wires `tracebloc resources set` — the approved-but-unbuilt
// P2 that RAISES how much of this machine tracebloc may use. Two forms, both
// locked in the #143 design:
//
//	tracebloc resources set --cpu 4 --memory 16Gi   # explicit per-run ceiling
//	tracebloc resources set max                     # give a run the whole machine
//
// The `--cpu`/`--memory` flags and the optional `max` positional are registered
// now for one reason: so these invocations PARSE cleanly and reach the honest
// exit-1 deferral in runResourcesSetDeferred, instead of dying on cobra's
// "unknown flag: --cpu". The shape matches the approved design so P2 slots in
// behind it once the Helm-values persistence path lands. It mutates nothing.
func newResourcesSetCmd() *cobra.Command {
	setCmd := &cobra.Command{
		Use:   "set [max]",
		Short: "Raise how much of this machine tracebloc may use (coming soon)",
		Long: `Raise the per-training-run ceiling — how much of this machine a single
training run may use.

  tracebloc resources set --cpu 4 --memory 16Gi   set an explicit ceiling
  tracebloc resources set max                     let a run use the whole machine

This is an approved but not-yet-built phase. Today the ceiling is set when this
machine is first connected; run ` + "`tracebloc resources`" + ` to see it.`,
		// Optional single positional; when present it can only be `max` (the one
		// non-flag form in the design). Both guards run so `set max extra` and
		// `set bogus` are rejected rather than silently accepted.
		ValidArgs: []string{"max"},
		Args:      cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runResourcesSetDeferred(printerFor(cmd))
		},
	}
	// Registered (unbound) purely so parsing succeeds; P2 will read them.
	setCmd.Flags().String("cpu", "", "per-run CPU ceiling (e.g. 4 or 500m)")
	setCmd.Flags().String("memory", "", "per-run memory ceiling (e.g. 16Gi)")
	return setCmd
}

// runResourcesShow renders the read-only allocation view. It resolves the
// cluster exactly like the data commands (active-client binding + cluster-wide
// fallback scan, exit 3 for kubeconfig / 4 for no-release), reads the machine's
// capacity from node allocatable, and reads the per-run training ceiling from
// the jobs-manager env — the same source `cluster doctor` parses, so the two
// never disagree.
func runResourcesShow(ctx context.Context, p *ui.Printer, opts cluster.KubeconfigOptions) error {
	p.Banner("tracebloc", "machine resources")

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

	p.Section("This machine")
	if nodeErr != nil {
		p.Field("capacity", "unavailable")
		p.Hintf("     couldn't read node capacity: %v", nodeErr)
	} else {
		p.Field("capacity", machineLine(machine))
	}

	p.Section("tracebloc uses")
	p.Field("per training run", trainingLine(train))

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
	p.Hintf("Raising tracebloc's share (`tracebloc resources set`) is coming; today it's set when this machine is connected.")
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

// trainingLine renders the per-run ceiling: "up to 2 CPU · 8 GiB" (+ GPU).
func trainingLine(t resources.Training) string {
	line := "up to " + resources.FormatCPU(t.CPU) + " · " + resources.FormatMem(t.Mem)
	if t.HasGPU {
		line += " · " + resources.FormatGPU(t.GPUName, t.GPU)
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

// runResourcesSetDeferred prints an honest "not in this build" message for the
// approved-but-unbuilt P2 (`set --cpu/--memory`, `set max`). Building it safely
// needs a persistence path the shipped groundwork doesn't yet re-expose to the
// CLI: the value must be written to Helm values (a `kubectl set env` is reverted
// by the hourly auto-upgrade CronJob), and `helm upgrade` needs the chart
// reference the installer resolves via TRACEBLOC_HELM_REPO_NAME / a dev path —
// not recoverable from `helm list` alone. Rather than shell Helm blindly at a
// customer's live training cluster, `set` is deferred to its own change.
//
// Both `set` forms funnel here (the positional-vs-flags distinction is P2's to
// consume), so the message names the command, not the specific verb.
func runResourcesSetDeferred(p *ui.Printer) error {
	p.Banner("tracebloc", "machine resources")
	p.Errorf("`tracebloc resources set` isn't supported in this build yet.")
	p.Hintf("Today, how much of this machine tracebloc may use is set when the machine is first connected.")
	p.Hintf("Run `tracebloc resources` to see the current allocation.")
	// Silent (err == nil): the ✖ + hints above already explained it, so main()
	// must not print a redundant "Error:" line (same contract as cluster doctor).
	return &exitError{code: 1, err: nil}
}
