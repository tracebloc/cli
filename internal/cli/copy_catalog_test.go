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

	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// TestCopyCatalog generates the copy catalog under testdata/golden/ — ONE file
// per command, so every user-facing string can be reviewed a screen at a time
// without deploying. Each file leads with what you see when you RUN the command
// (byte-exact, colour off), covers every state/path, and ends with its `--help`.
// zz-all-strings.golden is a completeness backstop: every user-facing string in
// the source, so nothing is missed even on a rare path.
//
// The test fails on drift; regenerate after an intentional copy change:
//
//	TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestCopyCatalog
//
// Files are ordered by how often a user hits them (00 = most). More command
// files (ingest run, resources, doctor, delete, login) land as their driven
// transcripts are wired; this generator makes adding one a single entry below.
func TestCopyCatalog(t *testing.T) {
	bi := BuildInfo{Version: "1.4.4", GitSHA: "0000000", BuildDate: "2026-01-01"}

	// help captures a command's `--help` through the REAL flag path (SetArgs+
	// Execute — exactly what the binary runs), byte-exact.
	help := func(path ...string) string {
		var b bytes.Buffer
		r := NewRootCmd(bi)
		r.SetOut(&b)
		r.SetErr(&b)
		r.SetArgs(append(append([]string{}, path...), "--help"))
		_ = r.Execute()
		return b.String()
	}
	// rndr renders one screen via the real renderer, colour off.
	rndr := func(f func(*ui.Printer)) string {
		var b bytes.Buffer
		f(ui.New(&b, ui.WithColor(false)))
		return b.String()
	}

	// doc assembles one command file: a title, then labelled `$ cmd` blocks
	// (verbatim output), then the command's --help at the bottom.
	type run struct {
		cmd, out string // "$ <cmd>" then verbatim <out>
	}
	doc := func(title, whenSeen string, runs []run, helpCmd string, helpOut string) string {
		var s strings.Builder
		s.WriteString(title + "\n" + strings.Repeat("=", len([]rune(title))) + "\n")
		s.WriteString(whenSeen + "\n")
		for _, r := range runs {
			s.WriteString("\n$ " + r.cmd + "\n")
			s.WriteString(r.out)
		}
		if helpOut != "" {
			s.WriteString("\n\n" + strings.Repeat("-", 60) + "\n--help\n" + strings.Repeat("-", 60) + "\n")
			s.WriteString("$ " + helpCmd + "\n")
			s.WriteString(helpOut)
		}
		return s.String()
	}

	// ── home models (every state resolveHomeModel can produce) ──────────────────
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

	homeFile := doc(
		"tb / tracebloc — home",
		"What you see when you run `tb` (or `tracebloc`) with no arguments. Covers every\nstate the home view resolves to.",
		[]run{
			{"tb   # signed in · secure environment Online", rndr(func(p *ui.Printer) { renderHome(p, online) })},
			{"tb   # signed in · running, couldn't confirm connection", rndr(func(p *ui.Printer) { renderHome(p, noComp) })},
			{"tb   # signed in · running, backend reports not online", rndr(func(p *ui.Printer) { renderHome(p, notOnline) })},
			{"tb   # signed in · starting up", rndr(func(p *ui.Printer) { renderHome(p, starting) })},
			{"tb   # signed in · offline (can't reach it)", rndr(func(p *ui.Printer) { renderHome(p, offline) })},
			{"tb   # signed in · no secure environment on this machine", rndr(func(p *ui.Printer) { renderHome(p, noEnv) })},
			{"tb   # not signed in", rndr(func(p *ui.Printer) { renderHome(p, signedOut) })},
		},
		"tracebloc --help", help(),
	)

	// ── data list ───────────────────────────────────────────────────────────────
	sample := []push.DatasetInfo{
		{Name: "xray_train", Intent: "train", Task: "image_classification", Records: 12000, Classes: 2, Extension: "jpg", SizeBytes: 1 << 30},
		{Name: "xray_test", Intent: "test", Task: "image_classification", Records: 3000, Classes: 2, Extension: "jpg", SizeBytes: 256 << 20},
		{Name: "ingest_run_journal", System: true},
	}
	dataListFile := doc(
		"tb data list — list your datasets",
		"What you see when you run `tb data list`. Covers empty, populated, and --all.",
		[]run{
			{"tb data list   # no datasets yet", rndr(func(p *ui.Printer) { renderDataList(p, "hello-world", nil, false) })},
			{"tb data list   # with datasets (system tables hidden)", rndr(func(p *ui.Printer) { renderDataList(p, "hello-world", sample, false) })},
			{"tb data list --all   # including system tables", rndr(func(p *ui.Printer) { renderDataList(p, "hello-world", sample, true) })},
		},
		"tracebloc data list --help", help("data", "list"),
	)

	files := map[string]string{
		"00-home.golden":      homeFile,
		"02-data-list.golden": dataListFile,
		"zz-all-strings.golden": "every user-facing string in the source (AST-harvested — all arguments, both\n" +
			`"…" and ` + "`…`" + " raw strings). The completeness backstop: catches error paths and\nthe multi-step flows (ingest steps, login, progress) not shown as a screen.\n" +
			"%s/%d are runtime placeholders.\n\n" + strings.Join(quoteAll(harvestMessages(t)), "\n") + "\n",
	}

	update := os.Getenv("TB_UPDATE_GOLDEN") != ""
	for name, content := range files {
		path := filepath.Join("testdata/golden", name)
		if update {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (regenerate: TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestCopyCatalog): %v", path, err)
		}
		if content != string(want) {
			t.Errorf("%s drifted. Regenerate + review the diff:\n  TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestCopyCatalog", path)
		}
	}
	if update {
		t.Logf("wrote %d catalog files under testdata/golden/", len(files))
	}
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strconv.Quote(s)
	}
	return out
}

// harvestMessages parses the user-facing packages and returns every string
// literal passed to a Printer method or an error constructor — ALL arguments
// (Step labels, MenuRow descriptions, Field values included), both "…" and `…`
// raw strings. Deduped + sorted.
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
