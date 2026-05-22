package push

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
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
	stdin io.Reader, _ io.Writer, stderr io.Writer,
) error {
	f.gotNS, f.gotPod, f.gotContainer = ns, pod, container
	f.gotCmd = cmd

	if stdin != nil && (f.drainBeforeReturn || f.errToReturn == nil) {
		b, _ := io.ReadAll(stdin)
		f.gotStdin = b
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
		{name: "labels.csv", size: 39}, // "image_id,label\n001.jpg,cat\n002.jpg,dog\n"
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
	// rm -rf is the hermetic-re-push guard (Bugbot PR-b round 5):
	// without it, a smaller second push leaves stale images on the
	// PVC that disagree with the new labels.csv. Pin its presence
	// AND that it targets the per-table subdir (not /data/shared
	// itself, which would nuke sibling tables) — ValidateTableName
	// is the security boundary that keeps the dest single-segment.
	for _, want := range []string{
		`rm -rf "/data/shared/my_table"`,
		`mkdir -p "/data/shared/my_table"`,
		`tar -xf -`,
		`-C "/data/shared/my_table"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("remote script missing %q: %s", want, script)
		}
	}
	// Defense-in-depth: the rm MUST appear BEFORE the mkdir+tar.
	// A future refactor that reorders these would silently break
	// hermetic re-push without this assertion.
	rmIdx := strings.Index(script, "rm -rf")
	mkdirIdx := strings.Index(script, "mkdir -p")
	if rmIdx >= mkdirIdx {
		t.Errorf("remote script has `rm -rf` after `mkdir -p` (order broken — re-push won't be hermetic):\n%s", script)
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
