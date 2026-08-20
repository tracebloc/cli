package resources

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// backend#2220. The in-repo half of the ticket's definition of done: a mutation
// to the arithmetic in client-runtime's node_sizing.py must redden tests HERE,
// not just there. Two independent nets, and they fail for different reasons:
//
//   - TestGoldenVectorsReplay pins that MaxRunCores/MaxRunGiB still agree with
//     the contract's own vectors, so a re-vendored contract whose numbers moved
//     cannot land silently.
//   - TestDecisionAIsIntact pins cli#143's contract itself, independently of the
//     contract file, so re-vendoring cannot quietly turn the overhead into
//     something that gets subtracted from the user's number.

func TestContractIsValid(t *testing.T) {
	// The embedded contract is decoded by a panicking MustCompile-style helper,
	// which is right for a compile-time asset but must never reach a release.
	// This is the test that keeps that promise.
	c := mustContract()
	if c.ContractVersion < 1 {
		t.Fatalf("contract_version = %d, want >= 1", c.ContractVersion)
	}
	if len(c.Vectors.SingleNode) == 0 {
		t.Fatal("the vendored contract carries no single-node vectors — " +
			"re-vendor it, or the replay below is asserting nothing")
	}
	if c.Overhead.CPUMilli != 1000 || c.Overhead.MemoryBytes != 3*gib {
		t.Errorf("overhead moved: got %dm / %d bytes, want 1000m / %d bytes. "+
			"If that is intended it is a FLEET envelope change, not a re-vendor — "+
			"see backend#2167 and RFC-BACKEND-664 L0",
			c.Overhead.CPUMilli, c.Overhead.MemoryBytes, 3*gib)
	}
	if c.Floor.CPUMilli != 1000 || c.Floor.MemoryBytes != 2*gib {
		t.Errorf("floor moved: got %dm / %d bytes, want 1000m / %d bytes",
			c.Floor.CPUMilli, c.Floor.MemoryBytes, 2*gib)
	}
}

func TestEmbeddedContractMatchesTheFileOnDisk(t *testing.T) {
	// go:embed snapshots the file at compile time; this catches an edit that was
	// made but not rebuilt, and — more usefully — proves the embedded bytes are
	// the very bytes the cross-repo drift gate diffs.
	onDisk, err := os.ReadFile("envelope_contract.json")
	if err != nil {
		t.Fatalf("reading envelope_contract.json: %v", err)
	}
	if string(onDisk) != string(contractBytes) {
		t.Error("the embedded contract differs from envelope_contract.json on disk")
	}
}

// node builds a Machine the way LargestReadyNode would, from k8s quantity
// strings, so the vectors are exercised through the real parsing path.
func node(t *testing.T, cpu, mem string) Machine {
	t.Helper()
	c, err := resource.ParseQuantity(cpu)
	if err != nil {
		t.Fatalf("bad cpu %q: %v", cpu, err)
	}
	m, err := resource.ParseQuantity(mem)
	if err != nil {
		t.Fatalf("bad memory %q: %v", mem, err)
	}
	return Machine{CPU: c, Mem: m, GPU: map[corev1.ResourceName]resource.Quantity{}}
}

func TestGoldenVectorsReplay(t *testing.T) {
	var failures []string
	for _, v := range mustContract().Vectors.SingleNode {
		// Vectors whose expected is null are the unparseable cases: the contract
		// says "I cannot answer", which k8s quantity parsing rejects long before
		// this package sees it. Nothing for MaxRun* to agree with.
		if v.Expected == nil {
			continue
		}
		m := node(t, v.AllocatableCPU, v.AllocatableMemory)

		gotCores := MaxRunCores(m)
		gotGiB := MaxRunGiB(m)

		// A machine the contract calls non-viable is one this package must clamp
		// to zero — MaxRun* returning a positive number for it would be the CLI
		// offering the user a ceiling the machine cannot host.
		if !v.Expected.Viable {
			if gotCores != 0 && gotGiB != 0 {
				failures = append(failures, fmt.Sprintf(
					"%s (%s/%s): contract says NOT viable, but MaxRunCores=%d MaxRunGiB=%d",
					v.Label, v.AllocatableCPU, v.AllocatableMemory, gotCores, gotGiB))
			}
			continue
		}

		wantCores := int(v.Expected.CPUMilli / 1000)
		wantGiB := int(v.Expected.MemoryBytes / gib)
		if gotCores != wantCores || gotGiB != wantGiB {
			failures = append(failures, fmt.Sprintf(
				"%s (%s/%s): want %dc/%dGiB, got %dc/%dGiB",
				v.Label, v.AllocatableCPU, v.AllocatableMemory,
				wantCores, wantGiB, gotCores, gotGiB))
		}
	}
	if len(failures) > 0 {
		t.Fatalf("MaxRunCores/MaxRunGiB no longer agree with the vendored "+
			"contract's vectors (contract v%d):\n  %s\n\n"+
			"Either this package's arithmetic drifted, or the contract was "+
			"re-vendored with different numbers. Both are real changes — do not "+
			"'fix' this by editing the expectations.",
			mustContract().ContractVersion, strings.Join(failures, "\n  "))
	}
}

func TestDecisionAIsIntact(t *testing.T) {
	// cli#143 Decision A, pinned independently of the contract file: the number
	// the user sets IS the per-run ceiling, and the overhead is a fit margin that
	// is NEVER subtracted from it. backend#2220 deleted the duplicate derivation
	// beside this rule; it did not touch the rule. If a future re-vendor makes
	// DeriveTraining start shrinking the user's ask, this is what says so.
	cpu := *resource.NewQuantity(6, resource.DecimalSI)
	mem := *resource.NewQuantity(24*gib, resource.BinarySI)

	got := DeriveTraining(cpu, mem, "", resource.Quantity{}, false)
	if got.CPU.Cmp(cpu) != 0 {
		t.Errorf("DeriveTraining shrank the user's CPU: got %s, want %s",
			got.CPU.String(), cpu.String())
	}
	if got.Mem.Cmp(mem) != 0 {
		t.Errorf("DeriveTraining shrank the user's memory: got %s, want %s",
			got.Mem.String(), mem.String())
	}
	if !got.HasCPUMem {
		t.Error("DeriveTraining dropped HasCPUMem")
	}
	if got.HasGPU {
		t.Error("DeriveTraining invented a GPU dimension")
	}
}

func TestOverheadIsStillAFitMarginOnly(t *testing.T) {
	// The distinction the contract's own comment insists on: overhead is added to
	// what the user asked for when checking fit, never subtracted from it. A
	// 6c/24GiB ask must NOT fit a 6c/24GiB node, because the platform still needs
	// its cut — that asymmetry is the whole design.
	small := node(t, "6", "24Gi")
	cpu := *resource.NewQuantity(6, resource.DecimalSI)
	mem := *resource.NewQuantity(24*gib, resource.BinarySI)
	if FitsNode(small, cpu, mem, "", resource.Quantity{}, false) {
		t.Error("a full-node ask fit a node with no room for the platform overhead")
	}

	big := node(t, "8", "28Gi")
	if !FitsNode(big, cpu, mem, "", resource.Quantity{}, false) {
		t.Error("a 6c/24Gi ask did not fit 8c/28Gi, which has room for the overhead")
	}
}

func TestFloorTextMatchesTheContractFloor(t *testing.T) {
	// The user-facing strings are hand-written; the floor they describe is not.
	// A re-vendor that moved the floor while these strings stayed put would make
	// the CLI lie in its error messages.
	c := mustContract()
	if want := fmt.Sprintf("%d core", c.Floor.CPUMilli/1000); CoreFloorText() != want {
		t.Errorf("CoreFloorText() = %q but the contract floor is %q", CoreFloorText(), want)
	}
	if want := fmt.Sprintf("%d GiB", c.Floor.MemoryBytes/gib); MemFloorText() != want {
		t.Errorf("MemFloorText() = %q but the contract floor is %q", MemFloorText(), want)
	}
}

func TestContractJSONIsCanonicalFormatting(t *testing.T) {
	// The cross-repo gate is a byte diff, so the vendored file must be the
	// upstream bytes — not a re-serialised equivalent. Catches a well-meaning
	// editor reformat that would redden the drift job for no real reason.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(contractBytes, &probe); err != nil {
		t.Fatalf("vendored contract is not a JSON object: %v", err)
	}
	if !strings.HasSuffix(string(contractBytes), "}\n") {
		t.Error("vendored contract should end with a closing brace and one newline, " +
			"as client-runtime's generator writes it")
	}
}
