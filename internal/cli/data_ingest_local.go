// The local, cluster-free half of `data ingest`: path expansion + the
// path-existence-first guard, the local dataset summary, and the
// preflight that previews the ingestor's validators on the local data.
// Moved verbatim from data.go (cli#282) — behavior unchanged.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/tracebloc/cli/internal/pathutil"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
)

// sortedKeys returns m's keys in sorted order — used to list a CSV's inferred
// columns in the friendly missing-label message (#214) deterministically.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// expandHome expands a leading ~ (current user or ~user) to a home
// directory, leaving every other path untouched. It's the CLI-local
// name for the shared pathutil.ExpandHome; cluster.expandPath resolves
// to the same helper, so ~-expansion is identical across subcommands
// (a --kubeconfig ~alice/... resolves alice's home just like a data
// ingest path does). See pathutil.ExpandHome for the full contract. (#181)
func expandHome(path string) string {
	return pathutil.ExpandHome(path)
}

// statDatasetPath is the "path existence FIRST" guard (#181): a typo'd
// path fails plainly on the path — a clean "no such file or directory" —
// before any family sniff, label preview, or schema work touches it.
// Both entry points call it: the flag-only path from runDataIngest's 0b
// step, and the guided path from runInteractive (before the family sniff),
// so the invariant holds on every route rather than only the flag path.
func statDatasetPath(path string) error {
	if _, serr := os.Stat(path); serr != nil {
		if errors.Is(serr, os.ErrNotExist) {
			return &exitError{code: 3, err: fmt.Errorf(
				"no such file or directory: %q — check the path to your dataset", path)}
		}
		return &exitError{code: 3, err: fmt.Errorf(
			"can't read %q: %w", path, serr)}
	}
	return nil
}

// printLocalSummary shows what the CLI found on disk plus the ingest
// settings it assembled — the detail under step 1 ("Check your data").
// Mirrors `cluster info`'s section/Field layout.
func printLocalSummary(p *ui.Printer, layout *push.LocalLayout, spec map[string]any) {
	cat, _ := spec["category"].(string)

	p.Section("Local dataset")
	p.Field("root", layout.Root)
	switch {
	case push.IsTabular(cat):
		p.Field("data CSV", layout.LabelsCSV)
		if sch, ok := spec["schema"].(map[string]string); ok {
			p.Field("columns", fmt.Sprintf("%d", len(sch)))
		}
	case push.IsText(cat):
		dir := push.TextSidecarDir(cat)
		p.Field("labels.csv", layout.LabelsCSV)
		p.Field(dir, fmt.Sprintf("%d files", len(layout.Sidecars[dir])))
	default:
		p.Field("labels.csv", layout.LabelsCSV)
		imagesVal := fmt.Sprintf("%d files", len(layout.Images))
		if ext, _ := spec["spec"].(map[string]any); ext != nil {
			if fo, _ := ext["file_options"].(map[string]any); fo != nil {
				if e, _ := fo["extension"].(string); e != "" {
					imagesVal = fmt.Sprintf("%d files (%s)", len(layout.Images), e)
				}
			}
		}
		p.Field("images", imagesVal)
		if anns := layout.Sidecars["annotations"]; len(anns) > 0 {
			p.Field("annotations", fmt.Sprintf("%d files", len(anns)))
		}
		if masks := layout.Sidecars["masks"]; len(masks) > 0 {
			p.Field("masks", fmt.Sprintf("%d files", len(masks)))
		}
	}
	p.Field("total size", push.HumanBytes(layout.TotalBytes))

	p.Section("Ingest settings")
	p.Field("name", fmt.Sprintf("%v", spec["table"]))
	p.Field("task", fmt.Sprintf("%v", spec["category"]))
	p.Field("intent", fmt.Sprintf("%v", spec["intent"]))
	switch lbl := spec["label"].(type) {
	case string:
		p.Field("label column", lbl)
	case map[string]any:
		p.Field("label column", fmt.Sprintf("%v (policy: %v)", lbl["column"], lbl["policy"]))
	}
	if tc, ok := spec["time_column"].(string); ok && tc != "" {
		p.Field("time column", tc)
	}
	p.Field("destination", push.FinalDestPrefix(spec["table"].(string)))
}

// runLocalPreflight maps push.PreflightDataset — THE shared preview
// dispatch, also exercised verbatim by the parity harness — onto the CLI's
// conventions: notes print dim to errOut, a BadFlag problem exits 2 (fix a
// flag), anything else exits 3 (fix the data).
func runLocalPreflight(a runDataIngestArgs, layout *push.LocalLayout, errOut io.Writer) error {
	notes, problem := push.PreflightDataset(a.Spec, layout)
	for _, n := range notes {
		_, _ = fmt.Fprintln(errOut, n)
	}
	if problem == nil {
		return nil
	}
	code := 3
	if problem.BadFlag {
		code = 2
	}
	return &exitError{code: code, err: problem.Err}
}
