package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// backend#2220 scope bullet 4. The marker only works if BOTH writers maintain
// it: the installer stamps installer/unknown, and `resources set` stamps user.
// If this side is missing, an edge the installer marked `installer` and the
// operator then re-sized by hand keeps saying `installer` — a human choice
// wearing the one label that invites a future ladder to overwrite it. That is
// strictly worse than having no marker at all, which is why these are here.

func TestBuildEnvSpecStampsUserProvenance(t *testing.T) {
	cpu := *resource.NewQuantity(6, resource.DecimalSI)
	mem := *resource.NewQuantity(24*gib, resource.BinarySI)

	env := BuildEnvSpec(cpu, mem, "", resource.Quantity{}, false)

	if got := env["RESOURCE_PROVENANCE"]; got != ProvenanceUser {
		t.Errorf("RESOURCE_PROVENANCE = %q, want %q — `resources set` IS the human choice",
			got, ProvenanceUser)
	}
	// The envelope itself must be untouched by the marker (cli#143 Decision A).
	if env["RESOURCE_LIMITS"] != "cpu=6,memory=24Gi" {
		t.Errorf("RESOURCE_LIMITS = %q, want cpu=6,memory=24Gi", env["RESOURCE_LIMITS"])
	}
	if env["RESOURCE_REQUESTS"] != env["RESOURCE_LIMITS"] {
		t.Error("requests and limits diverged — BuildEnvSpec writes them equal so a" +
			" CPU training pod is Guaranteed. (Not \"the chart contract\", as this" +
			" message said until backend#2872: the chart's derive path writes no cpu" +
			" limit at all since backend#2418.)")
	}
}

func TestBuildEnvSpecStampsProvenanceOnTheGPUPathToo(t *testing.T) {
	// Every dimension is always written; the marker must not be an exception on
	// one branch, or a GPU-enabled `resources set` would leave a stale label.
	cpu := *resource.NewQuantity(6, resource.DecimalSI)
	mem := *resource.NewQuantity(24*gib, resource.BinarySI)
	gpu := *resource.NewQuantity(1, resource.DecimalSI)

	env := BuildEnvSpec(cpu, mem, corev1.ResourceName("nvidia.com/gpu"), gpu, true)

	if got := env["RESOURCE_PROVENANCE"]; got != ProvenanceUser {
		t.Errorf("RESOURCE_PROVENANCE = %q on the GPU path, want %q", got, ProvenanceUser)
	}
}

func TestBuildEnvSpecIsUnconditional(t *testing.T) {
	// BuildEnvSpec takes no prior state on purpose, so there is no branch on
	// which it could omit the key. Omitting it would NOT clear it anyway: the
	// apply runs `helm upgrade --reset-then-reuse-values`, which re-applies the
	// release's stored values on top of chart defaults, so an omitted key is
	// silently re-inherited rather than removed. Same reasoning the GPU
	// NoGPUEnvValue carries.
	cpu := *resource.NewQuantity(2, resource.DecimalSI)
	mem := *resource.NewQuantity(8*gib, resource.BinarySI)
	for _, wantGPU := range []bool{true, false} {
		env := BuildEnvSpec(cpu, mem, corev1.ResourceName("nvidia.com/gpu"),
			*resource.NewQuantity(1, resource.DecimalSI), wantGPU)
		if _, ok := env["RESOURCE_PROVENANCE"]; !ok {
			t.Errorf("wantGPU=%v: RESOURCE_PROVENANCE missing entirely", wantGPU)
		}
	}
}

func TestNormalizeProvenance(t *testing.T) {
	cases := map[string]string{
		"installer": ProvenanceInstaller,
		"user":      ProvenanceUser,
		// Everything else is unknown, NEVER a guess. "unknown" is the honest
		// answer for a release predating the key, and callers treat it as a human
		// choice — guessing "installer" would risk overruling an operator.
		"unknown":   ProvenanceUnknown,
		"":          ProvenanceUnknown,
		"banana":    ProvenanceUnknown,
		"Installer": ProvenanceUnknown, // case-sensitive by design: the chart enum is lowercase
		"user ":     ProvenanceUnknown, // no trimming — a stray space is not a verdict
		"future":    ProvenanceUnknown, // a value this binary predates
	}
	for raw, want := range cases {
		if got := NormalizeProvenance(raw); got != want {
			t.Errorf("NormalizeProvenance(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseTrainingReadsProvenance(t *testing.T) {
	base := map[string]string{"RESOURCE_LIMITS": "cpu=4,memory=16Gi"}

	t.Run("explicit user", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env["RESOURCE_PROVENANCE"] = "user"
		if got := ParseTraining(env).Provenance; got != ProvenanceUser {
			t.Errorf("Provenance = %q, want %q", got, ProvenanceUser)
		}
	})

	t.Run("explicit installer", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env["RESOURCE_PROVENANCE"] = "installer"
		if got := ParseTraining(env).Provenance; got != ProvenanceInstaller {
			t.Errorf("Provenance = %q, want %q", got, ProvenanceInstaller)
		}
	})

	t.Run("a pre-marker release reports unknown, not empty", func(t *testing.T) {
		// The shape every edge in the field has today. An empty string here would
		// make callers branch on "" and invent their own default — a second
		// policy, which is the thing this ticket removes.
		if got := ParseTraining(base).Provenance; got != ProvenanceUnknown {
			t.Errorf("Provenance = %q, want %q", got, ProvenanceUnknown)
		}
	})

	t.Run("provenance does not disturb the numbers", func(t *testing.T) {
		env := map[string]string{
			"RESOURCE_LIMITS":     "cpu=4,memory=16Gi",
			"RESOURCE_PROVENANCE": "user",
		}
		train := ParseTraining(env)
		if !train.HasCPUMem {
			t.Fatal("HasCPUMem false")
		}
		if train.CPU.Value() != 4 || train.Mem.Value() != 16*gib {
			t.Errorf("ceiling moved: got %s / %s", train.CPU.String(), train.Mem.String())
		}
	})
}

func TestRoundTripSetThenRead(t *testing.T) {
	// The invariant that matters end to end: what `resources set` writes, the
	// read path reports as a human choice. If this ever breaks, the CLI would be
	// telling a future ladder it may overwrite a size the operator chose.
	cpu := *resource.NewQuantity(6, resource.DecimalSI)
	mem := *resource.NewQuantity(24*gib, resource.BinarySI)

	written := BuildEnvSpec(cpu, mem, "", resource.Quantity{}, false)
	readBack := ParseTraining(written)

	if readBack.Provenance != ProvenanceUser {
		t.Errorf("round trip lost the marker: got %q, want %q",
			readBack.Provenance, ProvenanceUser)
	}
	if readBack.CPU.Cmp(cpu) != 0 || readBack.Mem.Cmp(mem) != 0 {
		t.Errorf("round trip changed the ceiling: %s / %s",
			readBack.CPU.String(), readBack.Mem.String())
	}
}
