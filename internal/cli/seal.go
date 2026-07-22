// The seal check — `tracebloc client status --seal` (cli#393, RFC-0003 §8.2
// D12). Runs the chart's conformance checks (its helm-test hooks; see
// internal/helm/seal.go for the chart contract) against this machine's secure
// environment and reports an honest verdict:
//
//	sealed   — every conformance check passed (exit 0)
//	unsealed — a check failed or couldn't run: a protection is not enforced (exit 2)
//	unknown  — the chart ships no conformance checks, so nothing was verified (exit 2)
//
// The honest-output principle (the same one delete's closing summary follows):
// never print a success state that wasn't verified. "Unknown" is explicitly NOT
// sealed — an environment that cannot demonstrate a guarantee is never claimed
// to hold it — and only a fully-passed suite exits 0, so scripts can gate on it.

package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/helm"
	"github.com/tracebloc/cli/internal/ui"
)

// Seams over the helm seal-check calls, so tests drive every verdict path
// without helm or a cluster — the same fn-var pattern as listDatasetsFn /
// resolveClusterTargetFn.
var (
	listTestHooksFn = helm.ListTestHooks
	runHelmTestFn   = helm.RunTest
)

// sealCheck is one conformance check's outcome, ready to render.
type sealCheck struct {
	name   string // display name (chart's seal-name annotation, else the de-prefixed hook name)
	passed bool
	detail string // one-line failure detail ("" when passed)
	hint   string // one-line remediation pointer ("" when passed)
}

// sealModel is everything renderSealResult needs — a pure value, so the copy
// catalog renders the sealed / unsealed / unknown screens byte-exact.
type sealModel struct {
	envName  string      // the secure environment being verified
	fallback bool        // chart marks no seal-check suite; ran ALL of its helm tests instead
	checks   []sealCheck // empty = the chart ships no conformance checks (verdict: unknown)
}

// sealed reports the verdict: true only when there were checks and every one
// passed. No checks at all is NOT sealed (it's unknown — nothing was verified).
func (m sealModel) sealed() bool {
	if len(m.checks) == 0 {
		return false
	}
	return m.failedCount() == 0
}

func (m sealModel) failedCount() int {
	n := 0
	for _, c := range m.checks {
		if !c.passed {
			n++
		}
	}
	return n
}

// runSealCheck drives the seal verdict: resolve the cluster + release the same
// way the data commands do (exit 3 unreachable, exit 4 no client — with the
// §7.3 active-client binding and its "runs on another machine" rewrite), list
// the chart's test hooks, run each one, render, and exit by the verdict.
func runSealCheck(ctx context.Context, p *ui.Printer, opts cluster.KubeconfigOptions, timeout time.Duration) error {
	binding := bindActiveClientNamespace(&opts)
	target, err := resolveClusterTargetFn(ctx, p, opts, binding, false)
	if err != nil {
		return binding.explain(err)
	}
	tt := helm.TestTarget{
		Release:     target.Release.ReleaseName,
		Namespace:   target.Resolved.Namespace,
		Kubeconfig:  opts.Path,
		KubeContext: opts.Context,
	}

	hooks, herr := listTestHooksFn(ctx, tt)
	if herr != nil {
		// Can't even enumerate the checks — that's an error, not a verdict:
		// printing "unsealed" (or worse, "unknown") off a helm failure would
		// misstate what we know about the environment.
		return &exitError{code: exitFailure, err: fmt.Errorf(
			"couldn't read the chart's conformance checks: %w", herr)}
	}
	suite, fallback := sealSuite(hooks)
	model := sealModel{envName: sealEnvName(target, binding), fallback: fallback}

	if len(suite) == 0 {
		renderSealResult(p, model)                          // header + the unknown verdict
		return &exitError{code: exitChecksFailed, err: nil} // rendered above — exit silent
	}

	renderSealHeader(p, model)
	for _, h := range suite {
		name := sealCheckName(h, tt.Release)
		sp := p.Spinner(fmt.Sprintf("Checking %s…", name), "")
		out, terr := runHelmTestFn(ctx, tt, h.Name, timeout)
		sp.Stop()
		// Ctrl-C (or a parent deadline) mid-suite is a cancelled run, not a
		// verdict: every remaining check would "fail" on the dead context and
		// render a fake Unsealed. Exit quietly, the way `status --wait` does.
		if ctx.Err() != nil {
			return &exitError{code: exitInterrupted}
		}
		check := sealCheck{name: name, passed: terr == nil}
		if terr != nil {
			check.detail = helmFailureDetail(out, terr)
			check.hint = sealFailureHint(h, tt.Namespace)
		}
		renderSealCheckLine(p, check)
		model.checks = append(model.checks, check)
	}

	renderSealVerdict(p, model)
	if !model.sealed() {
		return &exitError{code: exitChecksFailed, err: nil} // rendered above — exit silent
	}
	return nil
}

// sealSuite picks the checks to run: the hooks the chart labels as the
// seal-check suite; when the chart doesn't label any (an older chart), ALL of
// its helm tests, reported as a fallback so the output says which contract ran.
func sealSuite(hooks []helm.TestHook) (suite []helm.TestHook, fallback bool) {
	for _, h := range hooks {
		if h.SealCheck {
			suite = append(suite, h)
		}
	}
	if len(suite) > 0 {
		return suite, false
	}
	return hooks, len(hooks) > 0
}

// sealEnvName names the environment under test: the active client's display
// name when the §7.3 binding chose the target, else the namespace actually
// resolved (explicit --namespace/--context, or the kubeconfig default).
func sealEnvName(target *clusterTarget, binding activeClientBinding) string {
	if binding.applied && binding.name != "" {
		return binding.name
	}
	return target.Resolved.Namespace
}

// sealCheckName is a check's display name: the chart's seal-name annotation
// when present, else the hook name with the release prefix trimmed (the
// chart's hooks are named "<release>-<check>").
func sealCheckName(h helm.TestHook, release string) string {
	if h.SealName != "" {
		return h.SealName
	}
	return strings.TrimPrefix(h.Name, release+"-")
}

// sealFailureHint is the one-line remediation pointer under a failed check:
// the chart's seal-hint annotation when present, else the kubectl command that
// shows the check's own output (the probes print their diagnosis).
func sealFailureHint(h helm.TestHook, namespace string) string {
	if h.SealHint != "" {
		return h.SealHint
	}
	ref := h.Name
	if strings.EqualFold(h.Kind, "Job") {
		ref = "job/" + h.Name
	}
	return fmt.Sprintf("see why: kubectl logs -n %s %s", namespace, ref)
}

// helmFailureDetail distills helm's combined output + error into the one-line
// reason a check failed. helm reports hook failures as a bullet list under an
// "Error:" line ("* job failed: BackoffLimitExceeded", "* timed out waiting
// for the condition"); prefer the first bullet (the specific reason), then the
// Error: line, then the raw error's first line — never empty for a failure.
func helmFailureDetail(out string, err error) string {
	if err == nil {
		return ""
	}
	var errLine, bullet string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case bullet == "" && strings.HasPrefix(l, "* "):
			bullet = strings.TrimSpace(strings.TrimPrefix(l, "* "))
		case errLine == "" && strings.HasPrefix(l, "Error:"):
			errLine = strings.TrimSpace(strings.TrimPrefix(l, "Error:"))
		}
	}
	detail := bullet
	if detail == "" {
		detail = errLine
	}
	if detail == "" {
		detail, _, _ = strings.Cut(err.Error(), "\n")
	}
	return sealTrimDetail(detail)
}

// sealTrimDetail caps a failure detail to one readable line; the hint under it
// points at the full output.
func sealTrimDetail(s string) string {
	const max = 140
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// ── rendering ────────────────────────────────────────────────────────────────
// Split header / per-check line / verdict so the live path streams each check's
// result as it lands while the copy catalog renders the identical composed
// screen (renderSealResult).

func renderSealHeader(p *ui.Printer, m sealModel) {
	p.Section(fmt.Sprintf("Seal check — secure environment %q", m.envName))
	if m.fallback {
		p.Infof("This chart doesn't mark a seal-check suite yet — running all of its checks instead.")
	}
	p.Newline()
}

func renderSealCheckLine(p *ui.Printer, c sealCheck) {
	if c.passed {
		p.CheckLine("%s", c.name)
		return
	}
	p.CrossLine("%s — %s", c.name, c.detail)
	if c.hint != "" {
		p.Hintf("    %s", c.hint)
	}
}

func renderSealVerdict(p *ui.Printer, m sealModel) {
	if len(m.checks) == 0 {
		// No conformance checks shipped — HONESTLY unknown. Never word this as
		// sealed: nothing was verified.
		p.Warnf("Seal unknown — this chart ships no conformance checks, so this environment's protections can't be verified. Not claiming sealed.")
		p.Hintf("Upgrade your secure environment to a chart with the seal-check suite, then re-run `tracebloc client status --seal`.")
		return
	}
	p.Newline()
	// The default chart can ship a single conformance check (the enforcement
	// probe only renders once the egress lockdown is enabled), so the singular
	// wording is a common case, not an edge.
	single := len(m.checks) == 1
	switch failed := m.failedCount(); {
	case failed > 0 && single:
		p.Errorf("Unsealed — the conformance check failed. This environment's protections are not enforced.")
	case failed > 0:
		p.Errorf("Unsealed — %d of %d conformance checks failed. This environment's protections are not all enforced.", failed, len(m.checks))
	case single:
		p.Successf("Sealed — the conformance check passed. This environment's protections are enforced.")
	default:
		p.Successf("Sealed — all %d conformance checks passed. This environment's protections are enforced.", len(m.checks))
	}
	if m.failedCount() > 0 {
		p.Hintf("Fix the failing checks above, then re-run `tracebloc client status --seal` to confirm the seal.")
	}
}

// renderSealResult renders a complete seal screen from a model — what a full
// run prints, composed of the same pieces the live path streams. The copy
// catalog drives this for the sealed / unsealed / unknown states.
func renderSealResult(p *ui.Printer, m sealModel) {
	renderSealHeader(p, m)
	for _, c := range m.checks {
		renderSealCheckLine(p, c)
	}
	renderSealVerdict(p, m)
}
