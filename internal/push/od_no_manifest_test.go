package push

import (
	"image"
	"image/jpeg"
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
	// A REAL decodable JPEG, not just magic bytes: PreflightDataset opens every
	// image (ValidateImages), so a stub would fail there and mask what these
	// tests are actually about.
	f, err := os.Create(filepath.Join(imgs, "001.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, image.NewRGBA(image.Rect(0, 0, 32, 32)), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ann := filepath.Join(dir, "annotations")
	if err := os.MkdirAll(ann, 0o755); err != nil {
		t.Fatal(err)
	}
	// TWO classes, in one file: object detection needs >= 2 distinct classes
	// (LabelDiversityValidator in-cluster, CheckVOCLabelDiversity locally), and
	// one image carrying both is the shape that would false-reject if the check
	// counted annotation FILES instead of classes.
	if err := os.WriteFile(filepath.Join(ann, "001.xml"),
		[]byte("<annotation><filename>001.jpg</filename>"+
			"<object><name>car</name></object>"+
			"<object><name>sign</name></object></annotation>"), 0o644); err != nil {
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

// TestObjectDetectionDoesNotUseALabelColumn: `--label-column` must be
// out-of-scope for a manifest-free category (Bugbot).
//
// The builder drops the value, so accepting the flag silently discarded the
// user's answer and let Review echo a column that never shipped — the same
// defect the self-supervised-text scope already guarded against, one family
// over. One predicate now covers both reasons a category has no user-declared
// label, so a future one cannot be wired without deciding this.
func TestObjectDetectionDoesNotUseALabelColumn(t *testing.T) {
	if UsesLabelColumn("object_detection") {
		t.Error("object_detection has no user-declared label — it reads <object><name>")
	}
	if UsesLabelColumn("masked_language_modeling") {
		t.Error("self-supervised text has no label column")
	}
	for _, c := range []string{
		"image_classification", "keypoint_detection", "text_classification",
		"tabular_classification", "semantic_segmentation",
	} {
		if !UsesLabelColumn(c) {
			t.Errorf("%s does ship a user-declared label column", c)
		}
	}
}

// TestFileCountExcludesAnAbsentManifest: FileCount drives the staging progress
// total, so counting a labels.csv the tar never writes would leave the upload
// permanently short of its own total (Bugbot: staging still packaged it).
func TestFileCountExcludesAnAbsentManifest(t *testing.T) {
	layout, err := DiscoverObjectDetection(mkODNoManifest(t))
	if err != nil {
		t.Fatalf("DiscoverObjectDetection: %v", err)
	}
	if got := layout.FileCount(); got != 2 {
		t.Errorf("FileCount = %d, want 2 (one image + one annotation)", got)
	}
}

// TestPreflightAcceptsAManifestFreeDataset is the guard for the defect that
// mattered most (Bugbot, high): discovery stopped requiring labels.csv, but the
// IMAGE preflight still opened that path for encoding, row and header checks.
// A perfectly valid images+annotations dataset therefore failed locally with a
// missing-file error and exit 3, before anything reached the cluster —
// end-to-end broken for the very category this work exists to enable.
//
// Driven through the real PreflightDataset over a real on-disk dataset, because
// that is the only level at which the bug was visible: every unit below it
// passed.
func TestPreflightAcceptsAManifestFreeDataset(t *testing.T) {
	dir := mkODNoManifest(t)
	layout, err := DiscoverObjectDetection(dir)
	if err != nil {
		t.Fatalf("DiscoverObjectDetection: %v", err)
	}

	_, problem := PreflightDataset(SpecArgs{
		Category:  "object_detection",
		Table:     "visdrone_train",
		Intent:    "train",
		Extension: ".jpg",
	}, layout)

	if problem != nil {
		t.Fatalf("preflight rejected a valid manifest-free dataset: %v", problem.Err)
	}
}

// TestPreflightStillRequiresAManifestElsewhere: the negative control. Relaxing
// the image preflight for object_detection must not relax it for the image
// categories that DO carry a manifest.
func TestPreflightStillRequiresAManifestElsewhere(t *testing.T) {
	dir := mkODNoManifest(t) // images/ + annotations/, no labels.csv
	layout, err := DiscoverObjectDetection(dir)
	if err != nil {
		t.Fatalf("DiscoverObjectDetection: %v", err)
	}

	_, problem := PreflightDataset(SpecArgs{
		Category:    "image_classification",
		Table:       "t",
		Intent:      "train",
		LabelColumn: "label",
		Extension:   ".jpg",
	}, layout)

	if problem == nil {
		t.Fatal("image_classification preflight accepted a layout with no manifest")
	}
}

// TestVOCLabelDiversity_LocalPreviewIsNotLost: dropping the local >= 2 class
// check for object_detection would have traded a FAST LOCAL failure for a SLOW
// POST-UPLOAD one — a single-class dataset passing preflight, uploading in
// full, then being rejected in-cluster. That is worse than before backend#1006,
// and it is the kind of regression that ships unnoticed because nothing goes
// red. The premise of #1006 is that the XML carries what the manifest did, so
// the preview reads exactly what the in-cluster gate reads.
func TestVOCLabelDiversity_LocalPreviewIsNotLost(t *testing.T) {
	dir := t.TempDir()
	ann := filepath.Join(dir, "annotations")
	if err := os.MkdirAll(ann, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(ann, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	single := write("a.xml", "<annotation><object><name>car</name></object></annotation>")

	if err := CheckVOCLabelDiversity([]string{single}); err == nil {
		t.Error("a single-class dataset must be rejected LOCALLY, not after the upload")
	}

	// Two classes in ONE file: counting annotation FILES instead of CLASSES
	// would false-reject this, which is the realistic shape (every image holds
	// a car and a sign).
	both := write("b.xml", "<annotation><object><name>car</name></object>"+
		"<object><name>sign</name></object></annotation>")
	if err := CheckVOCLabelDiversity([]string{both}); err != nil {
		t.Errorf("two classes in one annotation must pass: %v", err)
	}
}

// TestVOCClasses_FailsClosedOnUnreadableXML mirrors CheckLabelDiversity's own
// rule: a partial scan under-counts classes, which would either false-reject
// good data or wave through a single-class dataset. Fail closed, and do not
// echo the parser's message — it can quote the document.
func TestVOCClasses_FailsClosedOnUnreadableXML(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.xml")
	if err := os.WriteFile(bad, []byte("<annotation><secret>PATIENT-42</secret>"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := VOCClasses([]string{bad})
	if err == nil {
		t.Fatal("a malformed annotation must fail closed, not silently contribute nothing")
	}
	if strings.Contains(err.Error(), "PATIENT-42") {
		t.Errorf("error leaked document text: %v", err)
	}
	if !strings.Contains(err.Error(), "bad.xml") {
		t.Errorf("error must name the file: %v", err)
	}
}
