package push

import (
	"encoding/json"
	"strconv"
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
	maxV := maxSupportedVersion(t)

	// The first version PAST the supported set (max+1), DERIVED rather than the
	// literal "5" this test used to carry (backend#3190). A hardcoded "5" is a
	// standing claim that v5 is unsupported forever, so the deliberate v5
	// re-vendor this whole guard exists to make safe would instead RED this
	// test — the set widens to include "5", and the loop then asserts an
	// accepted version is refused. Deriving it means the target moves to "6" on
	// that re-vendor and the guard keeps meaning what it should: "the first
	// version beyond what this code has been read against is refused."
	//
	// Alongside it, tokens the integer version field can never legitimately
	// take — an empty version, a dotted float, a "v"-prefixed tag — which stay
	// refused across any re-vendor and pin that the predicate does no fuzzy
	// matching.
	next := strconv.Itoa(maxV + 1)
	for _, v := range []string{next, "", "4.0", "v4"} {
		if SupportedLayoutVersions[v] {
			t.Errorf("version %q is accepted; only the versions this code has "+
				"been read against should be", v)
		}
	}
	// ...and the highest supported version IS accepted, or every rejection
	// above proves nothing. Read back from the set (not the old literal "4")
	// so a re-vendor that changes which versions ship can't leave this
	// assertion pinned to a version that is no longer supported. The canonical
	// string form is used deliberately: a non-canonical key like "04" would
	// fail here, which is a shape worth catching.
	if want := strconv.Itoa(maxV); !SupportedLayoutVersions[want] {
		t.Errorf("highest supported version %q reads as not accepted, so every "+
			"rejection above proves nothing", want)
	}
}

// maxSupportedVersion returns the highest integer version in
// SupportedLayoutVersions — the anchor TestUnsupportedVersionIsRefused derives
// its "first unsupported version" from. Non-integer keys are skipped: the
// contract version is a bare integer the ingestor bumps by one, so max+1 is
// only meaningful against the integer keys. Fails the test if the set carries
// no integer version at all, which would leave the guard nothing to anchor to
// (and, since reaching this point proves the set is non-empty, is also what
// keeps the rejections above non-vacuous).
func maxSupportedVersion(t *testing.T) int {
	t.Helper()
	maxV, found := 0, false
	for v := range SupportedLayoutVersions {
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		if !found || n > maxV {
			maxV, found = n, true
		}
	}
	if !found {
		t.Fatalf("SupportedLayoutVersions %v has no integer version to derive "+
			"an unsupported one from", sortedVersions())
	}
	return maxV
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
