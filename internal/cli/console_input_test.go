package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// enableKeyEventInput sits in front of every interactive prompt, so its failure
// modes matter more than its happy path: if it can ever panic or return nil, a
// console quirk turns into a crashed command. Under `go test` stdin is not a
// console, so on Windows this exercises the GetConsoleMode-failed branch and off
// Windows the no-op — both of which must be silently harmless.
func TestEnableKeyEventInputIsAlwaysSafe(t *testing.T) {
	restore := enableKeyEventInput()
	if restore == nil {
		t.Fatal("enableKeyEventInput returned a nil restore func; callers `defer restore()` unconditionally and would panic")
	}
	restore()

	// Prompts run in sequence in the guided flow, so this is called and restored
	// many times per command. Nested/repeated use must not blow up either.
	for i := 0; i < 3; i++ {
		r := enableKeyEventInput()
		if r == nil {
			t.Fatalf("call %d returned a nil restore func", i)
		}
		defer r()
	}
}

// The bug this guards (#475): pressing the down arrow in the ingest task-type
// Select typed "[B" into the filter and never moved the selection, because the
// console was delivering arrows as ANSI escapes instead of VK_DOWN key events.
// The fix only holds while every prompt is wrapped, and the failure is invisible
// off Windows — nothing in a macOS/Linux test run or in CI would notice a fourth
// prompt added without the wrapper. So assert it structurally, at the seam where
// survey is actually called.
func TestEverySurveyPromptClearsVirtualTerminalInput(t *testing.T) {
	const file = "interactive.go"

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	// Walk each function that calls survey.AskOne and require the same function
	// body to also call enableKeyEventInput.
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}

		var callsAskOne, callsEnable bool
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr: // survey.AskOne
				if pkg, ok := f.X.(*ast.Ident); ok && pkg.Name == "survey" && f.Sel.Name == "AskOne" {
					callsAskOne = true
				}
			case *ast.Ident: // enableKeyEventInput()
				if f.Name == "enableKeyEventInput" {
					callsEnable = true
				}
			}
			return true
		})

		if callsAskOne && !callsEnable {
			t.Errorf("%s: %s calls survey.AskOne without `defer enableKeyEventInput()()`.\n"+
				"On Windows the console can deliver arrow keys as ANSI escape sequences "+
				"instead of VK_UP/VK_DOWN key events; survey only understands the key events, "+
				"so arrows stop navigating and leak \"[B\" into the prompt (#475). "+
				"Every prompt needs the wrapper — it is a no-op everywhere else.",
				file, fn.Name.Name)
		}
		return true
	})
}

// Both build-tagged halves must expose the identical signature, or one platform
// fails to compile — and CI only builds some of them. Cheap to assert here.
func TestConsoleInputBuildTagsCoverEveryPlatform(t *testing.T) {
	for _, tc := range []struct{ file, wantTag string }{
		{"console_input_windows.go", "//go:build windows"},
		{"console_input_other.go", "//go:build !windows"},
	} {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("reading %s: %v", tc.file, err)
		}
		body := string(src)
		if !strings.HasPrefix(body, tc.wantTag+"\n") {
			t.Errorf("%s must start with %q so the two halves are mutually exclusive and exhaustive", tc.file, tc.wantTag)
		}
		if !regexp.MustCompile(`func enableKeyEventInput\(\) \(restore func\(\)\) \{`).MatchString(body) {
			t.Errorf("%s: enableKeyEventInput signature drifted; both halves must match or one platform will not build", tc.file)
		}
	}
}
