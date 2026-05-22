// Package submit owns the `tracebloc dataset push` Phase 4 step:
// POST the synthesized ingest spec to jobs-manager's
// /internal/submit-ingestion-run endpoint, then watch the ingestor
// Job the cluster spawns in response.
//
// Phase 4 sits between Phase 3's stage Pod (which lays files on the
// PVC) and Phase 5's release distribution. The protocol is the same
// one tracebloc/client's ingestor subchart's post-install hook uses
// today — see ingestor/templates/configmap-ingest-config.yaml.
// Keeping the protocol identical means the CLI and the helm flow
// are interchangeable at the cluster's API surface, and the chart
// stays a fully-supported alternative for ops folks who prefer it.
//
// The package is split into:
//   - body.go (this file): synthesize the POST body
//   - client.go:            HTTP client + bearer token + 4xx framing
//   - watch.go:             poll Job + stream Pod logs
//   - summary.go:           parse the 📊 INGESTION SUMMARY banner
//   - submit.go:            top-level orchestrator
package submit

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// SubmitRequest is the wire shape POSTed to jobs-manager's
// /internal/submit-ingestion-run. Field names mirror the chart's
// ingestor/templates/configmap-ingest-config.yaml body.json key
// for-key so the chart and the CLI are interchangeable on the
// server side.
//
// json struct tags are explicit-snake so a future re-import via
// encoding/json doesn't silently produce mixedCase keys.
type SubmitRequest struct {
	// IngestConfig is the customer's ingest spec as a verbatim
	// YAML string. jobs-manager re-parses + revalidates this
	// server-side. We don't re-marshal — the CLI's Phase 3 spec
	// synthesis already produced canonical YAML.
	IngestConfig string `json:"ingest_config"`

	// IdempotencyKey is the per-invocation replay token.
	// jobs-manager records this in its idempotency-key table; a
	// second POST with the same key returns the SAME job_name as
	// the first (with replay=true) instead of spawning a new Job.
	//
	// Default is a fresh UUID-ish 16-byte hex string per
	// invocation; `--idempotency-key <s>` overrides for the
	// at-most-once-across-attempts case where the customer
	// genuinely wants retry-safety across multiple CLI runs.
	IdempotencyKey string `json:"idempotency_key"`

	// ImageDigest optionally pins the ingestor container image.
	// Empty = let jobs-manager use the cluster's configured
	// default (set by the parent client chart's
	// `images.ingestor.digest`, kept current by the auto-upgrade
	// cronjob). Setting it locks the run to a specific image,
	// matching the chart's --set image.digest=... override path.
	//
	// `omitempty` on the JSON tag means jobs-manager sees no
	// image_digest key at all when this is empty, which is the
	// well-tested default-image code path on the server side.
	ImageDigest string `json:"image_digest,omitempty"`
}

// BuildRequest is the constructor used by the orchestrator. Both
// IngestConfig (the YAML the CLI already synthesized in Phase 3)
// and the optional ImageDigest flow through unchanged; the
// idempotency key is the only non-trivial bit.
//
// If override is empty, a fresh 16-byte hex string is generated
// from crypto/rand. UUID-shaped without the dashes — the chart's
// own helper does the same (ingestor.idempotencyKey in
// _helpers.tpl), so server-side hash-table lookups are uniform
// across both flows.
func BuildRequest(ingestYAML string, idempotencyKeyOverride, imageDigest string) (*SubmitRequest, error) {
	key := idempotencyKeyOverride
	if key == "" {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generating idempotency key: %w", err)
		}
		key = hex.EncodeToString(raw)
	}
	return &SubmitRequest{
		IngestConfig:   ingestYAML,
		IdempotencyKey: key,
		ImageDigest:    imageDigest,
	}, nil
}

// SubmitResponse is jobs-manager's 201 reply. job_name is the
// ingestor Job watch.go will poll; namespace is the resolved API
// namespace (usually the same one the CLI POSTed to, but
// jobs-manager can in principle redirect cross-namespace).
//
// Replay distinguishes "we just spawned this Job" (replay=false)
// from "we already have a Job for this idempotency key, here it
// is" (replay=true). The CLI prints a different lifecycle banner
// for each — replay means "another invocation already kicked
// this off; we're attaching to it."
type SubmitResponse struct {
	JobName   string `json:"job_name"`
	Namespace string `json:"namespace"`
	Replay    bool   `json:"replay"`
}
