package push

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/schema"
)

// TestEmbeddedContractVersionIsSupported: the vendored bytes must be a version
// this CLI reads. Cheap, and it is the assertion that turns a silent stale
// re-vendor into a build failure (backend#3146).
func TestEmbeddedContractVersionIsSupported(t *testing.T) {
	var c LayoutContract
	if err := json.Unmarshal(schema.LayoutV1Bytes, &c); err != nil {
		t.Fatalf("parsing embedded layout.v1.json: %v", err)
	}
	if !SupportedLayoutVersions[c.Version] {
		t.Fatalf("embedded contract is version %q, not in %v — re-vendor and "+
			"widen the set deliberately", c.Version, sortedVersions())
	}
}

// TestUnsupportedVersionIsRefused: the guard must FIRE, not merely exist.
//
// Driven through the same predicate `mustLoadLayoutContract` consults rather
// than through the loader itself, because the loader reads bytes embedded at
// build time and cannot be handed a different document from a test. That is a
// real limit of the design and worth stating: what is proven here is the
// DECISION, and `TestEmbeddedContractVersionIsSupported` above proves the
// decision is applied to the real bytes.
func TestUnsupportedVersionIsRefused(t *testing.T) {
	for _, v := range []string{"3", "5", "", "4.0", "v4"} {
		if SupportedLayoutVersions[v] {
			t.Errorf("version %q is accepted; only the versions this code has "+
				"been read against should be", v)
		}
	}
	// ...and the one that must pass, or the test above is vacuous.
	if !SupportedLayoutVersions["4"] {
		t.Error(`version "4" is not accepted, so every rejection above proves nothing`)
	}
}

// TestTheGuardIsWiredIntoTheLoader: the predicate could be correct and unused.
//
// This is the failure mode that shipped on data-ingestors#557 — a tripwire
// defined and never called, with a mutation table crediting it for reds that
// came from a neighbouring check. Asserting the loader's own source would be a
// prose grep; instead this pins that the panic message a stale re-vendor would
// produce actually names the remedy, which only holds if the branch exists.
func TestTheGuardIsWiredIntoTheLoader(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("loading an unsupported version did not panic")
		} else if msg, _ := r.(string); !strings.Contains(msg, "SupportedLayoutVersions") {
			t.Errorf("panic does not name the remedy: %v", r)
		}
	}()
	// Temporarily narrow the supported set so the embedded (valid) bytes
	// become unsupported, then re-run the loader. Restored by the defer order.
	orig := SupportedLayoutVersions
	defer func() { SupportedLayoutVersions = orig }()
	SupportedLayoutVersions = map[string]bool{"__none__": true}
	_ = mustLoadLayoutContract()
}
