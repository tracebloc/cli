package push

import (
	"fmt"
	"path/filepath"
)

// xmlExtensions are the annotation file types object_detection reads
// (Pascal VOC XML).
var xmlExtensions = map[string]struct{}{".xml": {}}

// DiscoverObjectDetection validates a local object_detection dataset:
//
//   - <root>/labels.csv        (required)
//   - <root>/images/*          (required)
//   - <root>/annotations/*.xml (required; Pascal VOC)
//
// It builds on the image-classification layout (labels.csv + images/)
// and adds the annotations/ sidecar via the shared sidecar walker, so
// the existing tar/stream machinery stages annotations under
// "annotations/".
func DiscoverObjectDetection(rootDir string) (*LocalLayout, error) {
	layout, err := Discover(rootDir) // labels.csv + images/ (+ caps + symlink guards)
	if err != nil {
		return nil, err
	}

	annotations, annoBytes, err := discoverSidecarFiles(layout.Root, "annotations", xmlExtensions)
	if err != nil {
		return nil, err
	}
	if len(annotations) == 0 {
		return nil, fmt.Errorf(
			"no .xml annotation files found in %q. object_detection expects "+
				"<dir>/annotations/*.xml (Pascal VOC).",
			filepath.Join(layout.Root, "annotations"))
	}
	if layout.Sidecars == nil {
		layout.Sidecars = map[string][]string{}
	}
	layout.Sidecars["annotations"] = annotations
	layout.TotalBytes += annoBytes

	if layout.TotalBytes > MaxTotalBytes {
		return nil, fmt.Errorf(
			"dataset is %s, exceeds v0.1 cap of %s. For larger datasets, the "+
				"cloud-source path is on the v0.2 roadmap (tracebloc/client#147).",
			HumanBytes(layout.TotalBytes), HumanBytes(MaxTotalBytes))
	}
	return layout, nil
}
