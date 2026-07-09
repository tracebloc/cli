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
// The name must START with a letter or underscore, then letters,
// digits, and underscores: [A-Za-z_][A-Za-z0-9_]*. No slashes, no
// dots — which is what closes the path-traversal hole (see
// ValidateTableName) — and no leading digit.
//
// This mirrors the ingestor's own check (the source of truth):
// tracebloc/data-ingestors' validators/table_name_validator.py
// requires ^[a-zA-Z_][a-zA-Z0-9_]*$. Any name accepted here is
// therefore accepted in-cluster; the looser old pattern let
// leading-digit / all-digit names ("123", "1data") through the CLI
// only for the cluster to reject them post-upload.
//
// All the real-world example tables (chest_xrays_train,
// cats_dogs_train) match this; it's the conventional snake_case
// table-naming style anyway.
var tableNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
		return fmt.Errorf("dataset name is required (set --name)")
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
			"table name %q is invalid: must start with a letter or "+
				"underscore, then letters, digits, and underscores only "+
				"(matches [A-Za-z_][A-Za-z0-9_]*). The table name is "+
				"used both as the MySQL table identifier and as the "+
				"/data/shared/<table>/ subdirectory on the cluster PVC, "+
				"so a leading digit, slashes, dots, and path-traversal "+
				"sequences are rejected.",
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

	// TargetSize, when len==2, holds the resolution as [W, H] — the
	// order produced by --target-size "WxH" and by DetectImageSize.
	// The ingestor's image_classification default is 512x512 and it
	// VALIDATES (it does not resize), so a dataset whose images don't
	// match the default hard-fails. Setting this emits
	// spec.file_options.target_size so the customer's actual
	// resolution wins. Empty (len 0) ⇒ omit and let the ingestor
	// default apply. Populated by the CLI from --target-size or by
	// auto-detecting the first image. (Image categories only.)
	//
	// NOTE: stored [W, H] here AND emitted as [width, height] by
	// buildImage — no swap. That's the order the ingest.v1 schema
	// documents ("matches PIL.Image.size and what
	// ImageResolutionValidator expects") and the order the validator
	// compares against verbatim (PIL's img.size is (W, H)). An earlier
	// [H,W] revision (PR #22 review note) was mistaken and made every
	// non-square dataset fail in-cluster; #147 reverted it. Do not
	// re-introduce a swap. (See the inline comment in buildImage.)
	TargetSize []int

	// Schema is the column→SQL-type map for tabular / time-series
	// categories (required by the schema for those). Populated by the
	// CLI from --schema or by inferring types from the CSV. Ignored
	// for image categories.
	Schema map[string]string

	// LabelPolicy is "passthrough" or "bucket". Regression-class
	// categories (tabular_regression, time_series_forecasting,
	// time_to_event_prediction) require the object label form with a
	// policy; the CLI defaults it to "bucket" so the raw numeric
	// target never ships to the central backend. Ignored for
	// classification categories (which emit the string label form).
	LabelPolicy string

	// TimeColumn names the time column for time_to_event_prediction.
	// Emitted as the top-level `time_column` field. Empty ⇒ the
	// ingestor falls back to a column named "time".
	TimeColumn string

	// NumberOfKeypoints is the keypoints-per-sample count for
	// keypoint_detection (required there; no convention default).
	// Emitted under spec.file_options.number_of_keypoints — the schema
	// allows arbitrary file_options keys, and conventions.resolve reads
	// it from there, so this needs no top-level schema field. 0 ⇒
	// unset (ignored for non-keypoint categories).
	NumberOfKeypoints int

	// Extension is the single image file extension every file in the
	// dataset shares (detected by DetectExtension), emitted as
	// spec.file_options.extension so the ingestor's FileTypeValidator
	// checks against what was actually staged instead of its .jpeg
	// convention default (cli#68).
	Extension string
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
		"csv":        path.Join(prefix, "labels.csv"),
	}
	switch {
	case IsTabular(a.Category):
		a.buildTabular(spec)
	case IsText(a.Category):
		a.buildText(spec, prefix)
	default:
		// Image categories (and any not-yet-special-cased category —
		// the schema validator produces the canonical error for those).
		a.buildImage(spec, prefix)
	}
	return spec
}

// buildText fills in the text-family fields: the text-file sidecar
// directory (texts/ for text_classification, sequences/ for
// masked_language_modeling) and the label. masked_language_modeling
// has NO label (the schema doesn't require one for it).
func (a SpecArgs) buildText(spec map[string]any, prefix string) {
	dir := TextSidecarDir(a.Category)
	// Trailing slash matches the directory-glob convention the
	// ingestor uses for sidecar dirs.
	spec[dir] = path.Join(prefix, dir) + "/"
	if a.Category == "text_classification" {
		spec["label"] = a.LabelColumn
	}
}

// buildImage fills in the image-family fields for image_classification,
// object_detection, and keypoint_detection: the images/ dir, the label,
// object_detection's annotations/ dir, and the resolution overrides.
//
// keypoint_detection emits target_size + number_of_keypoints as
// TOP-LEVEL fields — the schema's keypoint conditional requires them
// there (both are dataset-specific, no convention defaults), and the
// ingestor validates against that conditional. image_classification
// and object_detection emit target_size under spec.file_options (the
// override key conventions.resolve reads); without it,
// image_classification defaults to 512x512 and the Image Resolution
// Validator rejects other sizes.
func (a SpecArgs) buildImage(spec map[string]any, prefix string) {
	// Trailing slash on the dir fields matches the schema example
	// (data-ingestors/examples/yaml/image_classification.yaml); the
	// ingestor treats them as directory globs.
	spec["images"] = path.Join(prefix, "images") + "/"
	spec["label"] = a.LabelColumn
	if a.Category == "object_detection" {
		spec["annotations"] = path.Join(prefix, "annotations") + "/"
	}

	// file_options carries the per-file conventions the ingestor's
	// validators read: the detected extension (all categories in the
	// image family — FileTypeValidator checks images against it) and,
	// for the non-keypoint categories, the resolution override.
	fileOptions := map[string]any{}
	if a.Extension != "" {
		fileOptions["extension"] = a.Extension
	}

	if a.Category == "keypoint_detection" {
		if len(a.TargetSize) == 2 {
			// Emitted as [width, height] — the schema's own description
			// says so ("matches PIL.Image.size and what
			// ImageResolutionValidator expects"), and the validator
			// compares PIL's (W,H) against this list verbatim (verified
			// empirically: an 8×4 image passes [8,4], fails [4,8]). An
			// earlier revision swapped to [H,W] here on a mistaken review
			// note, which made EVERY non-square dataset fail in-cluster
			// after the full upload.
			spec["target_size"] = []int{a.TargetSize[0], a.TargetSize[1]}
		}
		if a.NumberOfKeypoints > 0 {
			spec["number_of_keypoints"] = a.NumberOfKeypoints
		}
	} else if len(a.TargetSize) == 2 {
		// [width, height] — same contract as the keypoint branch above.
		fileOptions["target_size"] = []int{a.TargetSize[0], a.TargetSize[1]}
	}

	if len(fileOptions) > 0 {
		spec["spec"] = map[string]any{"file_options": fileOptions}
	}
}

// buildTabular fills in the tabular / time-series fields: the column
// schema, the label (string form for classification, object form with
// a policy for regression-class), and time_column for
// time_to_event_prediction.
func (a SpecArgs) buildTabular(spec map[string]any) {
	spec["schema"] = a.Schema
	if IsRegressionClass(a.Category) {
		// Regression-class tasks require the object label form with an
		// explicit policy (the schema enforces this). Default to
		// `bucket` so the raw numeric target never ships to the central
		// backend unless the customer opts into passthrough.
		policy := a.LabelPolicy
		if policy == "" {
			policy = "bucket"
		}
		spec["label"] = map[string]any{"column": a.LabelColumn, "policy": policy}
	} else {
		// tabular_classification: plain column-name label.
		spec["label"] = a.LabelColumn
	}
	if a.Category == "time_to_event_prediction" && a.TimeColumn != "" {
		spec["time_column"] = a.TimeColumn
	}
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
