package cli

import (
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/ui"
	"image"
	"image/png"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"bytes"
	"context"
	"errors"
	"github.com/tracebloc/cli/internal/cluster"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// imgcLayout drops a minimum-viable image_classification directory
// under t.TempDir() and returns its path. Mirrors push.imgcDir
// (tests can't import test helpers across packages, so we duplicate
// the few lines).
func imgcLayout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "labels.csv"),
		[]byte("image_id,label\n001.jpg,cat\n002.jpg,dog\n"), 0o644); err != nil {
		t.Fatalf("write labels.csv: %v", err)
	}
	imagesDir := filepath.Join(root, "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatalf("mkdir images: %v", err)
	}
	// Real decodable images with two classes: the P3 preflight decodes
	// EVERY image and requires >=2 label classes, so opaque stubs would
	// short-circuit any test that needs to get past preflight (a
	// kubeconfig test failed vacuously on exactly that).
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001.jpg", "002.jpg"} {
		if err := os.WriteFile(filepath.Join(imagesDir, name), buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

// execDataIngest drives the full cobra dispatch for the ingest
// command using the canonical `data ingest` form and returns the
// exit code + captured stdout/stderr.
// Mirrors the execIngestValidate helper from ingest_test.go — same
// rationale about not sharing *cobra.Command across cases (cobra
// holds flag state on the command tree).
//
// kubeconfigPath is required because every ingest invocation tries
// kubeconfig load before any cluster work; tests that want to
// stop EARLIER (at schema validation or layout walk) still need a
// kubeconfig path that resolves predictably. We feed in a path
// that's guaranteed to fail os.Stat so the kubeconfig branch
// errors out consistently when reached — and tests assert on the
// EARLIER stage's exit code, which fires before kubeconfig.
func execDataIngest(t *testing.T, args []string) (exitCode int, stdout, stderr string) {
	t.Helper()
	root := NewRootCmd(BuildInfo{Version: "test"})
	var so, se bytes.Buffer
	root.SetOut(&so)
	root.SetErr(&se)

	// Always inject a guaranteed-bad kubeconfig path so tests that
	// "fall through" the local pre-checks into kubeconfig load
	// get a deterministic exit 3 (not a flaky "depends on whether
	// you have a real kubeconfig" outcome).
	cmdArgs := append([]string{"data", "ingest",
		"--kubeconfig=/tmp/tracebloc-cli-test-nonexistent-" + t.Name()},
		args...)
	root.SetArgs(cmdArgs)

	err := root.Execute()
	return ExitCodeFromError(err), so.String(), se.String()
}

// TestDataIngest_UnsupportedCategory_ExitsTwo: the CLI-side category
// gate runs before schema validation so a customer who passes a
// not-yet-supported category gets an actionable message (exit 2)
// rather than the schema's confusing missing-property error. Today's
// supported set is image_classification + the tabular / time-series
// family; the other image categories (which need annotation/mask
// sidecar staging), the text family, and nonsense values are gated
// out here. Bugbot review-on-self caught the missing gate on PR-a.
func TestDataIngest_UnsupportedCategory_ExitsTwo(t *testing.T) {
	root := imgcLayout(t)
	for _, badCategory := range []string{
		"semantic_segmentation",     // known but blocked on the ingestor (data-ingestors#136)
		"instance_segmentation",     // dead — removed from the registry (#1005), now unrecognized
		"definitely-not-a-category", // nonsense; gate catches this too
	} {
		t.Run(badCategory, func(t *testing.T) {
			code, _, _ := execDataIngest(t, []string{
				root,
				"--name=t1",
				"--task=" + badCategory,
				"--intent=train",
				"--label-column=label",
			})
			if code != 2 {
				t.Fatalf("expected exit 2 for unsupported task %q, got %d", badCategory, code)
			}
		})
	}
}

// TestDataIngest_KnownUnsupportedCategory_PendingNote pins the Bugbot fix
// (v0.4.0 RC): a registry-known but CLI-unsupported NON-image category
// (causal_language_modeling) must get the registry's pending-support note, not
// the misleading "isn't a recognized task category" message. execDataIngest
// discards the error and SilenceErrors swallows it, so run the command here and
// inspect the returned error directly.
func TestDataIngest_KnownUnsupportedCategory_PendingNote(t *testing.T) {
	root := imgcLayout(t)
	rootCmd := NewRootCmd(BuildInfo{Version: "test"})
	rootCmd.SetOut(&bytes.Buffer{})
	rootCmd.SetErr(&bytes.Buffer{})
	rootCmd.SetArgs([]string{"data", "ingest",
		"--kubeconfig=/tmp/tracebloc-cli-test-nonexistent-" + t.Name(),
		root, "--name=t1", "--task=causal_language_modeling",
		"--intent=train", "--label-column=label"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected an error for a known-but-unsupported task")
	}
	if got := ExitCodeFromError(err); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
	msg := err.Error()
	if strings.Contains(msg, "isn't a recognized task") {
		t.Errorf("known task misrouted to the unrecognized-task branch:\n%s", msg)
	}
	if !strings.Contains(msg, "isn't supported by the CLI yet") {
		t.Errorf("want the registry pending-support note, got:\n%s", msg)
	}
}

// TestDataIngest_TraversalTableName_ExitsTwo is the security
// regression pin at the CLI layer. --name=../../etc must be
// rejected with exit 2 BEFORE any spec synthesis or cluster work —
// the name flows into the /data/shared/<table>/ PVC path,
// and a traversal value would let PR-b's stage Pod escape that
// subtree. Bugbot flagged this on PR #8 commit 4240097.
func TestDataIngest_TraversalTableName_ExitsTwo(t *testing.T) {
	root := imgcLayout(t)
	for _, bad := range []string{"../../etc", "../foo", "foo/bar"} {
		t.Run(bad, func(t *testing.T) {
			code, _, _ := execDataIngest(t, []string{
				root,
				"--name=" + bad,
				"--task=image_classification",
				"--intent=train",
				"--label-column=label",
			})
			if code != 2 {
				t.Fatalf("expected exit 2 for traversal table name %q, got %d", bad, code)
			}
		})
	}
}

// TestDataIngest_OmittedIntent_DefaultsToTrain: --intent defaults to
// "train", so omitting it no longer fails schema validation (exit 2).
// The run gets past the spec checks and stops at the injected bad
// kubeconfig (exit 3) — the same fall-through point as
// TestDataIngest_BadKubeconfig_ExitsThree, which proves the default was
// applied rather than the value being rejected as missing.
func TestDataIngest_OmittedIntent_DefaultsToTrain(t *testing.T) {
	root := imgcLayout(t)
	code, _, _ := execDataIngest(t, []string{
		root,
		"--name=t1",
		"--task=image_classification",
		// intent omitted → defaults to train
		"--label-column=label",
	})
	if code != 3 {
		t.Fatalf("expected exit 3 (default intent applied, then bad kubeconfig), got %d", code)
	}
}

// TestDataIngest_NonexistentLocalPath_ExitsThree: the layout walk
// runs AFTER schema validation, so an invalid local path with
// otherwise-valid flags surfaces at the walk stage with exit 3
// (the "local input or kubeconfig" code).
//
// We only assert on the exit code, not the error text — cobra's
// SilenceErrors=true means root.Execute() doesn't surface the
// returned error to stderr (main.go does that). Mirrors
// ingest_test.go's TestIngestValidate_UnreadableFileExitsThree
// pattern; the error-content surface is exercised at the package
// level (internal/push.Discover's own tests).
func TestDataIngest_NonexistentLocalPath_ExitsThree(t *testing.T) {
	code, _, _ := execDataIngest(t, []string{
		"/tmp/tracebloc-cli-test-no-such-dir-" + t.Name(),
		"--name=t1",
		"--task=image_classification",
		"--intent=train",
		"--label-column=label",
	})
	if code != 3 {
		t.Fatalf("expected exit 3 for missing local path, got %d", code)
	}
}

// TestDataIngest_MissingLabelsCSV_ExitsThree: most likely "real
// world" wrong-layout case — customer has images but forgot
// labels.csv. Pins the exit-code contract for the common failure
// mode; the diagnostic-text content is covered by
// internal/push.TestDiscover_MissingLabelsCSV.
func TestDataIngest_MissingLabelsCSV_ExitsThree(t *testing.T) {
	root := t.TempDir()
	imagesDir := filepath.Join(root, "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imagesDir, "a.jpg"),
		make([]byte, 100), 0o644); err != nil {
		t.Fatalf("write img: %v", err)
	}

	code, _, _ := execDataIngest(t, []string{
		root,
		"--name=t1",
		"--task=image_classification",
		"--intent=train",
		"--label-column=label",
	})
	if code != 3 {
		t.Fatalf("expected exit 3 for missing labels.csv, got %d", code)
	}
}

// TestDataIngest_BadKubeconfig_ExitsThree: schema + layout both
// pass; kubeconfig load fails because the injected path doesn't
// exist. The exit-code contract matches `cluster info`'s — same
// class of failure (3 = local input problem) surfaces with the
// same code regardless of which command tripped it.
func TestDataIngest_BadKubeconfig_ExitsThree(t *testing.T) {
	root := imgcLayout(t)
	code, _, _ := execDataIngest(t, []string{
		root,
		"--name=t1",
		"--task=image_classification",
		"--intent=train",
		"--label-column=label",
	})
	if code != 3 {
		t.Fatalf("expected exit 3 for bad kubeconfig, got %d", code)
	}
}

// TestDataIngest_RequiresExactlyOneArg: cobra-level Args check
// pins the command signature. Two positional args, or zero, should
// fail before the runner even fires.
func TestDataIngest_RequiresExactlyOneArg(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{
			name: "no positional",
			args: []string{
				"--name=t1", "--task=image_classification",
				"--intent=train", "--label-column=label",
			},
		},
		{
			name: "two positionals",
			args: []string{
				"./a", "./b",
				"--name=t1", "--task=image_classification",
				"--intent=train", "--label-column=label",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, _ := execDataIngest(t, c.args)
			if code == 0 {
				t.Errorf("expected non-zero exit for %s, got 0", c.name)
			}
		})
	}
}

// TestDataIngest_DeprecatedFlagAliases pins that the pre-#180 flag names
// still resolve through their hidden aliases so existing scripts don't
// break: --table→--name and --category→--task. A valid
// spec via the old names must fall through the local checks to the
// injected bad kubeconfig (exit 3) exactly as the canonical names do; a
// bad value via --category must still reach the task gate (exit 2),
// proving the aliased value flows through rather than being ignored.
func TestDataIngest_DeprecatedFlagAliases(t *testing.T) {
	root := imgcLayout(t)

	t.Run("valid via old names falls through to kubeconfig", func(t *testing.T) {
		code, _, _ := execDataIngest(t, []string{
			root,
			"--table=t1",
			"--category=image_classification",
			"--intent=train",
			"--label-column=label",
		})
		if code != 3 {
			t.Fatalf("expected exit 3 (aliases resolved, then bad kubeconfig), got %d", code)
		}
	})

	t.Run("bad value via --category reaches the task gate", func(t *testing.T) {
		code, _, _ := execDataIngest(t, []string{
			root,
			"--table=t1",
			"--category=definitely-not-a-task",
			"--intent=train",
			"--label-column=label",
		})
		if code != 2 {
			t.Fatalf("expected exit 2 (aliased task value hit the gate), got %d", code)
		}
	})
}

// TestDataIngest_OmitTask_NonInteractive_Errors: dropping --task's old
// image_classification default means a non-interactive run that omits the
// task no longer silently assumes images. Off a TTY (as in tests) the
// picker can't run, so the task gate returns a clear exit-2 error naming
// --task. execDataIngest discards the error, so run the command directly
// and inspect it (mirrors TestDataIngest_KnownUnsupportedCategory_PendingNote).
func TestDataIngest_OmitTask_NonInteractive_Errors(t *testing.T) {
	root := imgcLayout(t)
	rootCmd := NewRootCmd(BuildInfo{Version: "test"})
	rootCmd.SetOut(&bytes.Buffer{})
	rootCmd.SetErr(&bytes.Buffer{})
	rootCmd.SetArgs([]string{"data", "ingest",
		"--kubeconfig=/tmp/tracebloc-cli-test-nonexistent-" + t.Name(),
		root, "--name=t1", "--intent=train", "--label-column=label"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected an error when --task is omitted non-interactively")
	}
	if got := ExitCodeFromError(err); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
	if !strings.Contains(err.Error(), "--task") {
		t.Errorf("error should tell the user to pass --task, got:\n%s", err.Error())
	}
}

// TestAliasResolution verifies that the deprecated aliases still dispatch
// to the same handlers as the canonical names:
//   - "dataset" → same as "data"
//   - "push"    → same as "ingest"
//   - "rm"      → same as "delete"
//
// We use --help invocations because they complete without cluster access;
// the exit code 0 + non-empty output is sufficient to confirm the alias
// resolved correctly.
func TestAliasResolution(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // substring expected in the combined output
	}{
		{
			name: "dataset alias resolves",
			args: []string{"dataset", "--help"},
			want: "ingest",
		},
		{
			name: "dataset push alias resolves",
			args: []string{"dataset", "push", "--help"},
			want: "Stages a local dataset",
		},
		{
			name: "data ingest canonical",
			args: []string{"data", "ingest", "--help"},
			want: "Stages a local dataset",
		},
		{
			name: "dataset rm alias resolves",
			args: []string{"dataset", "rm", "--help"},
			want: "Removes the in-cluster artifacts",
		},
		{
			name: "data delete canonical",
			args: []string{"data", "delete", "--help"},
			want: "Removes the in-cluster artifacts",
		},
		{
			// `ingest validate` moved under data (top-level `ingest` is a
			// hidden deprecated alias) — both paths must keep resolving.
			name: "data validate canonical",
			args: []string{"data", "validate", "--help"},
			want: "validates it against the bundled",
		},
		{
			name: "ingest validate alias resolves",
			args: []string{"ingest", "validate", "--help"},
			want: "validates it against the bundled",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rootCmd := NewRootCmd(BuildInfo{Version: "test"})
			var out bytes.Buffer
			rootCmd.SetOut(&out)
			rootCmd.SetErr(&out)
			rootCmd.SetArgs(c.args)
			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("Execute() = %v, want nil (help should not error)", err)
			}
			combined := out.String()
			if !strings.Contains(combined, c.want) {
				t.Errorf("output missing %q:\n%s", c.want, combined)
			}
		})
	}
}

// destTableExists backs the cli#70 P4-lite guard: an existing destination
// table must be caught BEFORE staging (a re-ingest used to burn the full
// upload and then fail in-cluster), and a broken check must fail OPEN with
// a visible note — never block the ingest, never pretend it ran.
func TestDestTableExists(t *testing.T) {
	resolved := &cluster.ResolvedConfig{Namespace: "ns"}

	restore := listDatasetsFn
	defer func() { listDatasetsFn = restore }()

	listDatasetsFn = func(_ context.Context, _ kubernetes.Interface, _ *rest.Config, _ string) ([]string, error) {
		return []string{"other", "MyTable"}, nil
	}
	matched, note := destTableExists(context.Background(), nil, resolved, "mytable")
	if matched != "MyTable" || note != "" {
		t.Errorf("case-insensitive match must return the EXISTING spelling (teardown acts on it): matched=%q note=%q, want MyTable/empty", matched, note)
	}

	matched, note = destTableExists(context.Background(), nil, resolved, "fresh_table")
	if matched != "" || note != "" {
		t.Errorf("absent table: matched=%q note=%q, want empty/empty", matched, note)
	}

	listDatasetsFn = func(_ context.Context, _ kubernetes.Interface, _ *rest.Config, _ string) ([]string, error) {
		return nil, errors.New("mysql pod not found")
	}
	matched, note = destTableExists(context.Background(), nil, resolved, "t")
	if matched != "" {
		t.Error("a broken check must fail open (no match), not closed")
	}
	if !strings.Contains(note, "couldn't check") || !strings.Contains(note, "mysql pod not found") {
		t.Errorf("fail-open note = %q, want it to say the check didn't run and why", note)
	}
}

// The images summary line surfaces the detected extension — the visible
// half of the cli#68 fix (the spec half is pinned in internal/push).
func TestPrintLocalSummary_ShowsDetectedExtension(t *testing.T) {
	var buf bytes.Buffer
	p := ui.New(&buf, ui.WithColor(false))
	layout := &push.LocalLayout{Root: "/d", LabelsCSV: "/d/labels.csv", Images: []string{"/d/images/a.png"}}
	spec := map[string]any{
		"table": "t", "category": "image_classification", "intent": "train",
		"spec": map[string]any{"file_options": map[string]any{"extension": ".png"}},
	}
	printLocalSummary(p, layout, spec)
	if !strings.Contains(buf.String(), "1 files (.png)") {
		t.Errorf("summary missing detected extension:\n%s", buf.String())
	}
}
