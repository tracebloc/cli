// Package push owns the `tracebloc dataset push` flow: synthesizing
// an ingest spec from CLI flags, walking the customer's local data
// directory, and (in a follow-up PR) staging files into the cluster's
// shared PVC via an ephemeral Pod + tar-over-exec stream.
//
// Phase 3 lands in two PRs (per tracebloc/client#151):
//
//   - PR-a (this one): the no-op-safe path — spec synthesis, local
//     layout discovery, PVC discovery, --dry-run. Everything up to
//     "ready to copy files" without actually copying.
//   - PR-b (next): the novel-engineering core — ephemeral stage Pod,
//     client-go remotecommand executor, tar stream, progress bar,
//     SIGINT-safe cleanup.
//
// The split keeps each diff reviewable. PR-a's purpose is "fail fast
// before we touch the cluster"; PR-b is "now actually push the bytes".
package push

import "path"

// SpecArgs is the user-facing flag set for `tracebloc dataset push`.
//
// It's intentionally narrower than the full ingest.v1.json schema —
// for v0.1, only the image_classification minimum is exposed. The
// epic (#147) defers all other categories to v0.2 as one-PR
// additions; the flag set will grow when those land.
//
// Validation is NOT enforced here. Build() produces an
// ingest.v1.json-conforming map, and the caller pipes it through
// internal/schema's V1 validator. Duplicating "intent must be
// train|test" or similar in Go-side code would drift from the
// embedded schema — the schema is the single source of truth.
type SpecArgs struct {
	// Table is the destination MySQL table name in the cluster. Also
	// used as the per-table subdirectory under /data/shared/ so
	// pushes of multiple tables don't collide on the PVC.
	Table string

	// Category pins the task family. For v0.1, only
	// "image_classification" is supported end-to-end — the epic
	// non-goals defer other categories to v0.2. The flag accepts
	// other values so the schema's enum check produces the canonical
	// error message (rather than a CLI-side "unknown category"
	// that drifts from the schema).
	Category string

	// Intent is "train" or "test" per the schema's enum. Same
	// rationale as Category for not pre-validating here.
	Intent string

	// LabelColumn is the column name in labels.csv that holds the
	// label. The schema accepts either a shorthand string (this
	// field) or a {column, policy} object; v0.1 only emits the
	// shorthand because passthrough is the only policy
	// image_classification cares about.
	LabelColumn string
}

// Build produces the ingest.v1.json-conforming spec map. The
// returned map is YAML-marshalable and ready to feed to
// internal/schema's V1 validator.
//
// PVC paths (csv, images) are constructed under
// /data/shared/<table>/ to match the chart's mount convention
// surfaced by Phase 2's cluster discovery — jobs-manager mounts
// client-pvc at /data/shared, and the per-table subdir prevents
// cross-table collisions when a customer pushes multiple datasets
// to the same release.
//
// File-name conventions inside that subdir (labels.csv,
// images/) are dictated by the local layout that internal/push.Discover
// requires. Keeping the layout convention in lock-step on both
// sides — the CLI's view of "what local files we expect" and the
// spec's view of "where they'll live in the cluster" — means a
// successful Discover guarantees a runnable spec.
func (a SpecArgs) Build() map[string]any {
	prefix := StagedPrefix(a.Table)
	return map[string]any{
		"apiVersion": "tracebloc.io/v1",
		"kind":       "IngestConfig",
		"category":   a.Category,
		"table":      a.Table,
		"intent":     a.Intent,
		// Trailing slash on `images` matches the schema example
		// (data-ingestors/examples/yaml/image_classification.yaml,
		// line 14). The ingestor treats it as a directory glob.
		"csv":    path.Join(prefix, "labels.csv"),
		"images": path.Join(prefix, "images") + "/",
		"label":  a.LabelColumn,
	}
}

// StagedPrefix returns the in-cluster destination directory the CLI
// writes files into for a given table. Used in two places that
// MUST agree:
//
//  1. Phase 3 (this PR + PR-b): the path the ephemeral stage Pod
//     creates and tars files into.
//  2. The csv/images fields in Build() above, which jobs-manager
//     reads to know where the ingestor Job will find them.
//
// Exported because Phase 3's PR-b (stage Pod construction) needs
// it from the same place, and Phase 4 (submit) might want to print
// it as part of "what we pushed."
func StagedPrefix(table string) string {
	// path.Join collapses redundant slashes but doesn't preserve
	// trailing slashes — fine here because callers either append a
	// filename (labels.csv) or add the trailing slash explicitly
	// (images/).
	return path.Join("/data/shared", table)
}
