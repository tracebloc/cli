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
// testdata/screens.golden, so wording AND exact layout (line breaks, tabs,
// leading/trailing blanks) can be reviewed without deploying — read the file, or
// the diff on any PR that changes copy.
//
// It's a verbatim terminal transcript: each block is `$ <command>` followed by
// the BYTE-EXACT output. Help is captured through the real `--help` flag path
// (SetArgs+Execute — exactly what the binary runs), screens through the real
// renderers. Then an appendix indexes every remaining user-facing string.
//
// The test fails on drift; regenerate after an intentional copy change:
//
//	TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestScreensGolden
func TestScreensGolden(t *testing.T) {
	const goldenPath = "testdata/screens.golden"
	bi := BuildInfo{Version: "1.4.4", GitSHA: "0000000", BuildDate: "2026-01-01"}
	var cat strings.Builder

	cat.WriteString("tracebloc CLI — complete copy catalog\n")
	cat.WriteString("A verbatim transcript: each `$ command` is followed by its byte-exact output\n")
	cat.WriteString("(line breaks, tabs, blank lines — all as the terminal prints them).\n")
	cat.WriteString("Regenerate: TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestScreensGolden\n")

	// block writes `$ <cmd>\n` then the output VERBATIM — nothing else. The only
	// whitespace between entries is each output's own real leading/trailing blank
	// lines, so what you read is byte-for-byte what the terminal prints.
	block := func(cmd, output string) {
		cat.WriteString("$ " + cmd + "\n")
		cat.WriteString(output)
	}

	// ── PART A: every command's --help, through the real flag path ──────────────
	cat.WriteString("\n\n" + strings.Repeat("=", 78) + "\n= COMMANDS — every `--help`, byte-exact\n" + strings.Repeat("=", 78) + "\n")
	// Enumerate every command path (incl. hidden — a user can still run them).
	var paths [][]string
	var walk func(c *cobra.Command, prefix []string)
	walk = func(c *cobra.Command, prefix []string) {
		paths = append(paths, prefix)
		subs := append([]*cobra.Command(nil), c.Commands()...)
		sort.Slice(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
		for _, s := range subs {
			if s.Name() == "help" || s.Name() == "completion" {
				continue
			}
			child := append(append([]string{}, prefix...), s.Name())
			walk(s, child)
		}
	}
	walk(NewRootCmd(bi), nil)
	for _, p := range paths {
		var b bytes.Buffer
		r := NewRootCmd(bi)
		r.SetOut(&b)
		r.SetErr(&b)
		r.SetArgs(append(append([]string{}, p...), "--help"))
		_ = r.Execute()
		block(strings.TrimSpace("tracebloc "+strings.Join(p, " "))+" --help", b.String())
	}

	// ── PART B: screens, rendered verbatim (same code the binary runs) ──────────
	cat.WriteString("\n\n" + strings.Repeat("=", 78) + "\n= SCREENS — byte-exact renderer output\n" + strings.Repeat("=", 78) + "\n")
	render := func(cmd string, f func(*ui.Printer)) {
		var b bytes.Buffer
		f(ui.New(&b, ui.WithColor(false)))
		block(cmd, b.String())
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

	render("tb   # home · Online", func(p *ui.Printer) { renderHome(p, online) })
	render("tb   # home · running (couldn't confirm)", func(p *ui.Printer) { renderHome(p, noComp) })
	render("tb   # home · running (backend not online)", func(p *ui.Printer) { renderHome(p, notOnline) })
	render("tb   # home · starting up", func(p *ui.Printer) { renderHome(p, starting) })
	render("tb   # home · offline", func(p *ui.Printer) { renderHome(p, offline) })
	render("tb   # home · no secure environment", func(p *ui.Printer) { renderHome(p, noEnv) })
	render("tb   # home · not signed in", func(p *ui.Printer) { renderHome(p, signedOut) })

	sample := []push.DatasetInfo{
		{Name: "xray_train", Intent: "train", Task: "image_classification", Records: 12000, Classes: 2, Extension: "jpg", SizeBytes: 1 << 30},
		{Name: "xray_test", Intent: "test", Task: "image_classification", Records: 3000, Classes: 2, Extension: "jpg", SizeBytes: 256 << 20},
		{Name: "ingest_run_journal", System: true},
	}
	render("tb data list   # empty", func(p *ui.Printer) { renderDataList(p, "hello-world", nil, false) })
	render("tb data list   # populated", func(p *ui.Printer) { renderDataList(p, "hello-world", sample, false) })
	render("tb data list --all", func(p *ui.Printer) { renderDataList(p, "hello-world", sample, true) })
	render("tb client create   # review", func(p *ui.Printer) { renderClientReview(p, "lukas-macbook", "lukas-macbook", "DE", "a1b2c3d4") })
	render("tb delete   # keep data", func(p *ui.Printer) { renderOffboardSummary(p, "lukas-macbook", true) })
	render("tb delete   # remove data", func(p *ui.Printer) { renderOffboardSummary(p, "lukas-macbook", false) })

	// ── APPENDIX: every remaining user-facing string ────────────────────────────
	cat.WriteString("\n\n" + strings.Repeat("=", 78) + "\n= MESSAGE INDEX — every user-facing string in the source (templates, not\n= rendered; %s/%d are runtime placeholders). Catches errors, hints, and the\n= flows not rendered above (ingest validation, login, resources).\n" + strings.Repeat("=", 78) + "\n\n")
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

// harvestMessages reads the user-facing packages and extracts every string
// literal passed to a Printer method or an error constructor — a complete,
// deduped index of user-facing copy, independent of whether a screen renders it.
func harvestMessages(t *testing.T) []string {
	t.Helper()
	printer := regexp.MustCompile(`\.(?:Successf|Warnf|Errorf|Infof|Hintf|Detailf|Para|Section|PromptHint|PromptHeader|WarnLine|CrossLine|CheckLine|Step|Action|Stat|Field)\(\s*"((?:[^"\\]|\\.)*)"`)
	errs := regexp.MustCompile(`(?:errors\.New|fmt\.Errorf)\(\s*"((?:[^"\\]|\\.)*)"`)
	seen := map[string]struct{}{}
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
