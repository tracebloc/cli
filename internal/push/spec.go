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

	// TargetSize, when len==2, pins the image resolution as [W, H].
	// The ingestor's image_classification default is 512x512 and it
	// VALIDATES (it does not resize), so a dataset whose images don't
	// match the default hard-fails. Setting this emits
	// spec.file_options.target_size so the customer's actual
	// resolution wins. Empty (len 0) ⇒ omit and let the ingestor
	// default apply. Populated by the CLI from --target-size or by
	// auto-detecting the first image.
	TargetSize []int
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
	spec := map[string]any{
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
	// Emit the image resolution under spec.file_options.target_size —
	// the same override key the helm flow + data-ingestors'
	// conventions.resolve honour (it merges spec.file_options over the
	// per-category default). Without this, image_classification
	// defaults to 512x512 and the ingestor's Image Resolution
	// Validator rejects any other size.
	if len(a.TargetSize) == 2 {
		spec["spec"] = map[string]any{
			"file_options": map[string]any{
				"target_size": []int{a.TargetSize[0], a.TargetSize[1]},
			},
		}
	}
	return spec
}

// SharedRoot is the in-cluster mount path of the chart's shared PVC
// (cluster.SharedPVCMountPath). Both the ephemeral stage Pod and the
// ingestor Job mount client-pvc here, so any path under it is visible
// to both — which is why the CLI's staging area lives under it.
const SharedRoot = "/data/shared"

// stagingDirName is the hidden directory under SharedRoot where the
// CLI lands a run's SOURCE files. It is deliberately SEPARATE from
// the ingestor's destination (SharedRoot/<table>):
//
// data-ingestors computes DEST_PATH = STORAGE_PATH/TABLE_NAME =
// SharedRoot/<table>, and its DuplicateValidator FAILS if that path
// already exists non-empty. If the CLI staged straight into
// SharedRoot/<table> (as it did originally), its own staging would
// create exactly the non-empty destination the validator rejects —
// so every push failed the duplicate check. Staging under
// SharedRoot/.tracebloc-staging/<table> keeps the destination fresh
// while remaining on the same PVC the ingestor reads.
const stagingDirName = ".tracebloc-staging"

// StagedPrefix returns the in-cluster directory the CLI streams a
// dataset's SOURCE files into for a given table. The synthesized
// spec's csv/images point here; the ingestor reads from here and
// writes the processed table to FinalDestPrefix(table).
//
// Two call sites MUST agree on this value:
//
//  1. The ephemeral stage Pod's tar target (StreamLayout).
//  2. The csv/images fields in Build() above, which jobs-manager
//     hands to the ingestor Job so it reads what we just staged.
//
// It is intentionally NOT SharedRoot/<table>: that is the ingestor's
// DEST_PATH, whose DuplicateValidator rejects a pre-existing,
// non-empty directory. See stagingDirName.
//
// PRECONDITION: table must already have passed ValidateTableName.
// This function panics on an unsafe name rather than returning an
// escape path — a name that escapes SharedRoot is a caller bug
// (validation was skipped), and a panic surfaces it loudly in tests
// instead of silently letting the stage Pod write to, say, /etc.
// Every production call path runs ValidateTableName first (see
// cli.runDatasetPush), so the panic is unreachable in correct code.
func StagedPrefix(table string) string {
	// Deliberately NOT path.Join here: path.Join cleans ".."
	// segments, which is exactly the silent traversal we're
	// guarding against. Plain concatenation keeps the name as a
	// literal segment so the assertion below can detect a bad one.
	if !tableNamePattern.MatchString(table) {
		panic(fmt.Sprintf(
			"push.StagedPrefix: unsafe table name %q — caller must "+
				"ValidateTableName before constructing a PVC path",
			table))
	}
	return SharedRoot + "/" + stagingDirName + "/" + table
}

// FinalDestPrefix returns where the ingestor writes the processed
// table: SharedRoot/<table>, matching data-ingestors' config.DEST_PATH
// (STORAGE_PATH/TABLE_NAME). This is what the training side reads and
// what the CLI shows the customer as the destination. The CLI never
// writes here directly — doing so would trip the ingestor's
// DuplicateValidator; it stages to StagedPrefix(table) and the
// ingestor produces this path.
//
// PRECONDITION: table must already have passed ValidateTableName.
// Panics on an unsafe name, same rationale as StagedPrefix.
func FinalDestPrefix(table string) string {
	if !tableNamePattern.MatchString(table) {
		panic(fmt.Sprintf(
			"push.FinalDestPrefix: unsafe table name %q — caller must "+
				"ValidateTableName before constructing a PVC path",
			table))
	}
	return SharedRoot + "/" + table
}
