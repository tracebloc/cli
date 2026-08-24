package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The command-surface contract (RFC-BACKEND-2363 D2, backend#2366).
//
// WHAT THIS PINS, and why it is not a list. Nothing pinned the CLI's command
// surface before this file, which is how a restated count of "16 leaf commands"
// survived review while being wrong three ways at once: in its TOTAL (19
// leaves), in its COMPOSITION (it carried the hidden deprecated `ingest
// validate` and omitted the canonical `data validate`, bare `resources`,
// `delete`, `upgrade` and the hidden `cluster doctor`), and in its ARITHMETIC
// (two sub-lists summing to 15 under a heading claiming 16). A fresh
// hand-written list here would be the same defect with a `_test.go` suffix.
//
// So every command in these tests comes out of walkTree over the tree
// NewRootCmd actually builds — the same tree main.go dispatches — and the
// classification comes off the commands themselves (surface.go's annotations).
// The golden file is GENERATED from that walk, never typed: it is the reviewable
// artifact, and it moves when the surface moves. Add, hide, rename or re-parent
// a command and the golden diff says so at PR time.
//
// WHAT IS DERIVED AND WHAT IS DECLARED — stated, because a check whose limits
// are unstated gets trusted for more than it does:
//
//   - The command SET, each command's leaf/group shape, its effective
//     visibility (hidden by itself or by an ancestor), its aliases and its flag
//     surface are all derived from the live tree. Nothing can drift here.
//   - The CLUSTER half of each class is cross-checked against the code: a
//     command that talks to a cluster takes a --kubeconfig, and one that does
//     not, does not. TestClusterClassAgreesWithTheKubeconfigFlag makes a
//     mis-declared class on that axis fail rather than sit there.
//   - The BACKEND/AUTH half is a declaration reviewed by a human. Deriving it
//     would need a whole-program call graph (the api client is reached through
//     several same-package hops), and a subtly-wrong derivation would be worse
//     than an honest declaration: it would launder the wrong answer as a
//     measurement. It is pinned, not proven.
//
// Regenerate the golden after a deliberate surface change, and review the diff:
//
//	TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestCommandSurfaceGolden
const surfaceGolden = "testdata/golden/command-surface.golden"

// surfaceEntry is one row of the derived surface.
type surfaceEntry struct {
	path        string
	class       string
	leaf        bool
	hidden      bool              // effective: this command or any ancestor
	aliases     []string          // other names this command answers to
	escalations map[string]string // flag name -> class it escalates to
}

// invocable reports whether a user can run this command and get work done. Every
// leaf is invocable; a parent is invocable only when it declares a runtime class
// of its own (bare `tracebloc resources` SHOWS, bare `tracebloc data` prints
// help). This is the distinction the wrong count collapsed.
func (e surfaceEntry) invocable() bool { return e.leaf || e.class != classDispatchOnly }

// needsCluster reports whether reaching this command's full behaviour needs a
// cluster — at its base class or behind one of its escalating flags.
func (e surfaceEntry) needsCluster() bool {
	if classesNeedingCluster[e.class] {
		return true
	}
	for _, c := range e.escalations {
		if classesNeedingCluster[c] {
			return true
		}
	}
	return false
}

// derivedSurface walks the live tree and reads each command's own declaration.
// It is the single producer every test below consumes: one walk, one place where
// "the surface" is defined.
func derivedSurface(t *testing.T) []surfaceEntry {
	t.Helper()
	isolateConfig(t)
	root := NewRootCmd(testBuildInfo())

	commands := walkTree(root)
	// The anchor. An empty or half-built walk makes every assertion below vacuous
	// and reads exactly like a clean pass in the log, so refuse it outright. The
	// floor is deliberately far below the real total (26 at the time of writing)
	// — it catches "the tree was not built", not "the tree changed"; the golden
	// is what catches that.
	if len(commands) < 15 {
		t.Fatalf("walked only %d commands — the tree was not built", len(commands))
	}

	out := make([]surfaceEntry, 0, len(commands))
	for _, cmd := range commands {
		out = append(out, surfaceEntry{
			path:        cmd.CommandPath(),
			class:       cmd.Annotations[runtimeClassAnnotation],
			leaf:        len(cmd.Commands()) == 0,
			hidden:      effectivelyHidden(cmd),
			aliases:     append([]string(nil), cmd.Aliases...),
			escalations: parseEscalations(cmd.Annotations[runtimeEscalationsAnnotation]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// effectivelyHidden reports whether a user browsing `--help` can find this
// command. `ingest validate` is not itself Hidden, but its parent `ingest` is —
// so it is off the surface, and a count that read only cmd.Hidden would list it
// as visible. That is one of the composition errors this file exists to prevent.
func effectivelyHidden(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Hidden {
			return true
		}
	}
	return false
}

// parseEscalations reads the `flag=class,flag=class` annotation. A malformed
// entry is returned as an empty class so the validation test can name it, rather
// than being silently dropped here.
func parseEscalations(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		flag, class, _ := strings.Cut(strings.TrimSpace(part), "=")
		out[strings.TrimSpace(flag)] = strings.TrimSpace(class)
	}
	return out
}

// --- every command declares a class ------------------------------------------

// TestEveryCommandDeclaresItsRuntimeClass is the fail-closed half of the
// contract: an undeclared command is a FINDING, not a pass. A command added
// without a class reddens here on the day it lands — which is also why adding
// one is the mutation this file is proved with.
func TestEveryCommandDeclaresItsRuntimeClass(t *testing.T) {
	for _, e := range derivedSurface(t) {
		switch {
		case e.class == "":
			t.Errorf("%s declares no runtime class — add "+
				"`Annotations: runtimeClassFor(class…)` to its cobra.Command "+
				"(see surface.go for which class)", e.path)
		case !runtimeClasses[e.class]:
			t.Errorf("%s declares class %q, which is not one of the classes in "+
				"surface.go", e.path, e.class)
		case e.leaf && e.class == classDispatchOnly:
			t.Errorf("%s is a leaf declared %q — a leaf has nothing to dispatch "+
				"to, so this is a real runtime class left unwritten",
				e.path, classDispatchOnly)
		}
	}
}

// TestEveryEscalatingFlagExists keeps the escalations honest: the flag a
// declaration names has to be a flag the command really has, so renaming or
// removing `--check` / `--seal` reddens instead of leaving a class conditional
// on nothing.
func TestEveryEscalatingFlagExists(t *testing.T) {
	isolateConfig(t)
	root := NewRootCmd(testBuildInfo())

	checked := 0
	for _, cmd := range walkTree(root) {
		esc := parseEscalations(cmd.Annotations[runtimeEscalationsAnnotation])
		for flag, class := range esc {
			checked++
			if cmd.Flags().Lookup(flag) == nil {
				t.Errorf("%s escalates on --%s, which it does not define",
					cmd.CommandPath(), flag)
			}
			if !runtimeClasses[class] {
				t.Errorf("%s escalates --%s to %q, which is not a known class",
					cmd.CommandPath(), flag, class)
			}
			if class == cmd.Annotations[runtimeClassAnnotation] {
				t.Errorf("%s escalates --%s to its own base class %q — an "+
					"escalation that escalates nothing", cmd.CommandPath(), flag, class)
			}
		}
	}
	// Anchor: this loop over zero escalations would read as a pass. Two are
	// declared today (`auth status --check`, `client status --seal`); if a future
	// change legitimately removes both, lower this WITH the golden diff that
	// shows it.
	if checked < 2 {
		t.Fatalf("only %d escalations were checked — the annotation stopped being read", checked)
	}
}

// --- the derived half of the classification ----------------------------------

// TestClusterClassAgreesWithTheKubeconfigFlag is the cross-check that makes the
// cluster axis measured rather than asserted. Every command that reaches a
// cluster is reached through internal/cluster's kubeconfig resolution, and every
// one of them exposes --kubeconfig to point it somewhere; a command that needs
// no cluster has no use for the flag. So the flag surface and the class have to
// agree, and a class edited in either direction without the code moving with it
// fails here.
func TestClusterClassAgreesWithTheKubeconfigFlag(t *testing.T) {
	isolateConfig(t)
	root := NewRootCmd(testBuildInfo())

	withFlag, withClass := 0, 0
	for _, cmd := range walkTree(root) {
		e := surfaceEntry{
			path:        cmd.CommandPath(),
			class:       cmd.Annotations[runtimeClassAnnotation],
			escalations: parseEscalations(cmd.Annotations[runtimeEscalationsAnnotation]),
		}
		hasKubeconfig := cmd.Flags().Lookup("kubeconfig") != nil
		if hasKubeconfig {
			withFlag++
		}
		if e.needsCluster() {
			withClass++
		}
		switch {
		case e.needsCluster() && !hasKubeconfig:
			t.Errorf("%s is classed %q (needs a cluster) but takes no --kubeconfig — "+
				"either the class is wrong or the command lost the flag", e.path, e.class)
		case !e.needsCluster() && hasKubeconfig:
			t.Errorf("%s takes --kubeconfig but is classed %q (no cluster) — "+
				"the class is understating what this command needs", e.path, e.class)
		}
	}
	// Anchor: a tree whose flags stopped being registered would agree with a tree
	// whose classes all said "no cluster", and both loops above would pass over
	// nothing. Eleven commands take --kubeconfig today.
	if withFlag < 5 || withClass < 5 {
		t.Fatalf("only %d commands take --kubeconfig and %d are classed as needing a "+
			"cluster — the flag surface was not built, so this check compared nothing",
			withFlag, withClass)
	}
}

// --- the golden: the whole surface, generated -------------------------------

// TestCommandSurfaceGolden pins the surface itself. The file is rendered from
// the walk above — including the counts, so the totals that were wrong in prose
// are now a generated, reviewed artifact — and any add / hide / rename /
// re-parent shows up as a diff a reviewer has to look at.
func TestCommandSurfaceGolden(t *testing.T) {
	got := renderSurface(derivedSurface(t))

	if os.Getenv("TB_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(surfaceGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(surfaceGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", surfaceGolden)
		return
	}

	want, err := os.ReadFile(surfaceGolden)
	if err != nil {
		t.Fatalf("read %s: %v\nRegenerate: TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestCommandSurfaceGolden",
			surfaceGolden, err)
	}
	if got != string(want) {
		t.Errorf("the CLI command surface changed.\n\n%s\n\n"+
			"If the change is intended, regenerate and review the diff:\n"+
			"  TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestCommandSurfaceGolden\n"+
			"Then check RFC-BACKEND-2363 D2's class table and the coverage sweep "+
			"still describe this surface.", firstDiff(string(want), got))
	}
}

// renderSurface writes the surface as text: one line per command, then the
// derived tallies. Stable, sorted, and diffable — a reviewer reads the diff, not
// the file.
func renderSurface(entries []surfaceEntry) string {
	var b strings.Builder
	b.WriteString("# The tracebloc CLI command surface — GENERATED, do not edit by hand.\n")
	b.WriteString("# Derived from the live cobra tree (NewRootCmd) by TestCommandSurfaceGolden.\n")
	b.WriteString("# Regenerate: TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestCommandSurfaceGolden\n")
	b.WriteString("#\n")
	b.WriteString("# Classes (RFC-BACKEND-2363 D2): a = binary only | a-prime = local host + GitHub |\n")
	b.WriteString("# b = backend + auth | b+c = backend and cluster | c = cluster only |\n")
	b.WriteString("# group = dispatch node (bare, it only prints help).\n")
	b.WriteString("#\n")
	b.WriteString("# class    shape  visibility  path   (aliases)   [--flag escalations]\n")

	byClass := map[string]int{}
	leaves, visibleLeaves, invocable := 0, 0, 0
	for _, e := range entries {
		shape := "group"
		if e.leaf {
			shape = "leaf"
			leaves++
			if !e.hidden {
				visibleLeaves++
			}
		}
		visibility := "visible"
		if e.hidden {
			visibility = "hidden"
		}
		byClass[e.class]++
		if e.invocable() {
			invocable++
		}
		fmt.Fprintf(&b, "%-8s %-6s %-11s %s", e.class, shape, visibility, e.path)
		if len(e.aliases) > 0 {
			// Aliases are invocation paths users' scripts hold: `data ingest` also
			// answers to `dataset push`. Dropping one is a breaking change, so the
			// surface records them and a silent removal shows up in this diff.
			aliases := append([]string(nil), e.aliases...)
			sort.Strings(aliases)
			fmt.Fprintf(&b, "   (%s)", strings.Join(aliases, " "))
		}
		if len(e.escalations) > 0 {
			flags := make([]string, 0, len(e.escalations))
			for f, c := range e.escalations {
				flags = append(flags, fmt.Sprintf("--%s=%s", f, c))
			}
			sort.Strings(flags)
			fmt.Fprintf(&b, "   [%s]", strings.Join(flags, " "))
		}
		b.WriteString("\n")
	}

	// The counts, derived. These are the numbers a plan or an RFC quotes, so they
	// are generated here and reviewed in this diff rather than restated in prose.
	// "invocable" counts every leaf plus the parents that do work when run bare
	// (today: `tracebloc` renders the home screen, `tracebloc resources` shows).
	b.WriteString("#\n# totals (derived)\n")
	fmt.Fprintf(&b, "# commands=%d leaves=%d visible-leaves=%d invocable=%d\n",
		len(entries), leaves, visibleLeaves, invocable)
	classes := make([]string, 0, len(byClass))
	for c := range byClass {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	parts := make([]string, 0, len(classes))
	for _, c := range classes {
		parts = append(parts, fmt.Sprintf("%s=%d", c, byClass[c]))
	}
	fmt.Fprintf(&b, "# by class: %s\n", strings.Join(parts, " "))
	return b.String()
}

// firstDiff renders the first differing line of the golden comparison, so the
// failure names the command that moved instead of dumping both files.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		lw, lg := "<end of file>", "<end of file>"
		if i < len(w) {
			lw = w[i]
		}
		if i < len(g) {
			lg = g[i]
		}
		if lw != lg {
			return fmt.Sprintf("first difference at line %d:\n  golden: %s\n  live:   %s", i+1, lw, lg)
		}
	}
	return "(files differ only in trailing content)"
}
