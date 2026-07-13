// Package helm is the thin, testable seam `tracebloc resources set` (cli#143 P2)
// uses to persist a new per-run resource ceiling into the client's Helm release.
//
// WHY HELM (not `kubectl set env`): the hourly auto-upgrade CronJob re-runs
// `helm upgrade --reset-then-reuse-values`, which resets to chart defaults and
// re-applies the release's STORED values — so any change made outside Helm (a raw
// `kubectl set env` on the jobs-manager) is silently reverted on the next tick.
// Writing the value into the release's Helm values is the only durable path, and
// it lands in the release Config exactly where reset-then-reuse preserves it
// (already CI-proven by the auto-upgrade job).
//
// It mirrors the installer's proven idiom (scripts/lib/install-client-helm.sh
// `_resolve_chart_ref` + `_reconcile_adopted_client`, and auto-upgrade-cronjob.yaml):
// resolve the chart ref (dev path or remote repo), version-gate
// `--reset-then-reuse-values` vs `--reuse-values`, pin `--version`, `--wait`. The
// new values go through a temp `-f` file rather than `--set` to dodge the
// comma footgun ("cpu=2,memory=8Gi" would be split into two --set keys).
//
// Like nodeboot.Runner, the actual shell-out is a package var so tests drive the
// whole path against a fake without a real cluster, helm, or network.
package helm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

const (
	// repoName / repoURL / chartName mirror install-client-helm.sh's
	// TRACEBLOC_HELM_REPO_NAME / _URL / TRACEBLOC_CHART_NAME so a CLI-driven
	// upgrade resolves the SAME chart the installer and auto-upgrade job do.
	repoName  = "tracebloc"
	repoURL   = "https://tracebloc.github.io/client"
	chartName = "client"

	// ChartRef is the remote chart reference (repo/name) used when no dev
	// TRACEBLOC_CHART_PATH override is set.
	ChartRef = repoName + "/" + chartName

	// resetThenReuse is preferred (helm >= 3.14): reset to the new chart's
	// defaults, then re-apply the release's stored user values, so operator
	// overrides survive while new defaults flow through. reuse is the older
	// fallback. Same gate the installer uses.
	resetThenReuse = "--reset-then-reuse-values"
	reuse          = "--reuse-values"
)

// Runner executes an external command and returns its combined output. A package
// var so tests substitute a fake without spawning real helm. Mirrors
// nodeboot.Runner exactly.
var Runner = func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// UpgradeParams is everything the resource-ceiling upgrade needs. Namespace,
// KubeContext and Kubeconfig come resolved from resolveClusterTarget so helm acts
// on the SAME cluster the CLI read from — never the ambient current-context.
type UpgradeParams struct {
	Release      string            // helm release name (== namespace, installer convention)
	Namespace    string            // resolved namespace
	KubeContext  string            // resolved kube-context (empty = current-context)
	Kubeconfig   string            // kubeconfig path (empty = ambient $KUBECONFIG/~/.kube/config)
	ChartVersion string            // pin --version to the currently-installed chart version
	ChartPath    string            // TRACEBLOC_CHART_PATH dev override; when set, skip repo add/update
	Env          map[string]string // chart env keys to set (RESOURCE_*, GPU_*)
	Timeout      string            // helm --timeout (default 5m)
	DryRun       bool              // print the plan, run/apply NOTHING
}

// Plan is the resolved-but-not-yet-run description of the upgrade: the exact helm
// argv and the values YAML that would be written. Returned so the CLI can echo it
// (always in --dry-run, at --verbose otherwise) without re-deriving it.
type Plan struct {
	Command    string // "helm upgrade …" for display
	Args       []string
	ValuesYAML string
	ChartRef   string
}

// Upgrade persists the new per-run ceiling by upgrading the release in place.
//
//   - DryRun: builds and returns the Plan and runs NOTHING — no repo add/update,
//     no helm upgrade, no temp file left behind. The printed argv uses the modern
//     --reset-then-reuse-values (what a current helm resolves to).
//   - Otherwise: version-gates the reuse flag against the local helm, resolves the
//     chart ref (adding/updating the repo unless a dev ChartPath is set), writes
//     the values to a temp -f file, and runs `helm upgrade … --wait`.
//
// It never mutates cluster state except through that single `helm upgrade`.
func Upgrade(ctx context.Context, p UpgradeParams) (Plan, error) {
	// SAFETY: a remote chart ref MUST be pinned. `helm upgrade tracebloc/client`
	// without --version resolves the LATEST published chart, so a values-only
	// change (raising a run's resource ceiling) would silently bump the whole
	// client to a new chart version. Refuse before touching anything — including
	// in --dry-run, whose printed plan would otherwise be an unsafe command we'd
	// never actually run. A local ChartPath (dev override) needs no pin: helm
	// uses the on-disk chart and --version doesn't apply. buildArgs only appends
	// --version when non-empty, so this guard is what stands between an absent
	// version and an unpinned upgrade.
	if p.ChartPath == "" && strings.TrimSpace(p.ChartVersion) == "" {
		return Plan{}, fmt.Errorf(
			"refusing to upgrade the remote chart %q without a pinned --version: "+
				"helm would pull the latest chart and could silently change the release", ChartRef)
	}

	valuesYAML := renderValues(p.Env)
	chartRef := ChartRef
	if p.ChartPath != "" {
		chartRef = p.ChartPath
	}
	timeout := p.Timeout
	if timeout == "" {
		timeout = "5m"
	}

	if p.DryRun {
		// Apply nothing. Show the command a real run would issue on modern helm.
		args := buildArgs(p, chartRef, resetThenReuse, "<values-file>", timeout)
		return plan(chartRef, args, valuesYAML), nil
	}

	// Version-gate the reuse flag exactly like the installer.
	reuseFlag := reuse
	if supportsResetThenReuse(ctx) {
		reuseFlag = resetThenReuse
	}

	// Resolve the chart: dev path needs no repo; the remote repo is added (if
	// missing) then updated, mirroring _resolve_chart_ref.
	if p.ChartPath == "" {
		if err := ensureRepo(ctx); err != nil {
			return Plan{}, err
		}
	}

	// Write the values to a temp -f file. -f (not --set) so the "cpu=2,memory=8Gi"
	// commas aren't parsed as multiple --set keys.
	f, err := os.CreateTemp("", "tracebloc-resources-*.yaml")
	if err != nil {
		return Plan{}, fmt.Errorf("creating values file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, werr := f.WriteString(valuesYAML); werr != nil {
		_ = f.Close()
		return Plan{}, fmt.Errorf("writing values file: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		return Plan{}, fmt.Errorf("closing values file: %w", cerr)
	}

	args := buildArgs(p, chartRef, reuseFlag, f.Name(), timeout)
	if out, uerr := Runner(ctx, "helm", args...); uerr != nil {
		return Plan{}, fmt.Errorf("helm upgrade failed: %w\n%s", uerr, strings.TrimSpace(out))
	}
	return plan(chartRef, args, valuesYAML), nil
}

// buildArgs assembles the helm upgrade argv. Order mirrors the installer/cronjob:
// release, chart, --namespace, [--kube-context], [--kubeconfig], --version, reuse,
// -f values, --wait --timeout. Context/kubeconfig/version are appended only when
// present, preserving default behavior.
func buildArgs(p UpgradeParams, chartRef, reuseFlag, valuesPath, timeout string) []string {
	args := []string{"upgrade", p.Release, chartRef, "--namespace", p.Namespace}
	if p.KubeContext != "" {
		args = append(args, "--kube-context", p.KubeContext)
	}
	if p.Kubeconfig != "" {
		args = append(args, "--kubeconfig", p.Kubeconfig)
	}
	if p.ChartVersion != "" {
		args = append(args, "--version", p.ChartVersion)
	}
	args = append(args, reuseFlag, "-f", valuesPath, "--wait", "--timeout", timeout)
	return args
}

func plan(chartRef string, args []string, valuesYAML string) Plan {
	return Plan{
		Command:    "helm " + strings.Join(args, " "),
		Args:       args,
		ValuesYAML: valuesYAML,
		ChartRef:   chartRef,
	}
}

// supportsResetThenReuse reports whether the local helm understands
// --reset-then-reuse-values (helm >= 3.14). Same probe the installer uses:
// grep the upgrade help. A probe failure conservatively answers no → the safe
// --reuse-values fallback.
func supportsResetThenReuse(ctx context.Context) bool {
	out, err := Runner(ctx, "helm", "upgrade", "--help")
	if err != nil {
		return false
	}
	return strings.Contains(out, resetThenReuse)
}

// ensureRepo adds the tracebloc helm repo if it isn't present, then updates it —
// mirroring _resolve_chart_ref. A repo-add failure is fatal (the upgrade can't
// resolve the chart); a repo-update failure is fatal too (stale index could pin
// the wrong version).
func ensureRepo(ctx context.Context) error {
	list, _ := Runner(ctx, "helm", "repo", "list")
	if !repoPresent(list) {
		if out, err := Runner(ctx, "helm", "repo", "add", repoName, repoURL); err != nil {
			return fmt.Errorf("helm repo add: %w\n%s", err, strings.TrimSpace(out))
		}
	}
	if out, err := Runner(ctx, "helm", "repo", "update", repoName); err != nil {
		return fmt.Errorf("helm repo update: %w\n%s", err, strings.TrimSpace(out))
	}
	return nil
}

// repoPresent reports whether `helm repo list` output names our repo (first
// column). Mirrors the installer's `grep "^tracebloc[[:space:]]"`.
func repoPresent(list string) bool {
	for _, line := range strings.Split(list, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == repoName {
			return true
		}
	}
	return false
}

// renderValues renders the chart env overrides as a minimal values YAML:
//
//	env:
//	  RESOURCE_LIMITS: "cpu=4,memory=16Gi"
//	  ...
//
// Keys are sorted for deterministic output (stable dry-run text + tests). Values
// are quoted so the "cpu=4,memory=16Gi" comma/colon can't confuse the YAML parser.
func renderValues(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("env:\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %q\n", k, env[k])
	}
	return b.String()
}
