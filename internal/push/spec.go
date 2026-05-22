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

import (
	"fmt"
	"path"
	"regexp"
)

// tableNamePattern is the safe character set for a table name. It
// must satisfy TWO independent constraints simultaneously:
//
//  1. A valid unquoted MySQL identifier — the chart's ingestor
//     CREATEs a table with this exact name.
//  2. A safe single path segment — the name becomes the
//     /data/shared/<table>/ subdirectory on the PVC.
//
// The intersection of "MySQL identifier" and "single safe path
// component" is [A-Za-z0-9_]: letters, digits, underscore. No
// slashes, no dots — which is what closes the path-traversal hole
// (see ValidateTableName).
//
// All the real-world example tables (chest_xrays_train,
// cats_dogs_train) match this; it's the conventional snake_case
// table-naming style anyway.
var tableNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// MaxTableNameLength caps `--table` at 63 chars. Two hard limits
// agree on this:
//
//  1. MySQL identifier limit: 64 chars (MySQL Reference 9.2 Schema
//     Object Names). We use 63 to leave one char of headroom for
//     any future "ingestion_run_id" suffix the chart might want to
//     append.
//  2. Kubernetes label value limit: 63 chars (DNS-1123 label rules).
//     The stage Pod's tracebloc.io/table label carries the
//     untransformed table name verbatim — a longer value fails Pod
//     creation post-pre-flight, which Bugbot flagged on PR-b
//     round 5.
//
// 63 is a coincidence that lets both checks share the same constant.
const MaxTableNameLength = 63

// ValidateTableName rejects table names that aren't safe as both a
// MySQL identifier and a single PVC path segment.
//
// Why a CLI-side check rather than the schema: the embedded
// ingest.v1.json only enforces `minLength: 1` on `table` — no
// `pattern`, no `maxLength`. Without this guard, --table=../../etc
// would flow into the /data/shared/<table>/ PVC path, and a 100-char
// name would make the Pod-label assignment fail post-pre-flight.
// Tightening the upstream schema is the proper long-term fix
// (it would protect the helm flow + jobs-manager too) but needs
// a change to tracebloc/data-ingestors' schema — filed as
// tracebloc/data-ingestors#116. Once that lands and we re-sync,
// this guard can collapse to a thin "schema says so" wrapper.
//
// Callers MUST run this before SpecArgs.Build() or StagedPrefix(),
// both of which assume a validated name.
func ValidateTableName(table string) error {
	if table == "" {
		return fmt.Errorf("table name is required (set --table)")
	}
	if len(table) > MaxTableNameLength {
		return fmt.Errorf(
			"table name is %d characters; the max is %d "+
				"(matches both the MySQL identifier limit and the "+
				"Kubernetes label-value limit, which the stage Pod's "+
				"tracebloc.io/table label is bound by). Use a shorter name.",
			len(table), MaxTableNameLength)
	}
	if !tableNamePattern.MatchString(table) {
		return fmt.Errorf(
			"table name %q is invalid: must match [A-Za-z0-9_]+ "+
				"(letters, digits, underscore only). The table name is "+
				"used both as the MySQL table identifier and as the "+
				"/data/shared/<table>/ subdirectory on the cluster PVC, "+
				"so slashes, dots, and path-traversal sequences are "+
				"rejected.",
			table)
	}
	return nil
}

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
//
// PRECONDITION: a.Table must have passed ValidateTableName. Build
// calls StagedPrefix, which panics on an unsafe name.
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
//
// PRECONDITION: table must already have passed ValidateTableName.
// This function panics on an unsafe name rather than returning an
// escape path — a name that escapes /data/shared is a caller bug
// (validation was skipped), and a panic surfaces it loudly in
// tests instead of silently letting PR-b's stage Pod write to,
// say, /etc. Every production call path runs ValidateTableName
// first (see cli.runDatasetPush), so the panic is unreachable in
// correct code.
func StagedPrefix(table string) string {
	// Deliberately NOT path.Join here: path.Join cleans ".."
	// segments, which is exactly the silent traversal we're
	// guarding against. Plain concatenation keeps the name as a
	// literal segment so the assertion below can detect a bad one.
	prefix := "/data/shared/" + table
	if !tableNamePattern.MatchString(table) {
		panic(fmt.Sprintf(
			"push.StagedPrefix: unsafe table name %q — caller must "+
				"ValidateTableName before constructing a PVC path",
			table))
	}
	return prefix
}
