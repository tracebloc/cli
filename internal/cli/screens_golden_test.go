package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/ui"
)

// TestScreensGolden renders the CLI's user-facing screens through the REAL
// renderers (plain, colour off — so wording and spacing are what you review) and
// pins them in testdata/screens.golden. That file is the one place to read all
// the copy without deploying: open it, or read the diff on any PR that changes it.
//
// Regenerate after an intentional copy change:
//
//	TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestScreensGolden
//
// Colour off is deliberate: the palette lives in STYLE.md + the ui tone table
// (pinned by brand_tones_test.go); this catalog is about words and whitespace.
func TestScreensGolden(t *testing.T) {
	const goldenPath = "testdata/screens.golden"

	render := func(f func(*ui.Printer)) string {
		var b bytes.Buffer
		f(ui.New(&b, ui.WithColor(false)))
		return b.String()
	}

	// The home view in every state resolveHomeModel can produce. compute is shown
	// only when Online; inv=tb means a `tb` launcher is installed beside the CLI.
	online := homeModel{
		state: homeOnline, email: "lukas@tracebloc.io", name: "Lukas", envName: "hello-world",
		compute: computeInfo{CPU: 12, MemGiB: 23}, hasCompute: true,
		inv: binTB, fullMenu: true, hasResources: true,
	}
	base := online
	base.state, base.hasCompute, base.compute = homeRunning, false, computeInfo{}

	running := base
	runningNotOnline := base
	runningNotOnline.confirmedNotOnline = true
	starting := base
	starting.state = homeStarting
	offline := base
	offline.state = homeOffline
	noEnv := base
	noEnv.state, noEnv.fullMenu, noEnv.envName = homeNoEnv, false, ""
	notSignedIn := homeModel{state: homeNotSignedIn, inv: binTB}

	type screen struct {
		title  string
		render func(*ui.Printer)
	}
	screens := []screen{
		{"tb — home · Online", func(p *ui.Printer) { renderHome(p, online) }},
		{"tb — home · running (couldn't confirm connection)", func(p *ui.Printer) { renderHome(p, running) }},
		{"tb — home · running (backend reports not online)", func(p *ui.Printer) { renderHome(p, runningNotOnline) }},
		{"tb — home · starting up", func(p *ui.Printer) { renderHome(p, starting) }},
		{"tb — home · offline (can't reach it)", func(p *ui.Printer) { renderHome(p, offline) }},
		{"tb — home · no secure environment yet", func(p *ui.Printer) { renderHome(p, noEnv) }},
		{"tb — home · not signed in", func(p *ui.Printer) { renderHome(p, notSignedIn) }},
		{"tb data list — empty", func(p *ui.Printer) { renderDataList(p, "hello-world", nil, false) }},
	}

	var out strings.Builder
	out.WriteString("tracebloc CLI — screen copy catalog\n")
	out.WriteString("═══════════════════════════════════\n")
	out.WriteString("The exact wording + spacing of every screen, rendered from the real\n")
	out.WriteString("renderers (colour off). Read this instead of deploying to see copy.\n")
	out.WriteString("Regenerate: TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestScreensGolden\n")
	for _, s := range screens {
		dashes := 60 - len(s.title)
		if dashes < 0 {
			dashes = 0
		}
		out.WriteString("\n\n┌─ " + s.title + " " + strings.Repeat("─", dashes) + "\n")
		out.WriteString(render(s.render))
		out.WriteString("└" + strings.Repeat("─", 62) + "\n")
	}
	got := out.String()

	if os.Getenv("TB_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s (regenerate: TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestScreensGolden): %v", goldenPath, err)
	}
	if got != string(want) {
		t.Errorf("screen copy drifted from %s.\nRegenerate + review the diff:\n  TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestScreensGolden", goldenPath)
	}
}
