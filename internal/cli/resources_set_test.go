package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/helm"
	"github.com/tracebloc/cli/internal/resources"
	"github.com/tracebloc/cli/internal/ui"
)

// setTarget builds a resolved cluster target from a fake clientset, with a chart
// version so the apply path can pin --version.
func setTarget(cs *fake.Clientset) *clusterTarget {
	return &clusterTarget{
		Resolved:  &cluster.ResolvedConfig{Namespace: "tracebloc", Context: "my-ctx"},
		Clientset: cs,
		Release:   &cluster.ParentRelease{ReleaseName: "tb", ChartVersion: "1.3.5"},
	}
}

// fakeHelm installs a recording helm.Runner (reset-then-reuse capable) and
// returns a pointer to the captured calls.
func fakeHelm(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	orig := helm.Runner
	helm.Runner = func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) >= 2 && args[0] == "upgrade" && args[1] == "--help" {
			return "  --reset-then-reuse-values", nil
		}
		return "", nil
	}
	t.Cleanup(func() { helm.Runner = orig })
	return &calls
}

// fakeHelmValues is fakeHelm plus the CONTENTS of the `-f` values file, read
// while the upgrade is in flight (helm.Upgrade writes and closes it before
// shelling Runner, and removes it after) — so it is only readable from inside
// the double.
//
// Needed because the existing provenance assertions all go through --dry-run,
// and --dry-run skips the confirmation gate. Proving that a marker lands on a
// path whose bug WAS the gate therefore has to use the real apply path
// (backend#2220).
func fakeHelmValues(t *testing.T) (*[][]string, *string) {
	t.Helper()
	var values string
	calls := fakeHelm(t)
	inner := helm.Runner
	helm.Runner = func(ctx context.Context, name string, args ...string) (string, error) {
		for i, a := range args {
			if a == "-f" && i+1 < len(args) {
				if b, err := os.ReadFile(args[i+1]); err == nil {
					values = string(b)
				}
			}
		}
		return inner(ctx, name, args...)
	}
	return calls, &values
}

// helmUpgraded reports whether a real `helm upgrade` (not the `--help` capability
// probe) was shelled.
func helmUpgraded(calls [][]string) bool {
	for _, c := range calls {
		if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
			return true
		}
	}
	return false
}

// runSet drives applyResourcesSet against a fake cluster + prompter, returning the
// captured stdout and the error.
func runSet(t *testing.T, cs *fake.Clientset, pr prompter, req setReq) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), pr,
		setTarget(cs), cluster.KubeconfigOptions{Path: "/tmp/kc"}, req)
	return buf.String(), err
}

func csWith(nodeCPU, nodeMem string, env map[string]string, gpu ...string) *fake.Clientset {
	return fake.NewClientset(resNode("n1", nodeCPU, nodeMem, gpu...), resJMDeploy("tb", env))
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, not an *exitError", err)
	}
	return ee.Code()
}

// --- validation matrix ------------------------------------------------------

func TestSet_ValidationMatrix(t *testing.T) {
	cur := map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"}
	cases := []struct {
		name string
		req  setReq
		want int
	}{
		{"cores too high", setReq{cores: "20", coresSet: true, yes: true}, 2},
		{"memory too high", setReq{memory: "200", memSet: true, yes: true}, 2},
		{"below core floor", setReq{cores: "0.5", coresSet: true, yes: true}, 2},
		{"below mem floor", setReq{memory: "1", memSet: true, yes: true}, 2},
		{"zero cores", setReq{cores: "0", coresSet: true, yes: true}, 2},
		{"negative cores", setReq{cores: "-2", coresSet: true, yes: true}, 2},
		{"cores wrong unit", setReq{cores: "6GB", coresSet: true, yes: true}, 2},
		{"memory wrong unit", setReq{memory: "16Mi", memSet: true, yes: true}, 2},
		{"memory unparseable", setReq{memory: "lots", memSet: true, yes: true}, 2},
		{"gpus on no-gpu machine", setReq{gpus: 1, gpusSet: true, yes: true}, 2},
		{"max plus explicit", setReq{max: true, cores: "4", coresSet: true, yes: true}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs := csWith("8", "32Gi", cur)
			_, err := runSet(t, cs, nil, c.req)
			if got := exitCode(t, err); got != c.want {
				t.Errorf("exit = %d, want %d", got, c.want)
			}
		})
	}
}

// TestSet_TooHighStatesRealMaxAndFix: the too-high message names the machine's
// real max and the exact fix flag.
func TestSet_TooHighStatesRealMaxAndFix(t *testing.T) {
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	out, err := runSet(t, cs, nil, setReq{cores: "20", coresSet: true, yes: true})
	if exitCode(t, err) != 2 {
		t.Fatalf("want exit 2")
	}
	// The message is on the error (non-silent), so main() prints it — assert its text.
	msg := err.Error()
	for _, want := range []string{"7 cores", "--cores 7"} {
		if !strings.Contains(msg, want) {
			t.Errorf("too-high message missing %q: %s\n%s", want, msg, out)
		}
	}
}

// TestSet_OffTTYWithoutFlags: no flags, no max, no terminal → exit 2 with a guide,
// not a usage dump.
func TestSet_OffTTYWithoutFlags(t *testing.T) {
	err := validateRequestShape(setReq{}, false)
	if exitCode(t, err) != 2 {
		t.Fatalf("want exit 2 for empty off-TTY set")
	}
	if !strings.Contains(err.Error(), "run this on a terminal") {
		t.Errorf("message should point at a terminal / flags: %s", err.Error())
	}
}

// TestSet_OffTTYWithFlagsNeedsYes: flags off a terminal without --yes → exit 1
// (mutating command needs confirmation), mirroring `delete`.
func TestSet_OffTTYWithFlagsNeedsYes(t *testing.T) {
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	_, err := runSet(t, cs, nil, setReq{cores: "4", coresSet: true}) // yes:false, pr:nil
	if exitCode(t, err) != 1 {
		t.Fatalf("want exit 1 without --yes off a terminal, got %v", err)
	}
}

// --- apply path -------------------------------------------------------------

// TestSet_ApplyBuildsHelmArgsAndValues: an explicit CPU-only set reaches helm with
// the right args (reset-then-reuse, -f, --version, resolved ctx/ns) and the temp
// values carry RESOURCE_*.
func TestSet_ApplyBuildsHelmArgsAndValues(t *testing.T) {
	calls := fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	out, err := runSet(t, cs, nil, setReq{cores: "4", memory: "16", coresSet: true, memSet: true, yes: true})
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	var upgrade []string
	for _, c := range *calls {
		if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
			upgrade = c
		}
	}
	if upgrade == nil {
		t.Fatalf("no helm upgrade call; calls=%v", *calls)
	}
	joined := strings.Join(upgrade, " ")
	for _, want := range []string{
		"upgrade tb", "--namespace tracebloc", "--kube-context my-ctx",
		"--kubeconfig /tmp/kc", "--version 1.3.5", "--reset-then-reuse-values", "-f", "--wait",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("helm args missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(out, "up to 4 CPU · 16 GiB") {
		t.Errorf("success echo missing the new size:\n%s", out)
	}
}

// TestSet_StampsUserProvenance: the apply carries RESOURCE_PROVENANCE=user all
// the way into the values helm is given (backend#2220).
//
// The unit test on BuildEnvSpec proves the map is right; this proves the map
// actually reaches helm. Worth having separately because the failure mode is
// silent: a marker that never lands looks identical to one that landed, and the
// consequence only shows up much later, when a ladder re-derives a size the
// operator had chosen on purpose.
func TestSet_StampsUserProvenance(t *testing.T) {
	// Start from an edge the INSTALLER marked — the dangerous case. After a
	// human `resources set`, the label must flip to user; leaving it as
	// "installer" would advertise a deliberate choice as ours to overwrite.
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS":     "cpu=2,memory=8Gi",
		"RESOURCE_PROVENANCE": "installer",
	})
	out, err := runSet(t, cs, nil, setReq{
		cores: "4", memory: "16", coresSet: true, memSet: true, dryRun: true, yes: true,
	})
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "RESOURCE_PROVENANCE") {
		t.Errorf("the plan does not mention RESOURCE_PROVENANCE at all:\n%s", out)
	}
	if !strings.Contains(out, "user") {
		t.Errorf("the plan does not stamp the set as a human choice:\n%s", out)
	}
	// And the envelope still reflects what was asked for (Decision A).
	if !strings.Contains(out, "cpu=4,memory=16Gi") {
		t.Errorf("plan lost the requested ceiling:\n%s", out)
	}
}

// TestSet_KeepsUnsetDimension: `set --cores 4` changes CPU only and KEEPS the
// current 8Gi memory (proven via the dry-run plan's resulting values).
func TestSet_KeepsUnsetDimension(t *testing.T) {
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	out, err := runSet(t, cs, nil, setReq{cores: "4", coresSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out, "cpu=4,memory=8Gi") {
		t.Errorf("unset memory should be kept at 8Gi:\n%s", out)
	}
}

// TestSet_MaxUsesWholeMachineMinusOverhead: `set max` writes cores/mem =
// machine − overhead (7 / 29 on an 8-core/32Gi node) and includes the GPU.
func TestSet_MaxUsesWholeMachineMinusOverhead(t *testing.T) {
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), nil,
		setTarget(csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"}, "nvidia.com/gpu", "1")),
		cluster.KubeconfigOptions{}, setReq{max: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("dry-run max: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "cpu=7,memory=29Gi") {
		t.Errorf("max should be 7 CPU / 29 GiB (machine − overhead):\n%s", out)
	}
	if !strings.Contains(out, "nvidia.com/gpu=1") {
		t.Errorf("max should include the machine GPU:\n%s", out)
	}
}

// TestSet_NoOpSkipsApply: setting the ceiling to what it already is skips the
// helm upgrade entirely.
func TestSet_NoOpSkipsApply(t *testing.T) {
	calls := fakeHelm(t)
	// RESOURCE_PROVENANCE=user is what makes this a CLEAN no-op (backend#2220):
	// restating the ceiling on an edge whose size is already recorded as the
	// operator's choice leaves nothing at all to persist. Without the marker the
	// command must fall through and stamp it — covered by
	// TestSet_SameCeilingStampsProvenance below.
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS":     "cpu=4,memory=16Gi",
		"RESOURCE_PROVENANCE": "user",
	})
	out, err := runSet(t, cs, nil, setReq{cores: "4", memory: "16", coresSet: true, memSet: true, yes: true})
	if err != nil {
		t.Fatalf("no-op: %v", err)
	}
	for _, c := range *calls {
		if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
			t.Errorf("no-op must NOT call helm upgrade: %v", c)
		}
	}
	if !strings.Contains(out, "nothing to change") {
		t.Errorf("no-op should say nothing changed:\n%s", out)
	}
}

// TestSet_SameCeilingStampsProvenance: the hole Bugbot found on #539 and
// saadqbal confirmed — a same-size `resources set` used to return BEFORE
// BuildEnvSpec, so an installer-sized edge kept RESOURCE_PROVENANCE=installer
// even though a human had just chosen that size.
//
// That is the state BuildEnvSpec's own comment calls the most dangerous the
// marker can be in: a deliberate choice wearing the one label that invites a
// future ladder to overwrite it. `resources set` restating the current ceiling
// IS a human choice, so it must persist.
func TestSet_SameCeilingStampsProvenance(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provenance string
	}{
		{"installer-sized edge", "installer"},
		{"pre-marker edge", ""},   // ParseTraining normalises to unknown
		{"junk marker", "banana"}, // ...as does anything unrecognised
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeHelm(t)
			env := map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi"}
			if tc.provenance != "" {
				env["RESOURCE_PROVENANCE"] = tc.provenance
			}
			cs := csWith("8", "32Gi", env)
			out, err := runSet(t, cs, nil, setReq{
				cores: "4", memory: "16", coresSet: true, memSet: true, yes: true,
			})
			if err != nil {
				t.Fatalf("same-ceiling set: %v\n%s", err, out)
			}
			upgraded := false
			for _, c := range *calls {
				if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
					upgraded = true
				}
			}
			if !upgraded {
				t.Errorf("a stale %q marker must NOT be a clean no-op — the apply is what stamps `user`:\n%s",
					tc.provenance, out)
			}
			if strings.Contains(out, "nothing to change") {
				t.Errorf("must not claim nothing changed while the marker is being corrected:\n%s", out)
			}
			if !strings.Contains(out, "explicit choice") {
				t.Errorf("the reason for the apply should be stated:\n%s", out)
			}
		})
	}
}

// TestSet_SameCeilingNeedsNoYes: a same-ceiling restatement must NOT require
// --yes. This is the script-facing half of backend#2220 that #539 broke — and
// the half its tests missed, because every same-ceiling case there passed
// `yes: true`, which is exactly the flag under dispute.
//
// #539 made any non-`user` marker stale, which sent the unchanged-ceiling path
// into the confirmation gate for the first time. Off a terminal that gate does
// not ask, it returns exit 1 — so `resources set --cores 4 --memory 16`
// restating the current ceiling flipped from the documented exit-0 no-op ("0
// applied (or nothing to change)" in the command's own help; the `no change →
// exit 0` edge in docs/cli-navigation.md, which bypasses CONF entirely) to a
// hard failure. Nearly every installed edge reads `installer` or `unknown`, so
// the blast radius was the installed base, and the callers that restate a size
// without --yes are scripts — the bootstrap and the end-to-end journey — which
// no interactive test exercises. (cli#546)
//
// The assertions are deliberately paired: exit 0 AND the apply still happening.
// Either one alone is satisfiable by the wrong fix — reverting the staleness
// treatment would give exit 0 with no re-stamp, and #539 as merged gives the
// re-stamp only to callers who pass --yes.
func TestSet_SameCeilingNeedsNoYes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provenance string
	}{
		{"installer-sized edge", "installer"},
		{"pre-marker edge", ""},
		{"junk marker", "banana"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, values := fakeHelmValues(t)
			env := map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi"}
			if tc.provenance != "" {
				env["RESOURCE_PROVENANCE"] = tc.provenance
			}
			cs := csWith("8", "32Gi", env)
			// pr == nil is a non-TTY script; no `yes` field is set.
			out, err := runSet(t, cs, nil, setReq{
				cores: "4", memory: "16", coresSet: true, memSet: true,
			})
			if err != nil {
				t.Fatalf("restating the current ceiling must not need --yes, got: %v\n%s", err, out)
			}
			if !helmUpgraded(*calls) {
				t.Errorf("the re-stamp must still happen without --yes — otherwise the fix\n"+
					"just reverted backend#2220 for every script:\n%s", out)
			}
			// And the marker that lands is `user`, read off the values file helm
			// was actually handed rather than inferred from the prose.
			if !strings.Contains(*values, "RESOURCE_PROVENANCE") ||
				!strings.Contains(*values, "user") {
				t.Errorf("values handed to helm do not stamp RESOURCE_PROVENANCE=user:\n%s", *values)
			}
			// The numbers are untouched: this is a bookkeeping write, not a resize.
			if !strings.Contains(*values, "cpu=4,memory=16Gi") {
				t.Errorf("a bookkeeping write must not move the ceiling:\n%s", *values)
			}
		})
	}

	// A GPU-less machine whose cluster still carries the chart-default
	// GPU_REQUESTS reaches the same fall-through for the same reason (#241), and
	// was broken by the same missing clause. Scripts must be able to clear it.
	t.Run("phantom GPU cleanup needs no --yes", func(t *testing.T) {
		calls, values := fakeHelmValues(t)
		cs := csWith("8", "32Gi", map[string]string{
			"RESOURCE_LIMITS":     "cpu=4,memory=16Gi",
			"GPU_REQUESTS":        "nvidia.com/gpu=1",
			"RESOURCE_PROVENANCE": "user", // isolate the phantom GPU as the only reason
		}) // no GPU on the node
		out, err := runSet(t, cs, nil, setReq{
			cores: "4", memory: "16", coresSet: true, memSet: true,
		})
		if err != nil {
			t.Fatalf("clearing a phantom GPU must not need --yes, got: %v\n%s", err, out)
		}
		if !helmUpgraded(*calls) {
			t.Errorf("the phantom-GPU cleanup must still apply without --yes:\n%s", out)
		}
		if !strings.Contains(*values, "GPU_REQUESTS") {
			t.Errorf("values must carry the explicit-empty GPU override:\n%s", *values)
		}
	})

	// The gate is skipped because the CEILING is unchanged — not because the
	// prompter is absent. An operator who would decline still gets the
	// bookkeeping write, because there was never a budget question to decline.
	// (fakePrompter.Confirm returning false is the only observable proof the
	// gate was not entered: entering it would cleanCancel and shell no helm.)
	t.Run("not even asked on a terminal", func(t *testing.T) {
		calls := fakeHelm(t)
		pr := &fakePrompter{confirm: boolPtr(false)}
		cs := csWith("8", "32Gi", map[string]string{
			"RESOURCE_LIMITS":     "cpu=4,memory=16Gi",
			"RESOURCE_PROVENANCE": "installer",
		})
		out, err := runSet(t, cs, pr, setReq{
			cores: "4", memory: "16", coresSet: true, memSet: true,
		})
		if err != nil {
			t.Fatalf("same-ceiling set on a terminal: %v\n%s", err, out)
		}
		if !helmUpgraded(*calls) {
			t.Errorf("a declining prompter proves the confirm gate was entered; it must\n"+
				"be skipped when the ceiling is unchanged:\n%s", out)
		}
		if strings.Contains(out, "nothing was changed") {
			t.Errorf("there was no budget question to decline:\n%s", out)
		}
	})

	// The narrow-fix guard: a REAL change off a terminal still refuses without
	// --yes, so the clause above cannot have disarmed the gate wholesale.
	// TestSet_OffTTYWithFlagsNeedsYes covers the same contract from the other
	// side; this keeps the pair adjacent to the fix it bounds.
	t.Run("a real change still needs --yes", func(t *testing.T) {
		calls := fakeHelm(t)
		cs := csWith("8", "32Gi", map[string]string{
			"RESOURCE_LIMITS":     "cpu=4,memory=16Gi",
			"RESOURCE_PROVENANCE": "installer",
		})
		_, err := runSet(t, cs, nil, setReq{cores: "6", coresSet: true})
		if got := exitCode(t, err); got != 1 {
			t.Fatalf("a changed ceiling off a terminal must still exit 1, got %d (%v)", got, err)
		}
		if helmUpgraded(*calls) {
			t.Error("an unconfirmed CHANGE must mutate nothing")
		}
	})
}

// TestSet_NoOpEvenWhenCurrentNoLongerFits: restating the ceiling that's already
// applied must stay a clean no-op success even when the machine has SHRUNK under
// it (smaller Docker Desktop VM, lost node) — the no-op check runs before the
// fit validation, because leaving things unchanged mutates nothing. An actual
// change on the same shrunken machine is still fit-checked.
func TestSet_NoOpEvenWhenCurrentNoLongerFits(t *testing.T) {
	// Node 4 CPU / 8 GiB, but the cluster already runs with cpu=8,memory=16Gi.
	// Marked `user` so this stays the clean-no-op case it is testing; the
	// stale-marker fall-through has its own test (backend#2220).
	cur := map[string]string{
		"RESOURCE_LIMITS":     "cpu=8,memory=16Gi",
		"RESOURCE_PROVENANCE": "user",
	}

	t.Run("flags restating the current ceiling", func(t *testing.T) {
		calls := fakeHelm(t)
		cs := csWith("4", "8Gi", cur)
		out, err := runSet(t, cs, nil, setReq{cores: "8", memory: "16", coresSet: true, memSet: true, yes: true})
		if err != nil {
			t.Fatalf("unchanged ceiling must be a no-op even when it no longer fits, got: %v\n%s", err, out)
		}
		if !strings.Contains(out, "nothing to change") {
			t.Errorf("no-op message missing:\n%s", out)
		}
		for _, c := range *calls {
			if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
				t.Errorf("no-op must NOT call helm upgrade: %v", c)
			}
		}
	})

	t.Run("wizard leave-as-is", func(t *testing.T) {
		calls := fakeHelm(t)
		pr := &fakePrompter{answers: map[string]string{"How much may one training run use?": "Leave it as it is"}}
		cs := csWith("4", "8Gi", cur)
		out, err := runSet(t, cs, pr, setReq{})
		if err != nil {
			t.Fatalf("leave-as-is must be a no-op even when the current ceiling no longer fits, got: %v\n%s", err, out)
		}
		if !strings.Contains(out, "nothing to change") {
			t.Errorf("no-op message missing:\n%s", out)
		}
		for _, c := range *calls {
			if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
				t.Errorf("leave-as-is must NOT call helm upgrade: %v", c)
			}
		}
	})

	t.Run("an actual change is still fit-checked", func(t *testing.T) {
		cs := csWith("4", "8Gi", cur)
		_, err := runSet(t, cs, nil, setReq{cores: "6", coresSet: true, yes: true})
		if got := exitCode(t, err); got != 2 {
			t.Fatalf("a changed, unfitting request must still exit 2, got %d (%v)", got, err)
		}
	})
}

// TestSet_DryRunAppliesNothing: --dry-run prints the plan and never shells helm.
func TestSet_DryRunAppliesNothing(t *testing.T) {
	calls := fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	out, err := runSet(t, cs, nil, setReq{cores: "4", coresSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("dry-run must not shell helm, got %v", *calls)
	}
	for _, want := range []string{"Dry run", "helm upgrade tb", "cpu=4,memory=8Gi"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

// failingHelm installs a helm.Runner whose mutating `helm upgrade` returns err —
// the reuse-flag probe and repo calls still succeed — so the apply path can be
// driven into its failure / interrupt handling without a real cluster.
func failingHelm(t *testing.T, err error) {
	t.Helper()
	orig := helm.Runner
	helm.Runner = func(_ context.Context, name string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "upgrade" && args[1] == "--help" {
			return "  --reset-then-reuse-values", nil // reuse-flag probe
		}
		if len(args) >= 2 && args[0] == "upgrade" { // the mutating upgrade (release != "--help")
			return "boom", err
		}
		return "", nil // repo list / add / update
	}
	t.Cleanup(func() { helm.Runner = orig })
}

// TestSet_InterruptReportedNotFailed: a Ctrl-C / cancelled context mid-`helm
// upgrade` must exit 130 with an honest "may have applied" note — NOT exit 1
// "helm upgrade failed", which reads as "nothing changed" when the change may
// actually be live (backend#2255).
func TestSet_InterruptReportedNotFailed(t *testing.T) {
	failingHelm(t, errors.New("signal: killed"))
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the SIGINT handler already propagated the cancel

	var buf bytes.Buffer
	err := applyResourcesSet(ctx, ui.New(&buf, ui.WithColor(false)), nil,
		setTarget(cs), cluster.KubeconfigOptions{Path: "/tmp/kc"},
		setReq{cores: "4", coresSet: true, yes: true})

	if got := exitCode(t, err); got != exitInterrupted {
		t.Fatalf("an interrupted apply must exit %d, got %d (%v)\n%s", exitInterrupted, got, err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "Interrupted") || !strings.Contains(out, "may already have applied") {
		t.Errorf("interrupt output should say the change may already have applied:\n%s", out)
	}
	if strings.Contains(out, "helm upgrade failed") {
		t.Errorf("must not frame an interrupt as a helm failure:\n%s", out)
	}
}

// TestSet_GenuineHelmFailureExitsFailure: a real helm failure (live context, a
// plain non-130 error) still exits with the failure code and "helm upgrade
// failed" — the interrupt handling must not swallow real failures (backend#2255).
func TestSet_GenuineHelmFailureExitsFailure(t *testing.T) {
	failingHelm(t, errors.New("boom"))
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})

	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), nil,
		setTarget(cs), cluster.KubeconfigOptions{Path: "/tmp/kc"},
		setReq{cores: "4", coresSet: true, yes: true})

	if got := exitCode(t, err); got != exitFailure {
		t.Fatalf("a genuine helm failure must exit %d, got %d (%v)", exitFailure, got, err)
	}
	if !strings.Contains(err.Error(), "helm upgrade failed") {
		t.Errorf("a genuine failure should read as a helm failure: %v", err)
	}
}

// timedOutHelm installs a helm.Runner whose mutating `helm upgrade` fails with
// helm's `--wait` readiness-timeout signature in its combined output — the
// "values applied, but pods not ready in time" case (backend#2587). The reuse-flag
// probe and repo calls still succeed.
func timedOutHelm(t *testing.T) {
	t.Helper()
	orig := helm.Runner
	helm.Runner = func(_ context.Context, name string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "upgrade" && args[1] == "--help" {
			return "  --reset-then-reuse-values", nil // reuse-flag probe
		}
		if len(args) >= 2 && args[0] == "upgrade" { // the mutating upgrade
			return "Error: UPGRADE FAILED: timed out waiting for the condition", errors.New("exit status 1")
		}
		return "", nil // repo list / add / update
	}
	t.Cleanup(func() { helm.Runner = orig })
}

// TestSet_WaitTimeoutReportedAsApplied: a `helm upgrade --wait` readiness timeout
// committed the values, so `resources set` must report it as APPLIED — exit 0 with
// a reassuring "may still be rolling" note — never exit 1 "helm upgrade failed",
// which reads as "nothing changed" (backend#2587, sibling of the interrupt case).
func TestSet_WaitTimeoutReportedAsApplied(t *testing.T) {
	timedOutHelm(t)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})

	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), nil,
		setTarget(cs), cluster.KubeconfigOptions{Path: "/tmp/kc"},
		setReq{cores: "4", coresSet: true, yes: true})

	if err != nil {
		t.Fatalf("a --wait readiness timeout applied the values — must not error, got %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "the change is applied") || !strings.Contains(out, "may still be rolling") {
		t.Errorf("timeout output should reassure the change applied and may still be rolling:\n%s", out)
	}
	if strings.Contains(out, "helm upgrade failed") {
		t.Errorf("must not frame an applied-but-not-ready timeout as a helm failure:\n%s", out)
	}
}

// TestSet_PreUpgradeInterruptIsQuiet: a Ctrl-C during the repo add/update phase —
// BEFORE the mutating `helm upgrade` — exits quietly (130) like the rest of the
// CLI, not a scary exit-1 "context canceled". But since nothing was applied yet,
// it must NOT claim the change may have applied (backend#2255).
func TestSet_PreUpgradeInterruptIsQuiet(t *testing.T) {
	orig := helm.Runner
	helm.Runner = func(_ context.Context, name string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "upgrade" && args[1] == "--help" {
			return "  --reset-then-reuse-values", nil // reuse-flag probe
		}
		if len(args) >= 2 && args[0] == "repo" && args[1] == "update" {
			return "boom", errors.New("signal: killed") // Ctrl-C mid repo update
		}
		return "", nil
	}
	t.Cleanup(func() { helm.Runner = orig })

	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	err := applyResourcesSet(ctx, ui.New(&buf, ui.WithColor(false)), nil,
		setTarget(cs), cluster.KubeconfigOptions{Path: "/tmp/kc"},
		setReq{cores: "4", coresSet: true, yes: true})

	if got := exitCode(t, err); got != exitInterrupted {
		t.Fatalf("a pre-upgrade interrupt must exit %d, got %d (%v)\n%s", exitInterrupted, got, err, buf.String())
	}
	if strings.Contains(buf.String(), "may already have applied") {
		t.Errorf("nothing was applied before the upgrade ran — must not claim it may have:\n%s", buf.String())
	}
}

// TestSet_ShowsProgressOnRealApply: a real (non-dry-run) apply prints a live wait
// line, so the run isn't invisible while `helm upgrade --wait` runs and a Ctrl-C
// mid-flight isn't a mystery (backend#2255, requirement #2).
func TestSet_ShowsProgressOnRealApply(t *testing.T) {
	fakeHelm(t) // every call succeeds
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})

	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), nil,
		setTarget(cs), cluster.KubeconfigOptions{Path: "/tmp/kc"},
		setReq{cores: "4", coresSet: true, yes: true})
	if err != nil {
		t.Fatalf("real apply: %v", err)
	}
	if !strings.Contains(buf.String(), "Applying the resource change") {
		t.Errorf("a real apply should surface a progress line:\n%s", buf.String())
	}
}

// --- wizard -----------------------------------------------------------------

// TestWizard_PreselectedMax: on a terminal with no flags, accepting the default
// Select choice ("use as much as possible") sizes the run to machine − overhead.
func TestWizard_PreselectedMax(t *testing.T) {
	pr := &fakePrompter{confirm: boolPtr(true)} // Select returns the default (optMax)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), pr,
		setTarget(cs), cluster.KubeconfigOptions{}, setReq{dryRun: true})
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	if !strings.Contains(buf.String(), "cpu=7,memory=29Gi") {
		t.Errorf("preselected max should size to 7/29:\n%s", buf.String())
	}
}

// TestWizard_ChooseAnAmount: picking "Choose an amount" runs the bounded prompts
// and applies the typed cores/memory.
func TestWizard_ChooseAnAmount(t *testing.T) {
	pr := &fakePrompter{
		answers: map[string]string{
			"How much may one training run use?": "Choose an amount",
			"CPU cores for one run (1–7)":        "3",
			"Memory for one run in GiB (2–29)":   "12",
		},
		confirm: boolPtr(true),
	}
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), pr,
		setTarget(cs), cluster.KubeconfigOptions{}, setReq{dryRun: true})
	if err != nil {
		t.Fatalf("wizard choose: %v", err)
	}
	if !strings.Contains(buf.String(), "cpu=3,memory=12Gi") {
		t.Errorf("chosen amount should be 3/12:\n%s", buf.String())
	}
}

// TestWizard_DefaultsClampedToShrunkMachine (#398): on a machine that shrank
// under the configured ceiling (the WSL2 field case: node smaller than the
// chart-default 8Gi RESOURCE_LIMITS), pressing Enter on every prompt must
// succeed — the offered defaults are clamped into their own valid ranges.
// Before the clamp, accepting the memory default "(8)" was rejected by its own
// validator ("must be between 2 and N").
func TestWizard_DefaultsClampedToShrunkMachine(t *testing.T) {
	pr := &fakePrompter{
		answers: map[string]string{
			"How much may one training run use?": "Choose an amount",
			// CPU + memory prompts deliberately NOT scripted: the fake returns
			// each prompt's DEFAULT — exactly "the user pressed Enter".
		},
		confirm: boolPtr(true),
	}
	// 12-CPU / 7-GiB node → maxCores 11, maxGiB 4; current cpu=2 fits, memory
	// 8Gi exceeds → default must clamp 8 → 4.
	cs := csWith("12", "7Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), pr,
		setTarget(cs), cluster.KubeconfigOptions{}, setReq{dryRun: true})
	if err != nil {
		t.Fatalf("Enter-on-defaults must succeed on a shrunk machine: %v", err)
	}
	if !strings.Contains(buf.String(), "cpu=2,memory=4Gi") {
		t.Errorf("clamped defaults should apply cpu=2,memory=4Gi:\n%s", buf.String())
	}
	// The header must not present the stale ceiling as a bare ">100%" fact.
	if !strings.Contains(buf.String(), "more than this machine can give one run now") {
		t.Errorf("header should annotate the over-ceiling budget:\n%s", buf.String())
	}
}

// TestWizard_ChooseAnAmountTooSmallMachine: on a machine too small to give a run
// even the 1-core / 2-GiB minimum after tracebloc's overhead, the "Choose an
// amount" path must NOT prompt an impossible range (e.g. "1–0") that rejects
// every answer and traps the user — it fails honestly with exit 2. Bugbot #241.
func TestWizard_ChooseAnAmountTooSmallMachine(t *testing.T) {
	pr := &fakePrompter{
		answers: map[string]string{"How much may one training run use?": "Choose an amount"},
	}
	// 1 CPU / 2 GiB node: after the ~1-core, ~3-GiB overhead, a run can have
	// nothing — maxCores/maxGiB drop below the 1-core/2-GiB prompt floor.
	cs := csWith("1", "2Gi", map[string]string{"RESOURCE_LIMITS": "cpu=1,memory=1Gi"})
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), pr,
		setTarget(cs), cluster.KubeconfigOptions{}, setReq{})
	if got := exitCode(t, err); got != 2 {
		t.Fatalf("too-small-machine 'Choose an amount' must exit 2 (not loop), got %d (%v)\n%s", got, err, buf.String())
	}
	if !strings.Contains(err.Error(), "too small") {
		t.Errorf("expected an honest 'too small' message, got: %v", err)
	}
}

// TestWizard_GPURowOmittedWhenNoGPU: the wizard header shows no GPU line on a
// GPU-less machine, and the Select still preselects max.
func TestWizard_GPURowOmittedWhenNoGPU(t *testing.T) {
	pr := &fakePrompter{confirm: boolPtr(true)}
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	var buf bytes.Buffer
	if err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), pr,
		setTarget(cs), cluster.KubeconfigOptions{}, setReq{dryRun: true}); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	out := buf.String()
	// No GPU should surface in the HUMAN-facing output (the wizard header row or
	// the size echo) on a GPU-less machine. The raw values dump below legitimately
	// carries the explicit no-GPU keys (FIX 2), so scope this to the text before
	// the `values:` block.
	human := out
	if i := strings.Index(out, "values:"); i >= 0 {
		human = out[:i]
	}
	if strings.Contains(human, "GPU") {
		t.Errorf("no GPU row/echo expected in the human-facing output on a GPU-less machine:\n%s", human)
	}
	// And a phantom GPU must never be written, anywhere.
	if strings.Contains(out, "nvidia.com/gpu") {
		t.Errorf("no phantom GPU should be written on a GPU-less machine:\n%s", out)
	}
	// Show block should carry the CPU + Memory "x of N" lines.
	for _, want := range []string{"of 8 cores", "of 32 GiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("wizard header missing %q:\n%s", want, out)
		}
	}
}

// TestWizard_LeaveAsIs: "Leave it as it is" is a no-op (no helm upgrade).
func TestWizard_LeaveAsIs(t *testing.T) {
	calls := fakeHelm(t)
	pr := &fakePrompter{answers: map[string]string{"How much may one training run use?": "Leave it as it is"}}
	// Marked `user` so "leave it as it is" is the clean no-op this test is about.
	// On an edge whose size is NOT yet recorded as the operator's choice, picking
	// "leave it as it is" does still persist — the marker is being corrected, and
	// TestSet_SameCeilingStampsProvenance covers that (backend#2220).
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS":     "cpu=4,memory=16Gi",
		"RESOURCE_PROVENANCE": "user",
	})
	out, err := runSet(t, cs, pr, setReq{})
	if err != nil {
		t.Fatalf("wizard leave: %v", err)
	}
	for _, c := range *calls {
		if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
			t.Errorf("leave-as-is must not upgrade: %v", c)
		}
	}
	if !strings.Contains(out, "nothing to change") {
		t.Errorf("leave-as-is should be a no-op:\n%s", out)
	}
}

// cancellingPrompter aborts the first question asked the way surveyPrompter
// maps Ctrl-C: errInteractiveCancelled from every prompt.
type cancellingPrompter struct{}

func (cancellingPrompter) Input(string, string, string, func(string) error) (string, error) {
	return "", errInteractiveCancelled
}
func (cancellingPrompter) Select(string, string, []string, string) (string, error) {
	return "", errInteractiveCancelled
}
func (cancellingPrompter) Confirm(string, bool) (bool, error) {
	return false, errInteractiveCancelled
}

// TestWizard_CtrlCIsACleanCancel: Ctrl-C at a wizard prompt (survey's interrupt,
// mapped to errInteractiveCancelled) is a choice, not a failure — exit 0 with
// the standard "Cancelled" note and no helm mutation, matching the clean-cancel
// convention of the other prompting commands (data ingest/delete, offboard).
func TestWizard_CtrlCIsACleanCancel(t *testing.T) {
	calls := fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	out, err := runSet(t, cs, cancellingPrompter{}, setReq{})
	if err != nil {
		t.Fatalf("Ctrl-C during the wizard must be a clean cancel (nil error → exit 0), got: %v", err)
	}
	if !strings.Contains(out, "Cancelled — nothing was changed.") {
		t.Errorf("cancel note missing:\n%s", out)
	}
	for _, c := range *calls {
		if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
			t.Errorf("a cancelled wizard must not `helm upgrade`: %v", c)
		}
	}
}

// TestSet_ConfirmCtrlCIsACleanCancel: the OTHER interrupt point — Ctrl-C at the
// step-6 "Let each training run use up to …?" confirm (flags route, so the
// wizard never runs) — must behave exactly like answering "No": exit 0, the
// same "Cancelled" note, no helm mutation. A silent exit-0 here would let the
// user believe the change went through.
func TestSet_ConfirmCtrlCIsACleanCancel(t *testing.T) {
	calls := fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	// Flags set, no --yes, on a "terminal" (prompter present) → the only prompt
	// reached is the final confirm, which the prompter interrupts.
	out, err := runSet(t, cs, cancellingPrompter{}, setReq{cores: "4", coresSet: true})
	if err != nil {
		t.Fatalf("Ctrl-C at the confirm must be a clean cancel (nil error → exit 0), got: %v", err)
	}
	if !strings.Contains(out, "Cancelled — nothing was changed.") {
		t.Errorf("cancel note missing — a silent success hides the abort:\n%s", out)
	}
	for _, c := range *calls {
		if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
			t.Errorf("a cancelled confirm must not `helm upgrade`: %v", c)
		}
	}
}

// TestSet_CPUOnlyMachineIgnoresChartDefaultGPU: the chart stamps a default
// GPU_REQUESTS on every install (CPU boxes included); a plain `--cores` change on
// a GPU-less machine must NOT inherit that phantom GPU and fail the GPU fit-check.
// It writes the EXPLICIT no-GPU value (empty string) — never the phantom
// nvidia.com/gpu — and, since no GPU was ever configured here, prints no
// "removed" note.
func TestSet_CPUOnlyMachineIgnoresChartDefaultGPU(t *testing.T) {
	// GPU-less node, but the env carries the chart's default GPU_REQUESTS.
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS": "cpu=2,memory=8Gi",
		"GPU_REQUESTS":    "nvidia.com/gpu=1",
	})
	out, err := runSet(t, cs, nil, setReq{cores: "4", coresSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("a plain --cores change on a GPU-less box must succeed, got: %v", err)
	}
	if strings.Contains(out, "nvidia.com/gpu") {
		t.Errorf("the phantom chart-default GPU must NOT be written on a GPU-less machine:\n%s", out)
	}
	// The GPU keys are written as the explicit no-GPU value so reset-then-reuse
	// can't re-inherit the phantom.
	for _, want := range []string{`GPU_LIMITS: ""`, `GPU_REQUESTS: ""`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected explicit no-GPU value %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "removed") {
		t.Errorf("no GPU was configured, so nothing should be announced as removed:\n%s", out)
	}
	if !strings.Contains(out, "cpu=4,memory=8Gi") {
		t.Errorf("expected CPU-only spec:\n%s", out)
	}
}

// TestSet_PhantomGPUForcesPersistOnNoOpCeiling: on a GPU-less machine whose
// cluster still carries the chart-default phantom GPU, merely RESTATING the
// current CPU/memory ceiling (an otherwise-clean no-op) must NOT short-circuit —
// `set` still persists so BuildEnvSpec's explicit no-GPU override clears the
// phantom, else jobs-manager keeps requesting a nonexistent nvidia.com/gpu
// (unschedulable + false GPU heartbeat). Bugbot #241 flagged the no-op path
// skipping this fix; the change path is covered above.
func TestSet_PhantomGPUForcesPersistOnNoOpCeiling(t *testing.T) {
	// GPU-less node; env carries the chart-default phantom GPU + a ceiling we
	// then restate exactly (cores=4/memory=16 == current).
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS": "cpu=4,memory=16Gi",
		"GPU_REQUESTS":    "nvidia.com/gpu=1",
		"GPU_LIMITS":      "nvidia.com/gpu=1",
	})
	out, err := runSet(t, cs, nil, setReq{cores: "4", memory: "16", coresSet: true, memSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("phantom-GPU restatement must persist the cleanup, got: %v\n%s", err, out)
	}
	if strings.Contains(out, "nothing to change") {
		t.Errorf("phantom GPU must NOT be treated as a clean no-op:\n%s", out)
	}
	if !strings.Contains(out, "no GPU while the cluster still requests one") {
		t.Errorf("expected the phantom-cleanup note explaining why we persist an unchanged ceiling:\n%s", out)
	}
	if strings.Contains(out, "nvidia.com/gpu") {
		t.Errorf("must not re-write the phantom GPU:\n%s", out)
	}
	for _, want := range []string{`GPU_LIMITS: ""`, `GPU_REQUESTS: ""`} {
		if !strings.Contains(out, want) {
			t.Errorf("the no-op path must still write explicit no-GPU %q to clear the phantom:\n%s", want, out)
		}
	}
}

// TestSet_PhantomGPUCleanupNotBlockedByFitCheck: a GPU-less machine that has
// SHRUNK under its current ceiling AND still carries a phantom GPU must still get
// the cleanup — the unchanged-ceiling fit-check is skipped (an unchanged ceiling
// mutates nothing to protect, and clearing the phantom only REMOVES a request),
// so restating the now-unfitting ceiling persists the explicit-empty GPU rather
// than exiting 2. Bugbot #241 flagged the earlier fix falling through to
// validateDesired on the unchanged ceiling.
func TestSet_PhantomGPUCleanupNotBlockedByFitCheck(t *testing.T) {
	// Node 4 CPU / 8 GiB, but the cluster already runs cpu=8/memory=16Gi (shrunk)
	// and carries the chart-default phantom GPU.
	cs := csWith("4", "8Gi", map[string]string{
		"RESOURCE_LIMITS": "cpu=8,memory=16Gi",
		"GPU_REQUESTS":    "nvidia.com/gpu=1",
		"GPU_LIMITS":      "nvidia.com/gpu=1",
	})
	out, err := runSet(t, cs, nil, setReq{cores: "8", memory: "16", coresSet: true, memSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("phantom cleanup on a shrunk machine must NOT be blocked by the fit-check, got: %v\n%s", err, out)
	}
	if strings.Contains(out, "nothing to change") {
		t.Errorf("phantom GPU must not be treated as a clean no-op:\n%s", out)
	}
	for _, want := range []string{`GPU_LIMITS: ""`, `GPU_REQUESTS: ""`} {
		if !strings.Contains(out, want) {
			t.Errorf("the shrunk-machine phantom cleanup must still write explicit no-GPU %q:\n%s", want, out)
		}
	}
}

// --- FIX 1: never run an unpinned upgrade -----------------------------------

// TestSet_RefusesWhenChartVersionUnknown: when the release carries no
// helm.sh/chart version label (ParentRelease.ChartVersion == ""), `set` must
// REFUSE before mutating — an unpinned `helm upgrade tracebloc/client` would
// pull the latest chart and silently bump the whole client.
func TestSet_RefusesWhenChartVersionUnknown(t *testing.T) {
	calls := fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	// Same target as setTarget, but with the chart version unknown.
	target := &clusterTarget{
		Resolved:  &cluster.ResolvedConfig{Namespace: "tracebloc", Context: "my-ctx"},
		Clientset: cs,
		Release:   &cluster.ParentRelease{ReleaseName: "tb", ChartVersion: ""},
	}
	var buf bytes.Buffer
	err := applyResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), nil,
		target, cluster.KubeconfigOptions{Path: "/tmp/kc"}, setReq{cores: "4", coresSet: true, yes: true})
	if exitCode(t, err) != 1 {
		t.Fatalf("want exit 1 when the chart version is unknown, got %v", err)
	}
	for _, want := range []string{"chart version", "installer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message missing %q: %s", want, err.Error())
		}
	}
	// The whole point: refuse BEFORE any helm shell-out (not even the probe).
	for _, c := range *calls {
		if len(c) >= 3 && c[1] == "upgrade" && c[2] != "--help" {
			t.Errorf("must not `helm upgrade` when the chart version is unknown: %v", c)
		}
	}
}

// --- FIX 2: removing a GPU actually removes it ------------------------------

// TestSet_RemovingGPUWritesExplicitNoGPUValue: on a GPU machine, `--gpus 0`
// must WRITE the explicit no-GPU value ("") into the plan values so
// --reset-then-reuse-values OVERRIDES the stored nvidia.com/gpu=1 — the fix for
// the silent no-op where the GPU stayed put while the echo claimed it was gone.
func TestSet_RemovingGPUWritesExplicitNoGPUValue(t *testing.T) {
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS": "cpu=2,memory=8Gi",
		"GPU_REQUESTS":    "nvidia.com/gpu=1",
		"GPU_LIMITS":      "nvidia.com/gpu=1",
	}, "nvidia.com/gpu", "1")
	out, err := runSet(t, cs, nil, setReq{gpus: 0, gpusSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("removing the GPU should succeed: %v\n%s", err, out)
	}
	for _, want := range []string{`GPU_LIMITS: ""`, `GPU_REQUESTS: ""`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected explicit no-GPU override %q:\n%s", want, out)
		}
	}
	// The stale count must be OVERRIDDEN, not left behind in the values.
	if strings.Contains(out, "nvidia.com/gpu") {
		t.Errorf("stale nvidia.com/gpu must be overridden, not left in the values:\n%s", out)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("dry-run should state the GPU is removed:\n%s", out)
	}
}

// TestSet_RemovingGPUEchoIsHonest: on the real apply path, removing the GPU
// prints an honest "GPU access removed" note — true only because the written
// values actually remove it.
func TestSet_RemovingGPUEchoIsHonest(t *testing.T) {
	fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS": "cpu=2,memory=8Gi",
		"GPU_REQUESTS":    "nvidia.com/gpu=1",
		"GPU_LIMITS":      "nvidia.com/gpu=1",
	}, "nvidia.com/gpu", "1")
	out, err := runSet(t, cs, nil, setReq{gpus: 0, gpusSet: true, yes: true})
	if err != nil {
		t.Fatalf("removing the GPU should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "GPU access removed") {
		t.Errorf("success echo should honestly announce GPU removal:\n%s", out)
	}
}

// TestSet_UntouchedGPUIsKept: a plain --cores change on a GPU machine leaves the
// GPU dimension untouched — it must be KEPT (re-written as nvidia.com/gpu=1),
// and nothing is announced as removed.
func TestSet_UntouchedGPUIsKept(t *testing.T) {
	cs := csWith("8", "32Gi", map[string]string{
		"RESOURCE_LIMITS": "cpu=2,memory=8Gi",
		"GPU_REQUESTS":    "nvidia.com/gpu=1",
		"GPU_LIMITS":      "nvidia.com/gpu=1",
	}, "nvidia.com/gpu", "1")
	out, err := runSet(t, cs, nil, setReq{cores: "4", coresSet: true, dryRun: true, yes: true})
	if err != nil {
		t.Fatalf("cpu-only change on a GPU machine should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, `GPU_LIMITS: "nvidia.com/gpu=1"`) {
		t.Errorf("an untouched GPU must be kept, not dropped:\n%s", out)
	}
	if strings.Contains(out, "removed") {
		t.Errorf("nothing was removed — don't claim it:\n%s", out)
	}
}

func boolPtr(b bool) *bool { return &b }

// proceedingPrompter answers the final confirm "yes"; the wizard prompts are
// unused on the flag-driven path.
type proceedingPrompter struct{}

func (proceedingPrompter) Input(string, string, string, func(string) error) (string, error) {
	return "", errInteractiveCancelled
}
func (proceedingPrompter) Select(string, string, []string, string) (string, error) {
	return "", errInteractiveCancelled
}
func (proceedingPrompter) Confirm(string, bool) (bool, error) { return true, nil }

// TestSet_ConfirmOpensWithSingleBlank: after the banner removal (#375) the
// flag-driven confirm path must still open with exactly ONE blank line.
// PromptHint self-leads with a newline, so a preceding Newline() stacked two —
// the command opened with a double blank (Bugbot #375). Pins the single-blank
// opening so the redundant Newline() can't creep back.
func TestSet_ConfirmOpensWithSingleBlank(t *testing.T) {
	fakeHelm(t)
	cs := csWith("8", "32Gi", map[string]string{"RESOURCE_LIMITS": "cpu=2,memory=8Gi"})
	out, err := runSet(t, cs, proceedingPrompter{}, setReq{cores: "4", coresSet: true})
	if err != nil {
		t.Fatalf("flag-driven confirm + proceed should succeed: %v\n%s", err, out)
	}
	head := out
	if len(head) > 48 {
		head = head[:48]
	}
	// PromptHint emits "\n  <hint>\n": exactly one leading newline, then two
	// spaces. A double blank ("\n\n…") is the regression.
	if !strings.HasPrefix(out, "\n  ") {
		t.Errorf("confirm path must open with a single blank line then the hint, got %q", head)
	}
	if strings.HasPrefix(out, "\n\n") {
		t.Errorf("confirm path opens with a DOUBLE blank line (banner-removal regression): %q", head)
	}
}

// TestSet_MultiClientRedirectOpensWithSingleBlank is the §380 multi-client mirror
// of TestSet_ConfirmOpensWithSingleBlank: it drives the OUTER runResourcesSet
// through the REAL resolveClusterTarget (loadClusterFn/newClientsetFn seams — NOT
// applyResourcesSet directly, the way runSet does), so discoverRelease's redirect
// actually fires. The kubeconfig default namespace hosts no client; the scan
// finds exactly one elsewhere; the redirect note (leadRedirect=true) self-leads
// its single blank, then the confirm PromptHint self-leads its own. The open must
// be exactly ONE leading blank then the note — never zero (the original
// "missing lead blank on set" bug) and never the #375 double.
func TestSet_MultiClientRedirectOpensWithSingleBlank(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir()) // no active-client binding → scan allowed
	fakeHelm(t)
	// Ready node for the fit-check, plus a chart-labeled jobs-manager in a
	// NON-default namespace so the scan retargets there.
	cs := fake.NewClientset(resNode("n1", "8", "32Gi"), jmDep("lukas-01"))
	withClusterSeams(t, cs)

	var buf bytes.Buffer
	// No --context/--namespace so allowScan stays true; a flag-driven change
	// (cores 4, up from the chart-default 2) so it's not a no-op and reaches the
	// confirm the proceedingPrompter accepts.
	err := runResourcesSet(context.Background(), ui.New(&buf, ui.WithColor(false)), proceedingPrompter{},
		cluster.KubeconfigOptions{}, setReq{cores: "4", coresSet: true})
	if err != nil {
		t.Fatalf("runResourcesSet (multi-client): %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "lukas-01") {
		t.Fatalf("redirect must actually run (note naming the retargeted namespace), got:\n%s", out)
	}
	head := out
	if len(head) > 64 {
		head = head[:64]
	}
	if !strings.HasPrefix(out, "\n  ") {
		t.Errorf("multi-client set must open with a single blank then the redirect, got %q", head)
	}
	if strings.HasPrefix(out, "\n\n") {
		t.Errorf("multi-client set opens with a DOUBLE blank line: %q", head)
	}
}

// TestWizard_NeverOffersMoreThanTheMachineCanHonour is backend#2221's fourth
// scope item — "never offer a ladder rung the VM cannot honour" — asserted as an
// INVARIANT rather than as the two examples that covered it.
//
// Why a swept test and not a third example. The existing coverage
// (TestWizard_DefaultsClampedToShrunkMachine, TestWizard_ChooseAnAmountTooSmallMachine)
// pins two machine shapes. The property #2221 actually asks for is universal: on
// ANY machine, whatever the wizard applies must be something that machine can
// still give one run. A future rung ladder (the epic's XS 4 · S 16 · M 32 · L 64 ·
// XL 128 GiB decision of record) is exactly the kind of change that satisfies both
// examples and breaks the property — offer a fixed rung, and on a small VM it is
// unhonourable by construction.
//
// The k3d row is #2221's own measurement: a Docker Desktop VM of 6 CPU / 11.67 GiB
// which two uncapped node containers each report in full. Bounding on the node is
// honest for ONE run precisely because a node's allocatable can never exceed the
// VM's — the double-count only misleads about CONCURRENCY, which is backend#2419's
// admission gate rather than this prompt's business.
func TestWizard_NeverOffersMoreThanTheMachineCanHonour(t *testing.T) {
	for _, tc := range []struct {
		name             string
		nodeCPU, nodeMem string
	}{
		{"k3d on a Docker Desktop VM (#2221's measurement)", "6", "11934Mi"},
		{"a small laptop VM", "4", "8Gi"},
		{"an 8-core workstation", "8", "32Gi"},
		{"a large host", "64", "256Gi"},
		{"an awkward, non-power-of-two VM", "3", "6600Mi"},
		{"barely above the floor", "2", "6Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// "Use as much as possible" — the pre-selected answer, so this is the
			// path most runs take.
			// THE FULL OPTION STRING, not a prefix. My first version answered
			// "Use as much as possible", which matches NO option — the fake then
			// returned the default, the wizard left the budget alone, and the
			// assertion below matched the CURRENT value instead of an offered one.
			// The test passed on every machine and survived a mutation that made
			// the wizard offer a fixed 8-core / 32-GiB rung: exactly the case it
			// exists to catch. A test that never reaches the code under test
			// cannot fail for it.
			pr := &fakePrompter{
				answers: map[string]string{
					"How much may one training run use?": "Use as much as possible (recommended if this machine is just for tracebloc)",
				},
				confirm: boolPtr(true),
			}
			// A current budget that every shape here could also honour — which is
			// exactly why it is NOT the no-op backstop. My first version claimed it
			// was: "if the wizard left it alone the floor check would fail". It
			// would not. 1 core / 3 GiB sits at and above the 1-core / 2-GiB floor,
			// so a silent no-op passed every assertion (Cursor Bugbot Medium).
			//
			// The real backstop is the equality check below: "use as much as
			// possible" must apply EXACTLY the machine's maximum, so a no-op (which
			// applies the current budget) and an over-offer both fail. That is the
			// contract of the option being answered, and it cannot be satisfied by
			// leaving things alone.
			cs := csWith(tc.nodeCPU, tc.nodeMem,
				map[string]string{"RESOURCE_LIMITS": "cpu=1,memory=3Gi"})
			var buf bytes.Buffer
			err := applyResourcesSet(context.Background(),
				ui.New(&buf, ui.WithColor(false)), pr,
				setTarget(cs), cluster.KubeconfigOptions{}, setReq{dryRun: true})

			node := resources.Machine{
				CPU: mustQty(t, tc.nodeCPU),
				Mem: mustQty(t, tc.nodeMem),
			}
			maxCores := resources.MaxRunCores(node)
			maxGiB := resources.MaxRunGiB(node)

			// A machine that cannot seat the floor must FAIL HONESTLY rather than
			// offer something unhonourable — the Bugbot #241 rule, restated as
			// part of the same invariant.
			if maxCores < 1 || maxGiB < 2 {
				if err == nil {
					t.Fatalf("machine cannot seat the floor (maxCores=%d maxGiB=%d) "+
						"but the wizard applied something:\n%s", maxCores, maxGiB, buf.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("wizard failed on a machine that can seat a run "+
					"(maxCores=%d maxGiB=%d): %v\n%s", maxCores, maxGiB, err, buf.String())
			}

			gotCores, gotGiB := appliedCPUAndGiB(t, buf.String())
			if gotCores > maxCores {
				t.Errorf("offered %d cores, machine can give one run at most %d",
					gotCores, maxCores)
			}
			if gotGiB > maxGiB {
				t.Errorf("offered %d GiB, machine can give one run at most %d",
					gotGiB, maxGiB)
			}
			// And it must not have collapsed below the per-run floor either: an
			// offer of nothing is not "bounded", it is broken.
			if gotCores < 1 || gotGiB < 2 {
				t.Errorf("offered %d cores / %d GiB, below the 1-core / 2-GiB floor",
					gotCores, gotGiB)
			}
			// THE NO-OP BACKSTOP, and the reason the bound above is not enough on
			// its own: "use as much as possible" has an exact contract, so anything
			// that quietly declines to offer — a prompt string that matches no
			// option, a future rung ladder, a silent early return — lands on the
			// current budget instead of the maximum and fails here. A bound alone
			// is satisfied by offering nothing.
			if gotCores != maxCores || gotGiB != maxGiB {
				t.Errorf("max-out applied %d cores / %d GiB, want exactly the "+
					"machine's %d / %d — a no-op or a fixed rung would look like this",
					gotCores, gotGiB, maxCores, maxGiB)
			}
		})
	}
}

func mustQty(t *testing.T, s string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(s)
	if err != nil {
		t.Fatalf("ParseQuantity(%q): %v", s, err)
	}
	return q
}

// appliedCPUAndGiB reads the cpu=N,memory=MGi the dry run says it would apply.
// Parsed from the output rather than returned by applyResourcesSet, because that
// output is what a user acts on — a bound the code respects internally and
// misreports is still a bound that misleads.
func appliedCPUAndGiB(t *testing.T, out string) (cores, gib int) {
	t.Helper()
	m := regexp.MustCompile(`cpu=(\d+),memory=(\d+)Gi`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no cpu=N,memory=MGi in the output:\n%s", out)
	}
	c, _ := strconv.Atoi(m[1])
	g, _ := strconv.Atoi(m[2])
	return c, g
}
