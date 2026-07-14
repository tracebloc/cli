package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// These pin cli prompt-validators + review renderers at boundaries mutation
// testing (gremlins) found unguarded — the review panel is the user's last look
// before a destructive ingest/create, so a field that silently stops rendering
// (or renders when it shouldn't) is a real confirm-gate regression.

// TestValidatePositiveInt kills the `n <= 0` → `n < 0` boundary (interactive.go):
// "0" must be REJECTED (a positive integer is > 0), which `n < 0` would accept.
func TestValidatePositiveInt(t *testing.T) {
	for _, s := range []string{"1", "42", " 5 "} { // trimmed
		if err := validatePositiveInt(s); err != nil {
			t.Errorf("validatePositiveInt(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"0", "-1", "abc", "", "  ", "1.5"} { // "0" is the boundary killer
		if err := validatePositiveInt(s); err == nil {
			t.Errorf("validatePositiveInt(%q) = nil, want error", s)
		}
	}
}

// TestBoundedInt_Bounds kills the `n < lo` → `<= lo` and `n > hi` → `>= hi`
// boundary mutants (resources_set.go): the range is INCLUSIVE, so exactly lo and
// exactly hi must be accepted.
func TestBoundedInt_Bounds(t *testing.T) {
	v := boundedInt(1, 10)
	for _, s := range []string{"1", "10", "5"} { // 1 and 10 are the boundary killers
		if err := v(s); err != nil {
			t.Errorf("boundedInt(1,10)(%q) = %v, want nil (inclusive bounds)", s, err)
		}
	}
	for _, s := range []string{"0", "11", "-3", "abc", ""} {
		if err := v(s); err == nil {
			t.Errorf("boundedInt(1,10)(%q) = nil, want error", s)
		}
	}
}

// TestRenderReview_FieldGating kills the optional-field `!= ""` / `> 0` negation
// mutants: every optional field must render iff its value is set, and the
// resolution/schema switches must fall to the category-driven default when the
// flag is empty.
func TestRenderReview_FieldGating(t *testing.T) {
	render := func(a *runDataIngestArgs) string {
		var b bytes.Buffer
		renderReview(ui.New(&b, ui.WithColor(false)), a)
		return b.String()
	}

	// All optionals set → every label + value renders.
	full := render(&runDataIngestArgs{
		LocalPath:      "/data/x",
		TargetSizeFlag: "224x224",
		MinSizeFlag:    "64x64",
		SchemaFlag:     "a:INT",
		Spec: push.SpecArgs{
			Table: "t1", Category: "image_classification", Intent: "train",
			LabelColumn: "lc", NumberOfKeypoints: 17, LabelPolicy: "strict", TimeColumn: "ts",
		},
	})
	for _, want := range []string{
		"name", "t1", "task", "image_classification", "intent", "train", "path", "/data/x",
		"label column", "lc", "keypoints", "17", "min size", "64x64",
		"resolution", "224x224", "schema", "a:INT", "label policy", "strict", "time column", "ts",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("full review missing %q:\n%s", want, full)
		}
	}

	// All optionals empty + a category that is neither image nor tabular → none of
	// the optional labels (nor the switch defaults) render.
	minimal := render(&runDataIngestArgs{LocalPath: "/d", Spec: push.SpecArgs{Table: "t", Category: "", Intent: "train"}})
	for _, absent := range []string{"label column", "keypoints", "min size", "resolution", "schema", "label policy", "time column"} {
		if strings.Contains(minimal, absent) {
			t.Errorf("minimal review must NOT show %q:\n%s", absent, minimal)
		}
	}

	// Category-driven defaults when the flag is empty (kills the IsImage/IsTabular
	// switch arms).
	img := render(&runDataIngestArgs{Spec: push.SpecArgs{Category: "image_classification"}})
	if !strings.Contains(img, "auto-detect") {
		t.Errorf("image category, no --target-size → resolution auto-detect:\n%s", img)
	}
	tab := render(&runDataIngestArgs{Spec: push.SpecArgs{Category: "tabular_classification"}})
	if !strings.Contains(tab, "infer from CSV") {
		t.Errorf("tabular category, no --schema → schema infer from CSV:\n%s", tab)
	}
}

// TestRenderClientReview_OptionalFields kills the `location != ""` / `clusterID
// != ""` negation mutants (client.go): name + namespace always render; location
// and cluster render ONLY when non-empty.
func TestRenderClientReview_OptionalFields(t *testing.T) {
	render := func(name, ns, loc, cluster string) string {
		var b bytes.Buffer
		renderClientReview(ui.New(&b, ui.WithColor(false)), name, ns, loc, cluster)
		return b.String()
	}

	full := render("Lab A", "lab-a", "FR", "cl-123")
	for _, want := range []string{"name", "Lab A", "namespace", "lab-a", "location", "FR", "cluster", "cl-123"} {
		if !strings.Contains(full, want) {
			t.Errorf("full client review missing %q:\n%s", want, full)
		}
	}

	bare := render("Lab B", "lab-b", "", "")
	if strings.Contains(bare, "location") {
		t.Errorf("empty location must not render a location line:\n%s", bare)
	}
	if strings.Contains(bare, "cluster") {
		t.Errorf("empty clusterID must not render a cluster line:\n%s", bare)
	}
}
