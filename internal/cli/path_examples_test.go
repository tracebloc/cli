package cli

import (
	"runtime"
	"strings"
	"testing"
)

// client#615: a Windows 10 user at "Step 3 of 4 · Where is your data?" saw only Unix examples
// (~/data/...), had nothing in the right shape to copy, and entered a Python-style
// r"C:\Users\..." literal that the CLI could not read. The examples must match the OS the user
// is actually on.
func TestDatasetPathExamplesAreWindowsShapedOnWindows(t *testing.T) {
	tab, img, txt := datasetPathExamples("windows")
	for _, got := range []string{tab, img, txt} {
		if !strings.HasPrefix(got, `C:\`) {
			t.Errorf("Windows example %q must look like a Windows path, or there is still nothing to copy the shape of", got)
		}
		if strings.Contains(got, "~") || strings.Contains(got, "/") {
			t.Errorf("Windows example %q leaks POSIX syntax (~ or /)", got)
		}
	}
	// A folder example that doesn't read as a folder invites a file path instead.
	if !strings.HasSuffix(img, `\`) || !strings.HasSuffix(txt, `\`) {
		t.Errorf("folder examples must end in a separator: images=%q text=%q", img, txt)
	}
	if !strings.HasSuffix(tab, ".csv") {
		t.Errorf("the tabular example is a single CSV file, got %q", tab)
	}
}

func TestDatasetPathExamplesStayPosixElsewhere(t *testing.T) {
	// Guard against "fixing" Windows by regressing everyone else.
	for _, goos := range []string{"linux", "darwin", "freebsd", ""} {
		tab, img, txt := datasetPathExamples(goos)
		for _, got := range []string{tab, img, txt} {
			if !strings.HasPrefix(got, "~/") {
				t.Errorf("goos=%q: example %q should stay POSIX (~/...)", goos, got)
			}
			if strings.Contains(got, `\`) {
				t.Errorf("goos=%q: example %q leaks a backslash", goos, got)
			}
		}
		if !strings.HasSuffix(img, "/") || !strings.HasSuffix(txt, "/") {
			t.Errorf("goos=%q: folder examples must end in /: %q %q", goos, img, txt)
		}
	}
}

// The three modalities must stay distinguishable: identical examples would tell a user nothing
// about which shape their own data should take.
func TestDatasetPathExamplesAreDistinct(t *testing.T) {
	for _, goos := range []string{"windows", "linux"} {
		tab, img, txt := datasetPathExamples(goos)
		if tab == img || img == txt || tab == txt {
			t.Errorf("goos=%q: examples must differ per modality (%q, %q, %q)", goos, tab, img, txt)
		}
	}
}

// The prompt must consult the real OS, not a hardcoded family — the whole point is that a
// Windows user sees Windows paths. runtime.GOOS is the only correct source at the call site.
func TestPromptUsesTheHostOS(t *testing.T) {
	tab, _, _ := datasetPathExamples(runtime.GOOS)
	switch runtime.GOOS {
	case "windows":
		if !strings.HasPrefix(tab, `C:\`) {
			t.Errorf("on windows the prompt should offer a Windows example, got %q", tab)
		}
	default:
		if !strings.HasPrefix(tab, "~/") {
			t.Errorf("on %s the prompt should offer a POSIX example, got %q", runtime.GOOS, tab)
		}
	}
}
