package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// TestScreensGolden pins EVERY piece of user-facing copy in one committed file,
// testdata/screens.golden, so wording + spacing can be reviewed without deploying
// — read the file, or the diff on any PR that changes copy. Three parts:
//
//	A. Commands   — the `--help` of every command (all Short/Long/flag copy, exact)
//	B. Screens    — the stateful views rendered plain (home, data list, review, …)
//	C. Messages   — a harvested, deduped index of every user-facing string in the
//	                source, so error paths + flows not rendered above are still here.
//
// The test fails on drift; regenerate after an intentional copy change:
//
//	TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestScreensGolden
func TestScreensGolden(t *testing.T) {
	const goldenPath = "testdata/screens.golden"
	var cat strings.Builder

	cat.WriteString("tracebloc CLI — complete copy catalog\n")
	cat.WriteString("═════════════════════════════════════\n")
	cat.WriteString("Every user-facing string, generated from the source. Read this (or the\n")
	cat.WriteString("diff on any PR that changes it) instead of deploying to review copy.\n")
	cat.WriteString("Regenerate: TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestScreensGolden\n")

	// ── PART A: every command's --help ─────────────────────────────────────────
	cat.WriteString("\n\n" + strings.Repeat("█", 3) + " A. COMMANDS — every `--help` " + strings.Repeat("█", 30) + "\n")
	root := NewRootCmd(BuildInfo{Version: "1.4.4", GitSHA: "0000000", BuildDate: "2026-01-01"})
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		var b bytes.Buffer
		c.SetOut(&b)
		c.SetErr(&b)
		c.InitDefaultHelpFlag()
		_ = c.Help()
		cat.WriteString("\n┌─ " + c.CommandPath() + " --help " + strings.Repeat("─", 40) + "\n")
		cat.WriteString(b.String())
		subs := append([]*cobra.Command(nil), c.Commands()...)
		sort.Slice(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
		for _, s := range subs {
			if s.Name() != "help" && s.Name() != "completion" {
				walk(s)
			}
		}
	}
	walk(root)

	// ── PART B: rendered screens (plain) ────────────────────────────────────────
	cat.WriteString("\n\n" + strings.Repeat("█", 3) + " B. SCREENS — rendered plain " + strings.Repeat("█", 31) + "\n")
	screen := func(title string, f func(*ui.Printer)) {
		var b bytes.Buffer
		f(ui.New(&b, ui.WithColor(false)))
		cat.WriteString("\n┌─ " + title + " " + strings.Repeat("─", maxi(0, 55-len(title))) + "\n")
		cat.WriteString(b.String())
		cat.WriteString("└" + strings.Repeat("─", 60) + "\n")
	}

	online := homeModel{
		state: homeOnline, email: "lukas@tracebloc.io", name: "Lukas", envName: "hello-world",
		compute: computeInfo{CPU: 12, MemGiB: 23}, hasCompute: true, inv: binTB, fullMenu: true, hasResources: true,
	}
	noComp := online
	noComp.state, noComp.hasCompute, noComp.compute = homeRunning, false, computeInfo{}
	notOnline := noComp
	notOnline.confirmedNotOnline = true
	starting := noComp
	starting.state = homeStarting
	offline := noComp
	offline.state = homeOffline
	noEnv := noComp
	noEnv.state, noEnv.fullMenu, noEnv.envName = homeNoEnv, false, ""
	signedOut := homeModel{state: homeNotSignedIn, inv: binTB}

	screen("tb — home · Online", func(p *ui.Printer) { renderHome(p, online) })
	screen("tb — home · running (couldn't confirm)", func(p *ui.Printer) { renderHome(p, noComp) })
	screen("tb — home · running (backend not online)", func(p *ui.Printer) { renderHome(p, notOnline) })
	screen("tb — home · starting up", func(p *ui.Printer) { renderHome(p, starting) })
	screen("tb — home · offline", func(p *ui.Printer) { renderHome(p, offline) })
	screen("tb — home · no secure environment", func(p *ui.Printer) { renderHome(p, noEnv) })
	screen("tb — home · not signed in", func(p *ui.Printer) { renderHome(p, signedOut) })

	sample := []push.DatasetInfo{
		{Name: "xray_train", Intent: "train", Task: "image_classification", Records: 12000, Classes: 2, Extension: "jpg", SizeBytes: 1 << 30},
		{Name: "xray_test", Intent: "test", Task: "image_classification", Records: 3000, Classes: 2, Extension: "jpg", SizeBytes: 256 << 20},
		{Name: "ingest_run_journal", System: true},
	}
	screen("tb data list — empty", func(p *ui.Printer) { renderDataList(p, "hello-world", nil, false) })
	screen("tb data list — populated", func(p *ui.Printer) { renderDataList(p, "hello-world", sample, false) })
	screen("tb data list --all — with system tables", func(p *ui.Printer) { renderDataList(p, "hello-world", sample, true) })
	screen("tb client create — review", func(p *ui.Printer) { renderClientReview(p, "lukas-macbook", "lukas-macbook", "DE", "a1b2c3d4") })
	screen("tb delete — offboard summary (keep data)", func(p *ui.Printer) { renderOffboardSummary(p, "lukas-macbook", true) })
	screen("tb delete — offboard summary (remove data)", func(p *ui.Printer) { renderOffboardSummary(p, "lukas-macbook", false) })

	// ── PART C: harvested message index ────────────────────────────────────────
	cat.WriteString("\n\n" + strings.Repeat("█", 3) + " C. MESSAGES — every user-facing string in the source " + strings.Repeat("█", 5) + "\n")
	cat.WriteString("(Deduped, sorted. Catches errors, hints, warnings, and flow copy — ingest,\n")
	cat.WriteString("login, resources — that isn't a rendered screen above. `%…` are placeholders.)\n\n")
	for _, m := range harvestMessages(t) {
		cat.WriteString("  " + m + "\n")
	}

	got := cat.String()
	if os.Getenv("TB_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", goldenPath, len(got))
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s (regenerate: TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestScreensGolden): %v", goldenPath, err)
	}
	if got != string(want) {
		t.Errorf("copy catalog drifted from %s.\nRegenerate + review the diff:\n  TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestScreensGolden", goldenPath)
	}
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// harvestMessages reads the user-facing packages and extracts every string
// literal passed to a Printer method or an error constructor — a complete,
// deduped index of user-facing copy, independent of whether a screen renders it.
func harvestMessages(t *testing.T) []string {
	t.Helper()
	// Printer method call with a double-quoted first arg, or errors.New / fmt.Errorf.
	printer := regexp.MustCompile(`\.(?:Successf|Warnf|Errorf|Infof|Hintf|Detailf|Para|Section|PromptHint|PromptHeader|WarnLine|CrossLine|CheckLine|Step|Action|Stat|Field)\(\s*"((?:[^"\\]|\\.)*)"`)
	errs := regexp.MustCompile(`(?:errors\.New|fmt\.Errorf)\(\s*"((?:[^"\\]|\\.)*)"`)
	seen := map[string]struct{}{}
	// Relative to internal/cli (the test's working dir).
	for _, dir := range []string{".", "../submit", "../push", "../doctor", "../cluster"} {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for _, re := range []*regexp.Regexp{printer, errs} {
				for _, m := range re.FindAllStringSubmatch(string(src), -1) {
					s := strings.TrimSpace(m[1])
					// Skip empties and format-only fragments (" ", "%s") — no real words.
					if len([]rune(s)) < 4 || !strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyz") {
						continue
					}
					seen[s] = struct{}{}
				}
			}
			return nil
		})
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
