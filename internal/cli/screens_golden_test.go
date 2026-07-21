package cli

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// TestScreensGolden pins EVERY piece of user-facing copy in one committed file,
// testdata/screens.golden, so wording AND exact layout (line breaks, tabs,
// blank lines) can be reviewed without deploying — read the file, or the diff on
// any PR that changes copy. A verbatim terminal transcript in three parts:
//
//	A. Commands  — the `--help` of every command, byte-exact (real --help path)
//	B. Screens   — the stateful views rendered verbatim (home, data list, review, …)
//	C. Messages  — every user-facing STRING in the source, harvested via AST (so
//	               error paths + live flows — ingest steps, login, progress —
//	               that aren't a single rendered screen are still all here).
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
	cat.WriteString("(line breaks, tabs, blank lines — all as the terminal prints them). The final\n")
	cat.WriteString("section indexes every user-facing string, incl. multi-step flows not shown as\n")
	cat.WriteString("a single screen. Regenerate: TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestScreensGolden\n")

	block := func(cmd, output string) {
		cat.WriteString("$ " + cmd + "\n")
		cat.WriteString(output)
	}

	// ── PART A: every command's --help, through the real flag path ──────────────
	cat.WriteString("\n\n" + strings.Repeat("=", 78) + "\n= COMMANDS — every `--help`, byte-exact\n" + strings.Repeat("=", 78) + "\n")
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
			walk(s, append(append([]string{}, prefix...), s.Name()))
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

	ingestReview := &runDataIngestArgs{
		LocalPath: "./data",
		Spec:      push.SpecArgs{Table: "xray_train", Category: "image_classification", Intent: "train"},
	}
	render("tb data ingest ./data   # pre-flight review", func(p *ui.Printer) { renderReview(p, ingestReview) })
	render("tb client create   # review", func(p *ui.Printer) { renderClientReview(p, "lukas-macbook", "lukas-macbook", "DE", "a1b2c3d4") })
	render("tb delete   # keep data", func(p *ui.Printer) { renderOffboardSummary(p, "lukas-macbook", true) })
	render("tb delete   # remove data", func(p *ui.Printer) { renderOffboardSummary(p, "lukas-macbook", false) })

	// ── PART C: every user-facing string (AST harvest — catches all args) ───────
	cat.WriteString("\n\n" + strings.Repeat("=", 78) + "\n= MESSAGE INDEX — every user-facing string in the source (templates, not\n= rendered; %s/%d are runtime placeholders). Catches the multi-step flows the\n= transcript above can't show whole: ingest steps + progress, login device flow,\n= delete confirmation, and every error/hint.\n" + strings.Repeat("=", 78) + "\n\n")
	for _, m := range harvestMessages(t) {
		cat.WriteString("  " + strconv.Quote(m) + "\n")
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

// harvestMessages parses the user-facing packages and returns every string
// literal passed to a Printer method or an error constructor — ALL arguments
// (so Step labels, MenuRow descriptions, Field values are included), both "…" and
// `…` raw strings. Deduped + sorted. A complete index of user-facing copy,
// independent of whether a screen renders it.
func harvestMessages(t *testing.T) []string {
	t.Helper()
	methods := map[string]bool{
		"Successf": true, "Warnf": true, "Errorf": true, "Infof": true, "Hintf": true,
		"Detailf": true, "Para": true, "Section": true, "PromptHint": true, "PromptHeader": true,
		"WarnLine": true, "CrossLine": true, "CheckLine": true, "Step": true, "Action": true,
		"Stat": true, "Field": true, "MenuRow": true, "Banner": true, "Command": true,
	}
	isCopyCall := func(call *ast.CallExpr) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if methods[sel.Sel.Name] {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok {
			return (x.Name == "errors" && sel.Sel.Name == "New") || (x.Name == "fmt" && sel.Sel.Name == "Errorf")
		}
		return false
	}

	seen := map[string]struct{}{}
	fset := token.NewFileSet()
	for _, dir := range []string{".", "../submit", "../push", "../doctor", "../cluster"} {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isCopyCall(call) {
					return true
				}
				for _, arg := range call.Args {
					lit, ok := arg.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					s, uerr := strconv.Unquote(lit.Value)
					if uerr != nil {
						continue
					}
					s = strings.TrimSpace(s)
					// Skip empties and format-only fragments (e.g. "%s", " ") — no words.
					if len([]rune(s)) < 4 || !strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyz") {
						continue
					}
					seen[s] = struct{}{}
				}
				return true
			})
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
