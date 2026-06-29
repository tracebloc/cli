package cli

import (
	"bytes"
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
		[]byte("image_id,label\n001.jpg,cat\n"), 0o644); err != nil {
		t.Fatalf("write labels.csv: %v", err)
	}
	imagesDir := filepath.Join(root, "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatalf("mkdir images: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imagesDir, "001.jpg"),
		make([]byte, 100), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
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
		"semantic_segmentation",     // blocked on the ingestor (data-ingestors#136)
		"instance_segmentation",     // not implemented
		"definitely-not-a-category", // nonsense; gate catches this too
	} {
		t.Run(badCategory, func(t *testing.T) {
			code, _, _ := execDataIngest(t, []string{
				root,
				"--table=t1",
				"--category=" + badCategory,
				"--intent=train",
				"--label-column=label",
			})
			if code != 2 {
				t.Fatalf("expected exit 2 for unsupported category %q, got %d", badCategory, code)
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
		root, "--table=t1", "--category=causal_language_modeling",
		"--intent=train", "--label-column=label"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected an error for a known-but-unsupported category")
	}
	if got := ExitCodeFromError(err); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
	msg := err.Error()
	if strings.Contains(msg, "isn't a recognized task category") {
		t.Errorf("known category misrouted to the unrecognized-category branch:\n%s", msg)
	}
	if !strings.Contains(msg, "isn't supported by the CLI yet") {
		t.Errorf("want the registry pending-support note, got:\n%s", msg)
	}
}

// TestDataIngest_TraversalTableName_ExitsTwo is the security
// regression pin at the CLI layer. --table=../../etc must be
// rejected with exit 2 BEFORE any spec synthesis or cluster work —
// the table name flows into the /data/shared/<table>/ PVC path,
// and a traversal value would let PR-b's stage Pod escape that
// subtree. Bugbot flagged this on PR #8 commit 4240097.
func TestDataIngest_TraversalTableName_ExitsTwo(t *testing.T) {
	root := imgcLayout(t)
	for _, bad := range []string{"../../etc", "../foo", "foo/bar"} {
		t.Run(bad, func(t *testing.T) {
			code, _, _ := execDataIngest(t, []string{
				root,
				"--table=" + bad,
				"--category=image_classification",
				"--intent=train",
				"--label-column=label",
			})
			if code != 2 {
				t.Fatalf("expected exit 2 for traversal table name %q, got %d", bad, code)
			}
		})
	}
}

// TestDataIngest_MissingIntent_ExitsTwo: pins the "intent is
// required" diagnostic path — different schema violation but the
// same exit-code class.
func TestDataIngest_MissingIntent_ExitsTwo(t *testing.T) {
	root := imgcLayout(t)
	code, _, stderr := execDataIngest(t, []string{
		root,
		"--table=t1",
		"--category=image_classification",
		// intent omitted
		"--label-column=label",
	})
	if code != 2 {
		t.Fatalf("expected exit 2 for missing intent, got %d", code)
	}
	if !strings.Contains(stderr, "intent") {
		t.Errorf("expected stderr to mention 'intent', got:\n%s", stderr)
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
		"--table=t1",
		"--category=image_classification",
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
		"--table=t1",
		"--category=image_classification",
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
		"--table=t1",
		"--category=image_classification",
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
				"--table=t1", "--category=image_classification",
				"--intent=train", "--label-column=label",
			},
		},
		{
			name: "two positionals",
			args: []string{
				"./a", "./b",
				"--table=t1", "--category=image_classification",
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
