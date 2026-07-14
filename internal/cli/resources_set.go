package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/helm"
	"github.com/tracebloc/cli/internal/resources"
	"github.com/tracebloc/cli/internal/ui"
)

// chartPathOverride returns the dev-mode local chart path (TRACEBLOC_CHART_PATH),
// mirroring the installer's _resolve_chart_ref: when set, `helm upgrade` uses the
// local chart and skips the remote repo add/update. Empty in production.
func chartPathOverride() string { return strings.TrimSpace(os.Getenv("TRACEBLOC_CHART_PATH")) }

// setReq is the fully-parsed `resources set` request, decoupled from cobra so the
// whole flow is unit-testable without a command tree. The *Set booleans record
// whether the user actually passed each dimension: `set --cores 4` changes CPU
// only and KEEPS the current memory/GPU (Decision — "allow setting just one
// dimension"), which a bare value can't express.
type setReq struct {
	cores    string
	coresSet bool
	memory   string
	memSet   bool
	gpus     int
	gpusSet  bool
	max      bool // the `max` positional: give a run the whole machine (− overhead)
	yes      bool
	dryRun   bool
}

// newResourcesSetCmd wires `tracebloc resources set` — raising how much of this
// machine a single training run may use (cli#143 P2). Interactive WIZARD by
// default on a terminal; flags for scripting / non-TTY.
func newResourcesSetCmd() *cobra.Command {
	var (
		req             setReq
		kubeconfigPath  string
		contextOverride string
		nsOverride      string
	)

	setCmd := &cobra.Command{
		Use:   "set [max]",
		Short: "Raise how much of this machine tracebloc may use",
		Long: `Raise the per-training-run ceiling — how much of this machine a single
training run may use.

Run it on a terminal with no flags for a guided walkthrough:

  tracebloc resources set

Or set it directly (for scripts / non-interactive shells):

  tracebloc resources set --cores 4 --memory 16Gi   an explicit per-run ceiling
  tracebloc resources set --cores 4                 change CPU only, keep the rest
  tracebloc resources set max                       let a run use the whole machine

The number you set is what ONE training run may use. tracebloc keeps a small fixed
amount (about 1 core and 3 GiB) for itself on top — you never have to subtract it.
The new ceiling applies to your NEXT training run; a run already going keeps its
size.

Exit codes:
  0   applied (or nothing to change)
  2   the requested size doesn't fit this machine / bad input
  3   kubeconfig could not be loaded / cluster unreachable
  4   cluster reachable but no tracebloc client found here`,
		ValidArgs: []string{"max"},
		Args:      cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.max = len(args) == 1 && args[0] == "max"
			req.coresSet = cmd.Flags().Changed("cores") || cmd.Flags().Changed("cpu")
			req.memSet = cmd.Flags().Changed("memory")
			req.gpusSet = cmd.Flags().Changed("gpus")

			// A terminal (and no explicit value flags / max) => guided wizard.
			var pr prompter
			if isInteractiveTTY() {
				pr = surveyPrompter{}
			}
			opts := cluster.KubeconfigOptions{
				Path:      kubeconfigPath,
				Context:   contextOverride,
				Namespace: nsOverride,
			}
			return runResourcesSet(cmd.Context(), printerFor(cmd), pr, opts, req)
		},
	}

	setCmd.Flags().StringVar(&req.cores, "cores", "",
		"CPU cores one training run may use (e.g. 4)")
	// --cpu is the hidden back-compat alias for --cores (the SHOW stub advertised
	// it); --cores is the self-documenting name we steer users to.
	setCmd.Flags().StringVar(&req.cores, "cpu", "", "alias for --cores")
	_ = setCmd.Flags().MarkHidden("cpu")
	setCmd.Flags().StringVar(&req.memory, "memory", "",
		"memory one training run may use (e.g. 16 or 16Gi — the number is GiB)")
	setCmd.Flags().IntVar(&req.gpus, "gpus", 0,
		"whole GPUs one training run may use (only on a GPU machine)")
	setCmd.Flags().BoolVar(&req.yes, "yes", false,
		"skip the confirmation prompt (for automation)")
	setCmd.Flags().BoolVar(&req.dryRun, "dry-run", false,
		"show exactly what would change and apply nothing")

	setCmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	setCmd.Flags().StringVar(&contextOverride, "context", "",
		"name of the kubeconfig context to use (default: kubeconfig's current-context)")
	setCmd.Flags().StringVarP(&nsOverride, "namespace", "n", "",
		"namespace where your tracebloc client is installed (default: the context's namespace, or 'default')")

	return setCmd
}

// runResourcesSet resolves the cluster (exit 3/4, exactly like SHOW) and hands off
// to applyResourcesSet. Split so the mutation logic is exercisable against a fake
// clientset + fake helm Runner without the real kubeconfig path (the seam
// renderResources / ingestion_run_test already use).
func runResourcesSet(ctx context.Context, p *ui.Printer, pr prompter, opts cluster.KubeconfigOptions, req setReq) error {
	p.Banner("tracebloc", "machine resources")

	// Pure request checks first — no cluster needed, so a bad invocation fails
	// fast with exit 2 (never touching a live cluster).
	if err := validateRequestShape(req, pr != nil); err != nil {
		return err
	}

	binding := bindActiveClientNamespace(&opts)
	target, err := resolveClusterTarget(ctx, p, opts, binding, false)
	if err != nil {
		return binding.explain(err)
	}
	return applyResourcesSet(ctx, p, pr, target, opts, req)
}

// validateRequestShape rejects the cluster-independent bad invocations up front:
// `max` combined with explicit value flags, and an empty `set` with no way to ask
// (no flags, no `max`, no terminal). exit 2, human message, no cluster contact.
func validateRequestShape(req setReq, interactive bool) error {
	if req.max && (req.coresSet || req.memSet || req.gpusSet) {
		return validationError("`max` means the whole machine — don't combine it with --cores/--memory/--gpus. Use one or the other.")
	}
	anyFlag := req.coresSet || req.memSet || req.gpusSet
	if !req.max && !anyFlag && !interactive {
		return validationError("nothing to set — pass --cores/--memory/--gpus (or `max`), or run this on a terminal for a guided walkthrough.")
	}
	return nil
}

// applyResourcesSet is the post-resolution half: validate the request, size the
// new per-run ceiling (from flags, `max`, or the wizard), fit-check it against the
// largest single Ready node (with the platform overhead as margin), then persist
// it via `helm upgrade` — confirming first unless --yes. It mutates nothing until
// every check has passed (Decision — fit-check before apply; exit WITHOUT
// mutating on failure).
func applyResourcesSet(ctx context.Context, p *ui.Printer, pr prompter, target *clusterTarget, opts cluster.KubeconfigOptions, req setReq) error {
	// Cluster-independent request checks already ran in runResourcesSet; direct
	// callers (tests) may re-run them cheaply — they're idempotent.
	if err := validateRequestShape(req, pr != nil); err != nil {
		return err
	}

	// (1) The one node a run can land on. `set` sizes and fits against the largest
	//     single Ready node (a pod gets all its resources from ONE node), never a
	//     sum across nodes — mirrors `cluster doctor`'s node-fit.
	nodes, nerr := target.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if nerr != nil {
		return &exitError{code: exitLocalEnv, err: fmt.Errorf("couldn't read this machine's capacity: %w", nerr)}
	}
	node, ok := resources.LargestReadyNode(nodes.Items)
	if !ok {
		return &exitError{code: exitLocalEnv, err: fmt.Errorf("no Ready node on this machine to size a training run against")}
	}
	machineGPUName, machineGPUCount, machineHasGPU := resources.MachineGPU(node)

	// (2) Current per-run ceiling (what a run uses today), for the wizard header,
	//     single-dimension keeps, and the no-op skip.
	env := resources.JobsManagerEnv(ctx, target.Clientset, target.Resolved.Namespace, target.Release.ReleaseName)
	current := resources.ParseTraining(env)
	// The chart template stamps a DEFAULT GPU_REQUESTS ("nvidia.com/gpu=1") on
	// every install — even CPU-only boxes — so ParseTraining reports HasGPU=true
	// there. On a machine with no GPU the effective GPU is none: normalize it away
	// so a plain `--cores` change doesn't inherit a phantom GPU and then fail the
	// GPU fit-check (or trigger a spurious no-op miss) on a GPU-less host.
	//
	// A GPU-less machine whose cluster STILL carries that chart-default
	// GPU_REQUESTS is a "phantom GPU": jobs-manager reads any non-empty value as
	// a GPU cluster (client-runtime `_gpu_available_from_env` treats ONLY an
	// explicit-empty value as "no GPU"), so training pods request a nonexistent
	// nvidia.com/gpu — unschedulable, forcing a GPU→CPU fallback with a false GPU
	// heartbeat. Capture it BEFORE normalizing HasGPU away, so an otherwise-no-op
	// still writes the explicit-empty override that clears it (persistCeiling →
	// BuildEnvSpec → NoGPUEnvValue). (Bugbot #241; runtime behavior confirmed.)
	phantomGPU := current.HasGPU && !machineHasGPU
	if !machineHasGPU {
		current.HasGPU = false
	}

	// (3) Decide the desired per-run ceiling. Ctrl-C at a wizard prompt surfaces
	//     as errInteractiveCancelled — a choice, not a failure: exit 0 with the
	//     same "Cancelled" note the confirm decline prints below, matching the
	//     clean-cancel convention of every other prompting command (data
	//     ingest/delete, offboard). Validation errors (exit 2) and real terminal
	//     failures pass through unchanged.
	desired, err := decideDesired(p, pr, req, node, current, machineGPUName, machineGPUCount, machineHasGPU)
	if err != nil {
		if errors.Is(err, errInteractiveCancelled) {
			p.Infof("Cancelled — nothing was changed.")
			return nil
		}
		return err
	}

	// (4) No-op: skip the apply (and the jobs-manager roll) when nothing changed.
	//     Checked BEFORE the fit validation: "Leave it as it is" (and flags that
	//     merely restate the current ceiling) must stay a clean success even when
	//     the machine has shrunk under an already-applied ceiling (smaller Docker
	//     Desktop VM, lost node) — leaving things unchanged mutates nothing, so
	//     there is nothing for the fit-check to protect. Sizing an actual CHANGE
	//     is still validated below, before anything mutates.
	ceilingUnchanged := sameCeiling(desired, current)
	if ceilingUnchanged && !phantomGPU {
		p.Successf("Each training run already uses up to %s — nothing to change.", perRunSize(desired))
		return nil
	}
	if ceilingUnchanged { // phantomGPU == true here
		// CPU/memory budget is unchanged, but this GPU-less machine's cluster
		// still requests a GPU (a stale chart default). Don't treat it as a
		// clean no-op — fall through to persist so BuildEnvSpec's explicit-empty
		// GPU override lands and clears it; otherwise runs stay unschedulable /
		// fall back to CPU while the heartbeat keeps advertising a GPU.
		p.Infof("Your CPU and memory budget is unchanged — but this machine has no GPU while the cluster still requests one, so I'll clear that stale GPU setting so runs can schedule.")
	}

	// (5) Validate + fit-check — ONLY when the ceiling actually CHANGES. An
	//     unchanged ceiling mutates nothing the fit-check protects (a machine
	//     that shrank keeps whatever was already applied), and the phantom-GPU
	//     cleanup only REMOVES a GPU request — so a machine that shrank under its
	//     current ceiling must not block the cleanup with exit 2 (Bugbot #241:
	//     "phantom GPU cleanup blocked by fit-check"). Sizing an actual CHANGE is
	//     still validated; never mutate on failure.
	if !ceilingUnchanged {
		if verr := validateDesired(desired, node); verr != nil {
			return verr
		}
	}

	// (6) Confirm (unless --yes or --dry-run). One gate for both the flag and
	//     wizard paths; --dry-run mutates nothing so it never needs confirming.
	if !req.yes && !req.dryRun {
		if pr == nil {
			return &exitError{code: exitFailure, err: fmt.Errorf(
				"refusing to change the ceiling without confirmation: pass --yes, or run on a terminal")}
		}
		p.Newline()
		p.PromptHint("tracebloc keeps about 1 core and 3 GiB for itself on top of this — it fits on this machine.")
		proceed, cerr := pr.Confirm(fmt.Sprintf("Let each training run use up to %s?", perRunSize(desired)), true)
		if cerr != nil {
			// Ctrl-C here is the same user choice as answering "No": print the
			// same note the decline below (and a wizard interrupt above) prints,
			// and exit 0 — never a silent success that hides the abort.
			if errors.Is(cerr, errInteractiveCancelled) {
				p.Infof("Cancelled — nothing was changed.")
				return nil
			}
			return mapClientErr(cerr)
		}
		if !proceed {
			p.Infof("Cancelled — nothing was changed.")
			return nil
		}
	}

	// (7) Apply via helm (or print the plan under --dry-run). gpuRemoved lets the
	//     echo honestly announce a GPU being turned off — true only when a GPU
	//     that WAS configured is now gone (never on a CPU-only machine, where
	//     current.HasGPU is already normalized to false above).
	gpuRemoved := current.HasGPU && !desired.HasGPU
	return persistCeiling(ctx, p, target, opts, desired, gpuRemoved, req.dryRun)
}

// decideDesired produces the per-run ceiling from the three routes: `max` (whole
// machine minus overhead), explicit flags (override only the dimensions passed,
// keep the rest), or the interactive wizard.
func decideDesired(p *ui.Printer, pr prompter, req setReq, node resources.Machine, current resources.Training, gpuName corev1.ResourceName, gpuCount int64, machineHasGPU bool) (resources.Training, error) {
	switch {
	case req.max:
		cpu := *resource.NewQuantity(int64(resources.MaxRunCores(node)), resource.DecimalSI)
		mem := resources.GiBToQuantity(float64(resources.MaxRunGiB(node)))
		if machineHasGPU {
			return resources.DeriveTraining(cpu, mem, gpuName, *resource.NewQuantity(gpuCount, resource.DecimalSI), true), nil
		}
		return resources.DeriveTraining(cpu, mem, "", resource.Quantity{}, false), nil

	case req.coresSet || req.memSet || req.gpusSet:
		return desiredFromFlags(req, current, gpuName, gpuCount, machineHasGPU)

	default:
		return runResourcesWizard(p, pr, node, current, gpuName, gpuCount, machineHasGPU)
	}
}

// desiredFromFlags starts from the current ceiling and overrides only the
// dimensions the user passed. Parse errors surface as exit-2 human messages.
func desiredFromFlags(req setReq, current resources.Training, gpuName corev1.ResourceName, gpuCount int64, machineHasGPU bool) (resources.Training, error) {
	cpu := current.CPU
	if req.coresSet {
		c, err := resources.ParseCores(req.cores)
		if err != nil {
			return resources.Training{}, validationError(err.Error())
		}
		cpu = c
	}
	mem := current.Mem
	if req.memSet {
		m, bare, err := resources.ParseMemoryGiB(req.memory)
		if err != nil {
			return resources.Training{}, validationError(err.Error())
		}
		_ = bare // echo of the GiB interpretation happens in the confirm/echo via FormatMem
		mem = m
	}

	// GPU: --gpus wins; otherwise keep the current setting.
	wantGPU, gName, gQty := current.HasGPU, current.GPUName, current.GPU
	if req.gpusSet {
		if !machineHasGPU {
			return resources.Training{}, validationError("this machine has no GPU — drop --gpus.")
		}
		if req.gpus < 0 {
			return resources.Training{}, validationError("--gpus can't be negative.")
		}
		if req.gpus == 0 {
			wantGPU = false
		} else {
			wantGPU, gName, gQty = true, gpuName, *resource.NewQuantity(int64(req.gpus), resource.DecimalSI)
		}
	}
	return resources.DeriveTraining(cpu, mem, gName, gQty, wantGPU), nil
}

// runResourcesWizard is the guided, plain-language flow (Decision — interactive
// wizard by default): show the current budget vs the machine, offer
// "use as much as possible" (pre-selected), "choose an amount", or "leave it",
// and — for a custom amount — ask bounded per-dimension questions so over-asking
// is impossible.
func runResourcesWizard(p *ui.Printer, pr prompter, node resources.Machine, current resources.Training, gpuName corev1.ResourceName, gpuCount int64, machineHasGPU bool) (resources.Training, error) {
	maxCores := resources.MaxRunCores(node)
	maxGiB := resources.MaxRunGiB(node)

	// (1) Current per-run budget vs the machine.
	p.Section("How much of this machine a training run may use")
	p.Field("CPU", fmt.Sprintf("%s of %s cores", coresNum(current.CPU), coresNum(node.CPU)))
	p.Field("Memory", fmt.Sprintf("%s of %s GiB", gibNum(current.Mem), gibNum(node.Mem)))
	if machineHasGPU {
		p.Field("GPU", fmt.Sprintf("%d of %d", currentGPUCount(current), gpuCount))
	}

	// (2) The headline choice. "Use as much as possible" is first + pre-selected:
	//     recommended when this machine is dedicated to tracebloc.
	const (
		optMax    = "Use as much as possible (recommended if this machine is just for tracebloc)"
		optChoose = "Choose an amount"
		optLeave  = "Leave it as it is"
	)
	p.Newline()
	choice, err := pr.Select("How much may one training run use?",
		"tracebloc keeps about 1 core and 3 GiB for itself on top of your choice",
		[]string{optMax, optChoose, optLeave}, optMax)
	if err != nil {
		return resources.Training{}, err
	}

	switch choice {
	case optLeave:
		return current, nil // → no-op skip downstream
	case optMax:
		cpu := *resource.NewQuantity(int64(maxCores), resource.DecimalSI)
		mem := resources.GiBToQuantity(float64(maxGiB))
		if machineHasGPU {
			return resources.DeriveTraining(cpu, mem, gpuName, *resource.NewQuantity(gpuCount, resource.DecimalSI), true), nil
		}
		return resources.DeriveTraining(cpu, mem, "", resource.Quantity{}, false), nil
	}

	// (3) Choose an amount — bounded prompts so the answer always fits. If this
	//     machine is too small to give a run even the minimum after tracebloc's
	//     overhead (maxCores < 1 or maxGiB < 2), the bounds would be impossible
	//     (e.g. "1–0"): every answer is rejected and the wizard can't complete
	//     except by interrupting. Fail honestly instead (Bugbot #241).
	if maxCores < 1 || maxGiB < 2 {
		return resources.Training{}, &exitError{code: exitBadInput, err: fmt.Errorf(
			"this machine is too small to choose an amount — after tracebloc's ~1 core and 3 GiB "+
				"overhead it can offer a training run at most %d core(s) and %d GiB. Free up "+
				"resources or use a larger machine.", maxCores, maxGiB)}
	}
	coresAns, err := pr.Input(
		fmt.Sprintf("CPU cores for one run (1–%d)", maxCores),
		"how many CPU cores a single training run may use",
		coresNum(current.CPU), boundedInt(1, maxCores))
	if err != nil {
		return resources.Training{}, err
	}
	memAns, err := pr.Input(
		fmt.Sprintf("Memory for one run in GiB (2–%d)", maxGiB),
		"how much memory a single training run may use, in GiB",
		gibNum(current.Mem), boundedInt(2, maxGiB))
	if err != nil {
		return resources.Training{}, err
	}
	cpu := *resource.NewQuantity(int64(mustAtoi(coresAns)), resource.DecimalSI)
	mem := resources.GiBToQuantity(float64(mustAtoi(memAns)))

	wantGPU, gName, gQty := false, corev1.ResourceName(""), resource.Quantity{}
	if machineHasGPU {
		useGPU, cerr := pr.Confirm("Use the GPU for training runs?", current.HasGPU)
		if cerr != nil {
			return resources.Training{}, cerr
		}
		if useGPU {
			n := int64(1)
			if gpuCount > 1 {
				gAns, gerr := pr.Input(
					fmt.Sprintf("How many GPUs for one run (1–%d)", gpuCount),
					"whole GPUs a single run may use",
					strconv.FormatInt(defaultGPUChoice(current, gpuCount), 10), boundedInt(1, int(gpuCount)))
				if gerr != nil {
					return resources.Training{}, gerr
				}
				n = int64(mustAtoi(gAns))
			}
			wantGPU, gName, gQty = true, gpuName, *resource.NewQuantity(n, resource.DecimalSI)
		}
	}
	return resources.DeriveTraining(cpu, mem, gName, gQty, wantGPU), nil
}

// validateDesired enforces the per-run floor and the machine ceiling (with the
// platform overhead as the fit margin). Every failure is a human message + exit 2,
// and NOTHING is mutated. On macOS the honest ceiling message points at Docker
// Desktop → Resources rather than pretending the VM can be auto-raised (Decision D
// / P3 deferred).
func validateDesired(d resources.Training, node resources.Machine) error {
	if resources.BelowCoreFloor(d.CPU) {
		return validationError(fmt.Sprintf("a training run needs at least %s — %s is too little.",
			resources.CoreFloorText(), resources.FormatCPU(d.CPU)))
	}
	if resources.BelowMemFloor(d.Mem) {
		return validationError(fmt.Sprintf("a training run needs at least %s — %s is too little.",
			resources.MemFloorText(), resources.FormatMem(d.Mem)))
	}
	if resources.FitsNode(node, d.CPU, d.Mem, d.GPUName, d.GPU, d.HasGPU) {
		return nil
	}

	// Doesn't fit — say why, with the real max and the fix (Decision: state the
	// machine's real max + how to fix; leaving room for the ~1 core/3 GiB overhead).
	maxCores := resources.MaxRunCores(node)
	maxGiB := resources.MaxRunGiB(node)
	var msg string
	switch {
	case d.HasGPU && machineGPUShort(node, d):
		msg = fmt.Sprintf("this machine has %s, but you asked for %s.",
			resources.FormatGPU(d.GPUName, node.GPU[d.GPUName]), resources.FormatGPU(d.GPUName, d.GPU))
	default:
		msg = fmt.Sprintf(
			"that doesn't fit. This machine has %s · %s, and tracebloc keeps about %s and %s for itself, "+
				"so one run can use at most %d cores and %d GiB. Try --cores %d --memory %d.",
			resources.FormatCPU(node.CPU), resources.FormatMem(node.Mem),
			resources.CoreFloorText(), "3 GiB",
			maxCores, maxGiB, maxCores, maxGiB)
	}
	if runtime.GOOS == "darwin" {
		msg += " On macOS this ceiling is the Docker Desktop virtual machine — raise it in Docker Desktop → Settings → Resources, then try again."
	}
	return validationError(msg)
}

// machineGPUShort reports whether the desired GPU count exceeds what the node has.
func machineGPUShort(node resources.Machine, d resources.Training) bool {
	have, ok := node.GPU[d.GPUName]
	return !ok || have.Cmp(d.GPU) < 0
}

// persistCeiling writes the new ceiling through `helm upgrade` (or prints the plan
// under --dry-run). It builds the chart env (RESOURCE_* + GPU_*), pins the
// currently-installed chart version, and targets the resolved namespace/context so
// the upgrade lands on the SAME cluster the CLI read from.
func persistCeiling(ctx context.Context, p *ui.Printer, target *clusterTarget, opts cluster.KubeconfigOptions, d resources.Training, gpuRemoved, dryRun bool) error {
	// SAFETY: never run an unpinned `helm upgrade` against the remote chart.
	// ChartVersion comes from the release's helm.sh/chart label
	// (ParentRelease.ChartVersion); when it's missing, an upgrade with no
	// --version would pull the LATEST chart and silently change the whole client
	// as a side effect of a resource tweak. Refuse BEFORE mutating (dry-run too,
	// so it never advertises the unsafe command). A dev TRACEBLOC_CHART_PATH
	// override uses a local chart (no remote pull, no version to pin), so it's
	// exempt — mirroring the same exemption in helm.Upgrade.
	if chartPathOverride() == "" && strings.TrimSpace(target.Release.ChartVersion) == "" {
		return &exitError{code: exitFailure, err: fmt.Errorf(
			"couldn't determine the installed client chart version (the release is missing " +
				"its helm.sh/chart version label), so the upgrade can't be pinned to it. Refusing " +
				"to change resources with an unpinned upgrade — it would pull the latest chart and " +
				"could silently change your client. Re-run the tracebloc installer to repair the " +
				"release, then try again")}
	}

	env := resources.BuildEnvSpec(d.CPU, d.Mem, d.GPUName, d.GPU, d.HasGPU)
	params := helm.UpgradeParams{
		Release:      target.Release.ReleaseName,
		Namespace:    target.Resolved.Namespace,
		KubeContext:  target.Resolved.Context,
		Kubeconfig:   opts.Path,
		ChartVersion: target.Release.ChartVersion,
		ChartPath:    chartPathOverride(),
		Env:          env,
		DryRun:       dryRun,
	}
	plan, err := helm.Upgrade(ctx, params)
	if err != nil {
		return &exitError{code: exitFailure, err: err}
	}

	if dryRun {
		p.Newline()
		p.Section("Dry run — nothing was changed")
		p.Field("would set each run to", perRunSize(d))
		if gpuRemoved {
			// Honest only because BuildEnvSpec now writes the explicit no-GPU
			// value into the plan's values — the run really will lose the GPU.
			p.Field("gpu", "removed — runs will use CPU only")
		}
		p.Field("command", plan.Command)
		p.Para("values:")
		p.Para(strings.TrimRight(plan.ValuesYAML, "\n"))
		return nil
	}

	p.Newline()
	p.Successf("Each training run may now use up to %s.", perRunSize(d))
	if gpuRemoved {
		// The values written above set GPU_REQUESTS/GPU_LIMITS to the no-GPU
		// value, so this claim is true — the GPU is actually removed, not just
		// dropped from the printed size.
		p.Hintf("GPU access removed — training runs will use CPU only.")
	}
	p.Hintf("Applies to your next training run; a run already going keeps its size.")
	p.Hintf("Only tracebloc's small jobs-manager restarts — running training isn't interrupted.")
	return nil
}

// --- small helpers ----------------------------------------------------------

// validationError is a bad-input failure: exit 2, message printed by main() (the
// err is non-nil so it isn't silent). Every user-facing "that won't work" path
// funnels through here so the exit code stays consistent.
func validationError(msg string) error {
	return &exitError{code: exitBadInput, err: fmt.Errorf("%s", msg)}
}

// perRunSize renders a ceiling the way the user reads it: "4 CPU · 16 GiB · 1 GPU".
func perRunSize(t resources.Training) string {
	s := resources.FormatCPU(t.CPU) + " · " + resources.FormatMem(t.Mem)
	if t.HasGPU {
		s += " · " + resources.FormatGPU(t.GPUName, t.GPU)
	}
	return s
}

// sameCeiling reports whether two ceilings are identical across every dimension —
// the no-op guard, so an unchanged `set` skips the apply and the jobs-manager roll.
func sameCeiling(a, b resources.Training) bool {
	if a.CPU.Cmp(b.CPU) != 0 || a.Mem.Cmp(b.Mem) != 0 {
		return false
	}
	if a.HasGPU != b.HasGPU {
		return false
	}
	if a.HasGPU {
		return a.GPUName == b.GPUName && a.GPU.Cmp(b.GPU) == 0
	}
	return true
}

// coresNum / gibNum render just the number ("8", "32") by trimming the unit the
// SHOW formatters append, for the wizard's "x of N" lines.
func coresNum(q resource.Quantity) string { return strings.TrimSuffix(resources.FormatCPU(q), " CPU") }
func gibNum(q resource.Quantity) string   { return strings.TrimSuffix(resources.FormatMem(q), " GiB") }

func currentGPUCount(t resources.Training) int64 {
	if t.HasGPU {
		return t.GPU.Value()
	}
	return 0
}

// defaultGPUChoice pre-fills the "how many GPUs" prompt: the current count when a
// GPU run is already configured, else 1.
func defaultGPUChoice(current resources.Training, machineCount int64) int64 {
	if current.HasGPU && current.GPU.Value() >= 1 && current.GPU.Value() <= machineCount {
		return current.GPU.Value()
	}
	return 1
}

// boundedInt is a prompt validator accepting a whole number in [lo, hi].
func boundedInt(lo, hi int) func(string) error {
	return func(s string) error {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("enter a whole number between %d and %d", lo, hi)
		}
		if n < lo || n > hi {
			return fmt.Errorf("must be between %d and %d", lo, hi)
		}
		return nil
	}
}

// mustAtoi parses an int that a bounded-prompt validator already accepted; a
// leftover parse error can only mean the validator was bypassed, so 0 (which the
// downstream floor check rejects) is a safe, non-panicking fallback.
func mustAtoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
