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

// LayoutV1Bytes is the raw JSON of the per-task dataset-layout contract
// (layout.v1.json), vendored from tracebloc/data-ingestors at build time via
// scripts/sync-schema.sh (data-ingestors#347/#353).
//
// The ingestor is the source of truth for what a task's local dataset looks
// like on disk — the manifest CSV, whether it carries a label column, the
// primary file subdir, extra sidecar dirs, and the in-.txt record format for
// the structured text tasks. Embedding it here lets the CLI's discovery +
// staging be a VERIFIED MIRROR of that contract (RFC-0002 Principle 6) rather
// than re-implementing the layout rules in Go — drift is caught at build time
// by the same sync-schema.sh --check the ingest schema uses.
//
//go:embed layout.v1.json
var LayoutV1Bytes []byte
