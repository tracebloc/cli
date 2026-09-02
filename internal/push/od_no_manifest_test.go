package push

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkODNoManifest builds the post-backend#1006 object_detection layout:
// images/ + annotations/, and deliberately NO labels.csv.
func mkODNoManifest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	imgs := filepath.Join(dir, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgs, "001.jpg"), []byte("\xff\xd8\xff\xe0"), 0o644); err != nil {
		t.Fatal(err)
	}
	ann := filepath.Join(dir, "annotations")
	if err := os.MkdirAll(ann, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ann, "001.xml"),
		[]byte("<annotation><filename>001.jpg</filename>"+
			"<object><name>car</name></object></annotation>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDiscoverObjectDetection_NoLabelsCSV is the point of backend#1006 on this
// side: the ingestor enumerates OD records from annotations/*.xml, so requiring
// a manifest here would make the CLI demand a file the ingestor now rejects.
func TestDiscoverObjectDetection_NoLabelsCSV(t *testing.T) {
	dir := mkODNoManifest(t)

	layout, err := DiscoverObjectDetection(dir)
	if err != nil {
		t.Fatalf("DiscoverObjectDetection without labels.csv: %v", err)
	}
	if layout.LabelsCSV != "" {
		t.Errorf("LabelsCSV = %q, want empty — there is no manifest to record", layout.LabelsCSV)
	}
	if len(layout.Images) != 1 {
		t.Errorf("Images = %d, want 1", len(layout.Images))
	}
	if got := len(layout.Sidecars["annotations"]); got != 1 {
		t.Errorf("annotations sidecars = %d, want 1", got)
	}
}

// TestDiscoverObjectDetection_StrayManifestIsIgnored: a labels.csv left over
// from a pre-#1006 dataset must not break discovery. The CLI simply does not
// read or stage it — failing here would punish users for a harmless leftover,
// and the config-level rejection (schema `csv` for object_detection) is what
// actually guards the ingest.
func TestDiscoverObjectDetection_StrayManifestIsIgnored(t *testing.T) {
	dir := mkODNoManifest(t)
	if err := os.WriteFile(filepath.Join(dir, "labels.csv"),
		[]byte("filename,image_label\n001.jpg,car\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	layout, err := DiscoverObjectDetection(dir)
	if err != nil {
		t.Fatalf("stray labels.csv broke discovery: %v", err)
	}
	if layout.LabelsCSV != "" {
		t.Errorf("LabelsCSV = %q, want empty — a stray manifest must not be staged", layout.LabelsCSV)
	}
}

// TestDiscover_StillRequiresLabelsCSV is the other half: relaxing the walk for
// object_detection must not relax it for the categories that DO have a
// manifest. Without this, the #1006 change could silently make every image
// dataset manifest-optional.
func TestDiscover_StillRequiresLabelsCSV(t *testing.T) {
	dir := mkODNoManifest(t) // images/ + annotations/, no labels.csv

	_, err := Discover(dir)
	if err == nil {
		t.Fatal("Discover accepted a directory with no labels.csv")
	}
	if !strings.Contains(err.Error(), "labels.csv") {
		t.Errorf("error %q does not mention labels.csv", err)
	}
}

// TestObjectDetectionErrorDoesNotDemandAManifest: the shared walk's messages
// are parameterised, so an OD user missing images/ is told what OD actually
// needs rather than being sent to create a labels.csv the ingestor rejects.
func TestObjectDetectionErrorDoesNotDemandAManifest(t *testing.T) {
	dir := t.TempDir()
	ann := filepath.Join(dir, "annotations")
	if err := os.MkdirAll(ann, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := DiscoverObjectDetection(dir)
	if err == nil {
		t.Fatal("expected an error for a dataset with no images/")
	}
	if strings.Contains(err.Error(), "labels.csv") {
		t.Errorf("object_detection error still mentions labels.csv: %q", err)
	}
}

// TestNoManifestCategory pins the registry fact the spec builder and the walk
// both read, so the behaviour can't be re-hardcoded to an id list.
func TestNoManifestCategory(t *testing.T) {
	if !NoManifestCategory("object_detection") {
		t.Error("object_detection must be manifest-free (backend#1006)")
	}
	for _, c := range []string{
		"image_classification", "keypoint_detection", "semantic_segmentation",
		"text_classification", "tabular_classification",
	} {
		if NoManifestCategory(c) {
			t.Errorf("%s must still carry a labels.csv manifest", c)
		}
	}
}

// TestSpecBuild_ObjectDetectionOmitsCSVAndLabel: the schema REJECTS both keys
// for object_detection rather than ignoring them, so emitting either would
// build a spec the ingestor refuses outright.
func TestSpecBuild_ObjectDetectionOmitsCSVAndLabel(t *testing.T) {
	spec := SpecArgs{
		Category:    "object_detection",
		Table:       "visdrone_train",
		Intent:      "train",
		LabelColumn: "image_label",
		Extension:   ".jpg",
	}.Build()

	if _, ok := spec["csv"]; ok {
		t.Errorf("spec emitted csv = %v; object_detection has no manifest", spec["csv"])
	}
	if _, ok := spec["label"]; ok {
		t.Errorf("spec emitted label = %v; object_detection derives it from <object><name>", spec["label"])
	}
	if _, ok := spec["annotations"]; !ok {
		t.Error("spec must still emit annotations/ — that is where the records come from")
	}
	if _, ok := spec["images"]; !ok {
		t.Error("spec must still emit images/")
	}
}

// TestSpecBuild_OtherImageCategoriesKeepCSVAndLabel: the negative control. If
// this ever fails alongside the test above, the omission has leaked from
// object_detection to the whole image family.
func TestSpecBuild_OtherImageCategoriesKeepCSVAndLabel(t *testing.T) {
	for _, category := range []string{"image_classification", "keypoint_detection"} {
		spec := SpecArgs{
			Category:          category,
			Table:             "t",
			Intent:            "train",
			LabelColumn:       "image_label",
			TargetSize:        []int{256, 256},
			NumberOfKeypoints: 9,
		}.Build()

		if _, ok := spec["csv"]; !ok {
			t.Errorf("%s: spec dropped csv", category)
		}
		if spec["label"] != "image_label" {
			t.Errorf("%s: spec label = %v, want image_label", category, spec["label"])
		}
	}
}
