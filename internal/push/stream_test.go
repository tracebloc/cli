package push

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// fakeExecutor is the stand-in for SPDYExecutor in tests. Captures
// every Exec call's parameters into fields the test can assert on,
// and reads the stdin stream into a buffer (so we can parse the tar
// archive back out and verify its contents).
type fakeExecutor struct {
	// Captured call parameters.
	gotNS, gotPod, gotContainer string
	gotCmd                      []string

	// Captured stdin (the tar archive).
	gotStdin []byte

	// What to write back to the caller as stderr (simulates a
	// "tar: no space left on device" style remote-side failure).
	stderrToReturn []byte

	// What to write back to the caller as stdout (e.g. the mysql query
	// result ListDatasets parses). Nil for the tar-stream callers.
	stdoutToReturn []byte

	// What to return as the exec error (simulates a 4xx/5xx from
	// the apiserver, a network drop, etc.).
	errToReturn error

	// drainBeforeReturn: if false, errToReturn fires BEFORE the
	// stdin pipe is drained — simulates a fast failure where the
	// remote tar dies on the first byte. Test infrastructure for
	// broken-pipe coverage.
	drainBeforeReturn bool
}

func (f *fakeExecutor) Exec(
	_ context.Context,
	ns, pod, container string,
	cmd []string,
	stdin io.Reader, stdout, stderr io.Writer,
) error {
	f.gotNS, f.gotPod, f.gotContainer = ns, pod, container
	f.gotCmd = cmd

	if stdin != nil && (f.drainBeforeReturn || f.errToReturn == nil) {
		b, _ := io.ReadAll(stdin)
		f.gotStdin = b
	}
	if len(f.stdoutToReturn) > 0 && stdout != nil {
		_, _ = stdout.Write(f.stdoutToReturn)
	}
	if len(f.stderrToReturn) > 0 && stderr != nil {
		_, _ = stderr.Write(f.stderrToReturn)
	}
	return f.errToReturn
}

// TestStreamLayout_TarPathsAreForwardSlash pins the cross-platform
// tar header convention. On Windows, filepath.Join uses '\' as
// separator — using it for the tar HEADER name would produce
// archive entries the Linux stage Pod's `tar -xf` either rejects
// or extracts as flat-named files (collapsing the images/ subdir).
// The fix is path.Join (always forward-slash); this test asserts
// the portable property regardless of which OS the test runs on.
// Bugbot flagged the Windows bug on PR-b round 6.
func TestStreamLayout_TarPathsAreForwardSlash(t *testing.T) {
	root := imgcDir(t, "a.jpg", "b.png")
	layout, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	fe := &fakeExecutor{}
	if err := StreamLayout(context.Background(), fe, "tracebloc", "p", "stage",
		layout, "t", NoOpProgress{}); err != nil {
		t.Fatalf("StreamLayout: %v", err)
	}

	tr := tar.NewReader(bytes.NewReader(fe.gotStdin))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		if strings.Contains(hdr.Name, `\`) {
			t.Errorf("tar entry %q contains backslash; "+
				"Linux stage Pod's `tar -xf` requires forward-slash paths", hdr.Name)
		}
	}
}

// TestStreamLayout_TarContents end-to-end: the bytes the executor
// sees on stdin MUST be a valid tar with the layout's files at the
// expected paths (labels.csv at the root, images/<basename> under
// images/). If this drifts, the in-cluster `tar -xf - -C
// /data/shared/<table>/` lands files in the wrong place and
// jobs-manager's ingestor in Phase 4 silently sees zero rows.
func TestStreamLayout_TarContents(t *testing.T) {
	root := imgcDir(t, "a.jpg", "b.png")
	layout, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	fe := &fakeExecutor{}
	err = StreamLayout(context.Background(), fe, "tracebloc", "pod-x", "stage",
		layout, "cats_dogs", NoOpProgress{})
	if err != nil {
		t.Fatalf("StreamLayout: %v", err)
	}

	// Parse the captured tar back out and collect (name, size).
	type entry struct {
		name string
		size int64
	}
	var got []entry
	tr := tar.NewReader(bytes.NewReader(fe.gotStdin))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		got = append(got, entry{name: hdr.Name, size: hdr.Size})
	}

	// Sort for deterministic comparison — the producer walks
	// images/ in OS order which isn't stable across filesystems.
	sort.Slice(got, func(i, j int) bool { return got[i].name < got[j].name })

	// Three entries: labels.csv + 2 images.
	if len(got) != 3 {
		t.Fatalf("tar entry count = %d, want 3 (labels.csv + 2 images); got=%v", len(got), got)
	}
	want := []entry{
		{name: "images/a.jpg", size: 100}, // imgcDir writes 100-byte images
		{name: "images/b.png", size: 100},
		{name: "labels.csv", size: 39}, // "filename,label\n001.jpg,cat\n002.jpg,dog\n"
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestStreamLayout_RemoteCommand pins the in-cluster command. If
// this ever drifts (e.g. someone changes the destination path
// helper), the tar gets extracted to the wrong PVC subdirectory
// and Phase 4 silently fails. The mkdir -p is what makes re-push
// overwrite semantics work.
func TestStreamLayout_RemoteCommand(t *testing.T) {
	layout, err := Discover(imgcDir(t))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	fe := &fakeExecutor{}
	_ = StreamLayout(context.Background(), fe, "tracebloc", "pod-x", "stage",
		layout, "my_table", NoOpProgress{})

	if got, want := len(fe.gotCmd), 3; got != want {
		t.Fatalf("len(cmd) = %d, want %d", got, want)
	}
	if fe.gotCmd[0] != "/bin/sh" || fe.gotCmd[1] != "-c" {
		t.Errorf("cmd[:2] = %v, want [/bin/sh -c]", fe.gotCmd[:2])
	}
	script := fe.gotCmd[2]
	// The remote command must be:
	//   1. HERMETIC (Bugbot r5): old files don't survive a re-push
	//   2. TRANSACTIONAL (Bugbot r7): tar failure preserves prev
	//      dataset
	//   3. RACE-SAFE under parallel-push (Bugbot r8): unique
	//      .staging-<hex> suffix per invocation so concurrent pushes
	//      don't interleave each other's tar extraction
	//
	// Pattern: extract to <dest>.staging-<hex>, then on tar SUCCESS
	// rm the old dest + mv staging → dest. The order matters:
	//
	//   - tar BEFORE any rm of $DEST  (preserves on tar failure)
	//   - mv AFTER tar succeeds       (atomic-ish swap)
	//
	// `dest` is StagedPrefix(table) — the CLI's SOURCE staging dir,
	// which (since #26) lives under SharedRoot/.tracebloc-staging/ so
	// it never collides with the ingestor's DEST_PATH. Derive the
	// expected paths from it so this test tracks StagedPrefix rather
	// than hardcoding the prefix.
	dest := StagedPrefix("my_table")

	// Extract the staging path with a regex so the random hex
	// suffix doesn't pin us to a specific invocation's bytes.
	stagingRE := regexp.MustCompile(regexp.QuoteMeta(dest) + `\.staging-[0-9a-f]{8}`)
	stagingPaths := stagingRE.FindAllString(script, -1)
	if len(stagingPaths) == 0 {
		t.Fatalf("remote script has no %s.staging-<8hex> path (race-safety regression):\n%s", dest, script)
	}
	// All staging mentions must refer to the SAME suffix in a single
	// invocation. If we see two distinct suffixes that's a bug:
	// rm/mkdir/tar/mv would target different dirs.
	for i := 1; i < len(stagingPaths); i++ {
		if stagingPaths[i] != stagingPaths[0] {
			t.Errorf("remote script mixes staging suffixes %q and %q:\n%s",
				stagingPaths[0], stagingPaths[i], script)
		}
	}
	staging := stagingPaths[0]

	// Pin the four key operations.
	for _, want := range []string{
		`mkdir -p "` + staging + `"`,
		`tar -xf - -C "` + staging + `"`,
		`mv "` + staging + `" "` + dest + `"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("remote script missing %q: %s", want, script)
		}
	}
	// Transactional property: tar must come BEFORE any mutation
	// of the real destination. r5+r7 used `rm $DEST` for the
	// pre-mv step; r10 swapped that to `mv $DEST $DEST.old-<hex>`
	// (backup-and-swap, so a mv failure can be rolled back).
	// The contract is the same: tar runs while $DEST is intact.
	tarIdx := strings.Index(script, `tar -xf - -C "`+staging+`"`)
	destBackupMvIdx := strings.Index(script, `mv "`+dest+`" "`)
	if tarIdx < 0 || destBackupMvIdx < 0 {
		t.Fatalf("remote script missing tar or destination-backup mv: %s", script)
	}
	if tarIdx >= destBackupMvIdx {
		t.Errorf("remote script touches $DEST BEFORE tar succeeds — partial-transfer could destroy previous data:\n%s", script)
	}
	// Single-segment guarantee: rm targets must never be the shared
	// root or the staging parent themselves (that would nuke sibling
	// tables / every in-flight push).
	for _, forbidden := range []string{
		`rm -rf "/data/shared"`,
		`rm -rf "/data/shared/"`,
		`rm -rf "` + SharedRoot + "/" + stagingDirName + `"`,
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("remote script contains dangerous %q (would nuke sibling tables/pushes):\n%s", forbidden, script)
		}
	}

	// Bugbot r9 + r10: orphan cleanup for previously-failed pushes
	// must catch BOTH .staging-* and .old-* siblings. The .old-*
	// dirs are created by the round-10 backup-and-rollback swap;
	// without including them in the find pattern, failed-mid-swap
	// pushes would leak .old-* dirs.
	for _, wantName := range []string{`-name "my_table.staging-*"`, `-name "my_table.old-*"`} {
		if !strings.Contains(script, wantName) {
			t.Errorf("remote script missing %s in orphan cleanup:\n%s", wantName, script)
		}
	}
	if !strings.Contains(script, `-mmin +60 -exec rm -rf {} +`) {
		t.Errorf("remote script missing -mmin+60 orphan window:\n%s", script)
	}

	// Bugbot r10: backup-and-swap pattern. The previous dataset
	// must be backed up to .old-<hex> BEFORE the new dataset
	// arrives, and restored if the main mv fails. Pin the key
	// shape pieces — backup mv, primary mv, rollback mv, cleanup.
	backupRE := regexp.MustCompile(regexp.QuoteMeta(dest) + `\.old-[0-9a-f]{8}`)
	backupPaths := backupRE.FindAllString(script, -1)
	if len(backupPaths) == 0 {
		t.Fatalf("remote script has no .old-<hex> backup path (r10 rollback regression):\n%s", script)
	}
	// All .old-<hex> mentions in a single invocation must agree —
	// otherwise the rollback would target a different path than
	// the backup created.
	for i := 1; i < len(backupPaths); i++ {
		if backupPaths[i] != backupPaths[0] {
			t.Errorf("remote script mixes backup suffixes %q vs %q:\n%s",
				backupPaths[0], backupPaths[i], script)
		}
	}
	backup := backupPaths[0]
	// Backup must use the SAME suffix as staging — different
	// suffixes would defeat the "find -name ...staging-* ...old-*"
	// orphan-cleanup symmetry, AND would risk collision with a
	// concurrent push's .old-<hex>.
	if strings.TrimPrefix(backup, dest+".old-") !=
		strings.TrimPrefix(staging, dest+".staging-") {
		t.Errorf("backup and staging suffixes diverge: %q vs %q", backup, staging)
	}
	// Backup mv (DEST → .old) must appear BEFORE primary mv
	// (.staging → DEST), or rollback wouldn't have anything to
	// restore.
	backupMvIdx := strings.Index(script, `mv "`+dest+`" "`+backup+`"`)
	primaryMvIdx := strings.Index(script, `mv "`+staging+`" "`+dest+`"`)
	if backupMvIdx < 0 || primaryMvIdx < 0 {
		t.Fatalf("remote script missing backup or primary mv:\n%s", script)
	}
	if backupMvIdx >= primaryMvIdx {
		t.Errorf("backup mv (DEST→.old) must come BEFORE primary mv (.staging→DEST); rollback contract broken:\n%s", script)
	}
}

// TestStreamLayout_StagingSuffixIsUniquePerInvocation: two back-to-
// back StreamLayout calls for the same table MUST produce different
// staging-dir suffixes. Without this, concurrent `dataset push` runs
// for the same table would race on the same .staging path and
// corrupt each other's tar extraction. Bugbot r8 fix.
func TestStreamLayout_StagingSuffixIsUniquePerInvocation(t *testing.T) {
	layout, err := Discover(imgcDir(t))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	stagingRE := regexp.MustCompile(regexp.QuoteMeta(StagedPrefix("t")) + `\.staging-[0-9a-f]{8}`)

	collect := func() string {
		fe := &fakeExecutor{}
		if err := StreamLayout(context.Background(), fe, "tracebloc", "p", "stage",
			layout, "t", NoOpProgress{}); err != nil {
			t.Fatalf("StreamLayout: %v", err)
		}
		m := stagingRE.FindString(fe.gotCmd[2])
		if m == "" {
			t.Fatalf("no staging path in: %s", fe.gotCmd[2])
		}
		return m
	}
	a, b := collect(), collect()
	if a == b {
		t.Errorf("back-to-back StreamLayout produced identical staging path %q; race-safety regression", a)
	}
}

// TestStreamLayout_TargetingFromArgs pins that namespace + pod name
// flow through to the executor unchanged. Catches a refactor that
// hardcodes the namespace or builds the pod name in the wrong layer.
func TestStreamLayout_TargetingFromArgs(t *testing.T) {
	layout, err := Discover(imgcDir(t))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	fe := &fakeExecutor{}
	_ = StreamLayout(context.Background(), fe, "custom-ns", "weird-pod-name", "container-x",
		layout, "t", NoOpProgress{})
	if fe.gotNS != "custom-ns" {
		t.Errorf("namespace = %q, want custom-ns", fe.gotNS)
	}
	if fe.gotPod != "weird-pod-name" {
		t.Errorf("pod = %q, want weird-pod-name", fe.gotPod)
	}
	if fe.gotContainer != "container-x" {
		t.Errorf("container = %q, want container-x", fe.gotContainer)
	}
}

// TestStreamLayout_ExecErrorWithRemoteStderr: when the in-cluster
// tar fails (e.g. read-only FS, no-space), the remote stderr must
// surface in the customer-facing error. This is the actionable
// signal — without it, customers see "exec stream failed" with no
// way to diagnose what went wrong server-side.
func TestStreamLayout_ExecErrorWithRemoteStderr(t *testing.T) {
	layout, err := Discover(imgcDir(t))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	fe := &fakeExecutor{
		stderrToReturn:    []byte("tar: write error: No space left on device"),
		errToReturn:       errors.New("command terminated with exit code 1"),
		drainBeforeReturn: true,
	}
	err = StreamLayout(context.Background(), fe, "tracebloc", "p", "stage",
		layout, "t", NoOpProgress{})
	if err == nil {
		t.Fatal("StreamLayout returned nil on exec error")
	}
	for _, want := range []string{"streaming files", "No space left on device", "exit code 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestStreamLayout_ProgressBytes: the Progress sink receives byte
// counts during the stream. This is the contract the schollz bar
// keys off — if the wrapper ever stops calling Add(), the bar would
// freeze at 0%.
func TestStreamLayout_ProgressBytes(t *testing.T) {
	layout, err := Discover(imgcDir(t))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	fp := &fakeProgress{}
	err = StreamLayout(context.Background(), &fakeExecutor{}, "tracebloc", "p", "stage",
		layout, "t", fp)
	if err != nil {
		t.Fatalf("StreamLayout: %v", err)
	}
	// At minimum we expect the file bodies (200 + 39 = 239) plus
	// tar headers (~512 per file = ~1536 bytes overhead minimum).
	// Assert "some bytes flowed" rather than an exact count
	// because the tar overhead is implementation-defined.
	if fp.added < 239 {
		t.Errorf("progress.Add total = %d, want at least 239 (file body bytes)", fp.added)
	}
	if !fp.finished {
		t.Errorf("progress.Finish() never called")
	}
}

// TestWriteLayoutTar_LabelsCSVFirst: ordering matters for fail-fast
// diagnostic — labels.csv at the start means a corrupt tar is more
// likely to surface before we've shipped a hundred image bytes.
func TestWriteLayoutTar_LabelsCSVFirst(t *testing.T) {
	layout, err := Discover(imgcDir(t))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var buf bytes.Buffer
	if err := writeLayoutTar(&buf, layout); err != nil {
		t.Fatalf("writeLayoutTar: %v", err)
	}
	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("reading first entry: %v", err)
	}
	if hdr.Name != "labels.csv" {
		t.Errorf("first entry name = %q, want labels.csv (ordering pins this)", hdr.Name)
	}
}

// fakeProgress: a Progress that records total bytes added and
// whether Finish was called.
type fakeProgress struct {
	added    int
	finished bool
}

func (p *fakeProgress) Add(n int) { p.added += n }
func (p *fakeProgress) Finish()   { p.finished = true }
