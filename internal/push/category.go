package push

// Category families. These mirror data-ingestors'
// tracebloc_ingestor/cli/conventions.py groupings so the CLI's
// per-category behaviour (which flags are required, which local
// layout to expect, which spec fields to emit) stays in lock-step
// with what the ingestor actually resolves.
//
// Kept as a single source of truth here rather than scattered
// string comparisons across spec.go / dataset.go.

// imageCategories take a labels CSV + an images/ directory (plus,
// for some, extra sidecar dirs handled in later increments).
var imageCategories = map[string]bool{
	"image_classification":  true,
	"object_detection":      true,
	"keypoint_detection":    true,
	"semantic_segmentation": true,
	"instance_segmentation": true,
}

// tabularCategories take a single CSV whose columns are described by
// a `schema` (column → SQL type) map. No sidecar files.
var tabularCategories = map[string]bool{
	"tabular_classification":   true,
	"tabular_regression":       true,
	"time_series_forecasting":  true,
	"time_to_event_prediction": true,
}

// regressionClassCategories predict a numeric target rather than a
// class. The schema requires the label in object form with an
// explicit `policy` so the raw target never ships to the central
// backend by default (policy=bucket bins it first).
var regressionClassCategories = map[string]bool{
	"tabular_regression":       true,
	"time_series_forecasting":  true,
	"time_to_event_prediction": true,
}

// IsImage reports whether category uses the labels.csv + images/
// local layout.
func IsImage(category string) bool { return imageCategories[category] }

// IsTabular reports whether category uses the single-CSV + schema
// local layout (no sidecar files).
func IsTabular(category string) bool { return tabularCategories[category] }

// IsRegressionClass reports whether category predicts a numeric
// target and therefore needs label.policy (object label form).
func IsRegressionClass(category string) bool { return regressionClassCategories[category] }
