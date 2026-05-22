package submit

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildRequest_GeneratesIdempotencyKey: when no override is
// provided, BuildRequest produces a fresh hex-encoded 16-byte key.
// Pins both "non-empty" + "looks-like-hex" so a future change to
// random source can't silently produce a malformed key.
func TestBuildRequest_GeneratesIdempotencyKey(t *testing.T) {
	req, err := BuildRequest("yaml content", "", "")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.IdempotencyKey == "" {
		t.Fatal("generated idempotency key is empty")
	}
	if len(req.IdempotencyKey) != 32 {
		t.Errorf("generated key length = %d, want 32 (16 bytes hex)", len(req.IdempotencyKey))
	}
	for i, c := range req.IdempotencyKey {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("key[%d] = %c, want lowercase hex", i, c)
			break
		}
	}
}

// TestBuildRequest_IdempotencyKeyOverride: --idempotency-key flag
// path. The override is plumbed verbatim — no hashing, no munging —
// so a customer using the same key across retries gets the chart's
// replay semantics.
func TestBuildRequest_IdempotencyKeyOverride(t *testing.T) {
	req, err := BuildRequest("yaml", "my-fixed-key-abc123", "")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.IdempotencyKey != "my-fixed-key-abc123" {
		t.Errorf("IdempotencyKey = %q, want override value", req.IdempotencyKey)
	}
}

// TestBuildRequest_KeysAreUniquePerCall: two back-to-back
// BuildRequest calls with no override produce distinct keys.
// Critical for jobs-manager's idempotency table — same key means
// "this is a retry, replay the previous run."
func TestBuildRequest_KeysAreUniquePerCall(t *testing.T) {
	a, err := BuildRequest("yaml", "", "")
	if err != nil {
		t.Fatalf("BuildRequest a: %v", err)
	}
	b, err := BuildRequest("yaml", "", "")
	if err != nil {
		t.Fatalf("BuildRequest b: %v", err)
	}
	if a.IdempotencyKey == b.IdempotencyKey {
		t.Errorf("back-to-back BuildRequest produced identical keys %q", a.IdempotencyKey)
	}
}

// TestBuildRequest_JSONShape: the wire format jobs-manager expects.
// Field names + omitempty behavior are the contract; if any of
// these drift, the server-side handler fails to parse.
func TestBuildRequest_JSONShape(t *testing.T) {
	t.Run("with image digest", func(t *testing.T) {
		req, _ := BuildRequest("yaml-content", "key123", "sha256:abc")
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		s := string(b)
		for _, want := range []string{
			`"ingest_config":"yaml-content"`,
			`"idempotency_key":"key123"`,
			`"image_digest":"sha256:abc"`,
		} {
			if !strings.Contains(s, want) {
				t.Errorf("JSON missing %q in: %s", want, s)
			}
		}
	})
	t.Run("omits empty image digest", func(t *testing.T) {
		req, _ := BuildRequest("yaml", "key", "")
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		// jobs-manager's no-image-digest code path is the
		// well-tested default; passing an empty string would
		// route through the override path on the server. The
		// omitempty tag is the contract that keeps the default
		// path engaged.
		if strings.Contains(string(b), "image_digest") {
			t.Errorf("JSON includes image_digest when empty: %s", b)
		}
	})
}
