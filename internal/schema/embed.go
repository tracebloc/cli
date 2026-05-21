// Package schema holds the embedded ingest schema(s) and the
// validator built on top of them.
//
// Today only v1 is supported. When a v2 lands, this package grows
// a SchemaVersion dispatch (per the ingest.yaml's apiVersion field)
// without changing the validator's public API.
//
// Why embed? Two reasons.
//
//  1. Local validation. `tracebloc ingest validate <path>` runs
//     entirely offline — no cluster, no network — which is the
//     whole point of doing it on the CLI rather than waiting for
//     jobs-manager's POST-time check.
//  2. Drift detection. Bundling the schema makes drift between
//     the CLI's view and data-ingestors' canonical source
//     observable at build time, not at runtime. scripts/sync-schema.sh
//     enforces parity in CI.
package schema

import _ "embed"

// V1Bytes is the raw JSON of ingest.v1.json, vendored from
// tracebloc/data-ingestors at build time via scripts/sync-schema.sh.
//
// Exposed as []byte (not parsed) so consumers can either pipe it
// into their preferred JSON-Schema implementation or inspect the
// raw $id / $schema fields to assert which version they got.
//
//go:embed ingest.v1.json
var V1Bytes []byte
