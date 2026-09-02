package push

import (
	"fmt"
	"path/filepath"
)

// xmlExtensions are the annotation file types object_detection reads
// (Pascal VOC XML).
var xmlExtensions = map[string]struct{}{".xml": {}}

// pngExtensions are the mask file types semantic_segmentation reads. The
// ingestor's FileTypeValidator forces masks to .png (modalities/validators.py
// semantic_segmentation) and the layout contract's masks sidecar globs *.png,
// so the CLI mirrors that here.
var pngExtensions = map[string]struct{}{".png": {}}

// DiscoverObjectDetection validates a local object_detection dataset:
//
//   - <root>/images/*          (required)
//   - <root>/annotations/*.xml (required; Pascal VOC)
//
// NO labels.csv. Since backend#1006 the ingestor enumerates object_detection
// records from the Pascal-VOC XML — <annotation><filename> names the image and
// <object><name> gives the classes — so a manifest carries nothing the
// annotations do not, and ingest.v1.json now REJECTS `csv:` for this category
// rather than ignoring it. Requiring one here would make the CLI demand a file
// the ingestor refuses.
//
// It shares the image walk (images/ + caps + symlink guards) and adds the
// annotations/ sidecar via the shared sidecar walker, so the existing
// tar/stream machinery stages annotations under "annotations/".
func DiscoverObjectDetection(rootDir string) (*LocalLayout, error) {
	layout, err := discover(rootDir, false) // images/ only (+ caps + symlink guards)
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

// DiscoverSemanticSegmentation validates a local semantic_segmentation dataset:
//
//   - <root>/labels.csv   (required; must declare + populate a mask_id column)
//   - <root>/images/*     (required)
//   - <root>/masks/*.png  (required; one PNG mask per image)
//
// Like object_detection it builds on the image-classification layout
// (labels.csv + images/) and adds a sidecar — here masks/ — via the shared
// sidecar walker, so the existing tar/stream machinery stages masks under
// "masks/". The images↔masks pairing (by the `_mask` filename suffix) and the
// mask_id link-column contract are previewed in preflight (CheckMaskPairing,
// CheckMaskIDColumn), mirroring the ingestor's FilePairingValidator +
// MaskIdColumnValidator (modalities/validators.py, backend#816).
func DiscoverSemanticSegmentation(rootDir string) (*LocalLayout, error) {
	layout, err := Discover(rootDir) // labels.csv + images/ (+ caps + symlink guards)
	if err != nil {
		return nil, err
	}

	masks, maskBytes, err := discoverSidecarFiles(layout.Root, "masks", pngExtensions)
	if err != nil {
		return nil, err
	}
	if len(masks) == 0 {
		return nil, fmt.Errorf(
			"no .png mask files found in %q. semantic_segmentation expects "+
				"<dir>/masks/*.png (one PNG mask per image, named <image>_mask.png).",
			filepath.Join(layout.Root, "masks"))
	}
	if layout.Sidecars == nil {
		layout.Sidecars = map[string][]string{}
	}
	layout.Sidecars["masks"] = masks
	layout.TotalBytes += maskBytes

	if layout.TotalBytes > MaxTotalBytes {
		return nil, fmt.Errorf(
			"dataset is %s, exceeds v0.1 cap of %s. For larger datasets, the "+
				"cloud-source path is on the v0.2 roadmap (tracebloc/client#147).",
			HumanBytes(layout.TotalBytes), HumanBytes(MaxTotalBytes))
	}
	return layout, nil
}
