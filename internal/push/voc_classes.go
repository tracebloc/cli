package push

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// vocAnnotation is the sliver of the Pascal-VOC schema the CLI reads: the
// object class names. Everything else in the document (bndbox, size, pose,
// truncated, ...) is the ingestor's and the trainer's business, not the local
// preview's.
type vocAnnotation struct {
	Objects []struct {
		Name string `xml:"name"`
	} `xml:"object"`
}

// VOCClasses returns the sorted distinct <object><name> values across the given
// Pascal-VOC annotation files.
//
// Fails CLOSED on a read or parse error, matching CheckLabelDiversity's own
// rule: a partial scan under-counts classes, which would false-reject good data
// or pass a single-class dataset. A file that parses but declares no objects is
// not an error here — PascalVOCXMLValidator owns malformed-annotation
// diagnostics in-cluster, and the ingestor skips object-less images.
func VOCClasses(annotationPaths []string) ([]string, error) {
	seen := map[string]struct{}{}
	for _, path := range annotationPaths {
		raw, err := os.ReadFile(path) // #nosec G304 -- paths come from the symlink-rejecting dataset walk of the operator-chosen root (per-file and total size already capped there); mirrors the in-cluster validator on the operator's own files.
		if err != nil {
			return nil, fmt.Errorf("reading annotation %s: %w", filepath.Base(path), err)
		}
		var doc vocAnnotation
		if err := xml.Unmarshal(raw, &doc); err != nil {
			// Type only, never the parser's message: it can quote the offending
			// document, and this reaches the customer's terminal and logs.
			return nil, fmt.Errorf(
				"annotation %s is not parseable XML (%T) — fix or remove it and re-run",
				filepath.Base(path), err)
		}
		for _, obj := range doc.Objects {
			if name := strings.TrimSpace(obj.Name); name != "" {
				seen[name] = struct{}{}
			}
		}
	}
	classes := make([]string, 0, len(seen))
	for name := range seen {
		classes = append(classes, name)
	}
	sort.Strings(classes)
	return classes, nil
}

// CheckVOCLabelDiversity previews the in-cluster >= 2 distinct class gate for a
// manifest-free object-detection dataset (backend#1006).
//
// Without this the local preview would simply STOP checking diversity for
// object detection: a single-class dataset would pass preflight, upload in
// full, and only then be rejected in-cluster — trading a fast local failure for
// a slow remote one, and losing coverage the CLI had before #1006. The premise
// of #1006 is that the XML already carries everything the manifest did, so the
// preview reads exactly what the in-cluster gate reads.
//
// Counts distinct CLASSES, not distinct annotation files: a dataset whose every
// image contains both a car and a sign is two classes in one file, and counting
// files would false-reject it.
func CheckVOCLabelDiversity(annotationPaths []string) error {
	classes, err := VOCClasses(annotationPaths)
	if err != nil {
		return err
	}
	if len(classes) >= 2 {
		return nil
	}
	return fmt.Errorf(
		"object detection needs at least 2 distinct classes, but the "+
			"annotations declare %d (%s). The cluster rejects a single-class "+
			"dataset (LabelDiversityValidator) — add the other classes' "+
			"annotations, or ingest a dataset that has them, then re-run",
		len(classes), TruncateList(classes, 5))
}

// previewImageLabelDiversity runs the image family's >= 2 distinct class
// preview against whichever source actually carries the classes.
//
// LabelDiversityValidator runs in-cluster for the WHOLE image family
// (is_classification covers object_detection and keypoint too), so the preview
// must too — it just has to read the right place. A manifest-free category has
// them in <object><name>; everything else has them in the manifest's label
// column, where the check benign-skips if no column resolves. Image labels are
// read untyped, so no NA drop and no numeric collapse.
func previewImageLabelDiversity(spec SpecArgs, layout *LocalLayout) error {
	if NoManifestCategory(spec.Category) {
		return CheckVOCLabelDiversity(layout.Sidecars["annotations"])
	}
	return CheckLabelDiversity(layout.LabelsCSV, spec.LabelColumn, false, false)
}
