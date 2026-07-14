package push

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Executor abstracts "exec a command in a Pod with stdin/stdout/
// stderr." The interface exists for one reason: client-go's
// fake.Clientset (the cornerstone of every test in this package)
// doesn't support the exec subresource. Without an interface
// boundary here, Stream's lifecycle couldn't be unit-tested —
// every assertion would need a real cluster.
//
// Production uses SPDYExecutor (this file). Tests construct a
// FakeExecutor (stream_test.go) that captures stdin into a buffer
// + records the command + can return a synthetic error.
type Executor interface {
	// Exec runs cmd in the named container of ns/pod, wiring
	// stdin/stdout/stderr to the provided streams. Returns when
	// the remote command terminates or the context is cancelled.
	//
	// stdin, stdout, stderr may be nil — Exec must tolerate that
	// (a stage Pod with no stderr to capture is a legitimate
	// case for the "just delete me" command after the tar is
	// done).
	Exec(ctx context.Context, ns, pod, container string,
		cmd []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// SPDYExecutor is the production Executor. Wraps client-go's
// remotecommand.NewSPDYExecutor — the same machinery `kubectl exec`
// uses internally.
//
// Holds both Config (needed by NewSPDYExecutor for auth/transport)
// and Client (for building the exec URL via the REST client). Both
// come from cluster.Load + cluster.NewClientset.
type SPDYExecutor struct {
	Config *rest.Config
	Client kubernetes.Interface
}

func (e *SPDYExecutor) Exec(
	ctx context.Context,
	ns, pod, container string,
	cmd []string,
	stdin io.Reader, stdout, stderr io.Writer,
) error {
	// Build the exec URL the way kubectl does — POST against the
	// /pods/<name>/exec subresource, parametrized via PodExecOptions
	// encoded through the scheme's ParameterCodec.
	req := e.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			// Toggle the streams that the caller actually wants.
			// PodExecOptions ignores the unused ones, but being
			// explicit here matches kubectl's behavior and means
			// "stdin requested but caller passed nil" surfaces as
			// a clearer error from the executor.
			Stdin:  stdin != nil,
			Stdout: stdout != nil,
			Stderr: stderr != nil,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.Config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating SPDY executor for %s/%s: %w", ns, pod, err)
	}

	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		// Tty=false: we're not running an interactive shell, we're
		// piping bytes. With Tty=true the tar stream would get
		// line-buffered/CR-translated/etc, which corrupts the
		// archive.
		Tty: false,
	})
	if err != nil {
		return fmt.Errorf("exec stream against %s/%s: %w", ns, pod, err)
	}
	return nil
}

// StreamLayout pipes a tar of the layout's files into the stage
// Pod's `tar -xf - -C /data/shared/<table>` so the files land at
// /data/shared/<table>/{labels.csv, images/...}.
//
// On success, the stage Pod's PVC mount now contains the files
// jobs-manager's ingestor will read in Phase 4.
//
// On failure, the partial-write state on the PVC is whatever tar
// managed before the error — possibly a few image files but
// likely not all. Phase 4's idempotency_key handles "retry the
// whole push"; for v0.1, customers re-run the command and the
// shell's `mkdir -p` is idempotent so re-creating the destination
// is fine. Truly-recovering a failed mid-stream push (resume from
// last-written file) is a v0.2 optimization.
//
// Why `tar` (rather than a more modern protocol like a custom REST
// upload endpoint on jobs-manager): tar-over-exec works against
// any cluster running the existing chart, with zero server-side
// changes. A custom upload endpoint would require a coordinated
// jobs-manager release before the CLI could ship.
func StreamLayout(
	ctx context.Context,
	exec Executor,
	namespace, podName, containerName string,
	layout *LocalLayout,
	table string,
	progress Progress,
) error {
	if progress == nil {
		progress = NoOpProgress{}
	}
	defer progress.Finish()

	// Generate the staging-dir suffix + compose the remote command
	// BEFORE spawning the tar goroutine. Earlier versions did the
	// suffix after, which meant a crypto/rand failure would return
	// from this function without closing the pipe or waiting on
	// the goroutine — the goroutine would block once the ~64 KB
	// pipe buffer filled, leaking forever. Bugbot flagged on r9.
	//
	// Staging dir gets a fresh 8-hex-char suffix per invocation so
	// two concurrent `dataset push` runs for the same table don't
	// race on the same .staging path (r8). Without this, push A's
	// `rm -rf .staging` could wipe push B's in-progress tar
	// extraction (or vice versa), producing an interleaved
	// corrupt dataset on the PVC.
	stagingSuffix, err := randomSuffix(4) // 4 bytes → 8 hex chars
	if err != nil {
		return fmt.Errorf("generating staging-dir suffix: %w", err)
	}
	dest := StagedPrefix(table)
	staging := dest + ".staging-" + stagingSuffix

	// Backup path for the swap. Same unique suffix as staging so
	// two parallel pushes don't collide on each other's .old-,
	// AND so the find pattern below catches both .staging-* and
	// .old-* siblings as orphan candidates.
	backup := dest + ".old-" + stagingSuffix

	parentDir := path.Dir(dest)
	tableBase := path.Base(dest)

	// Remote command pipeline. Crosses three semantic guarantees:
	//
	//   - HERMETIC (r5): old files don't survive into a re-push
	//   - TRANSACTIONAL (r7+r10): the previously-staged dataset
	//     stays intact if THIS push fails mid-transfer; backup-
	//     and-rollback restores on mv failure
	//   - PARALLEL-SAFE (r8): unique .staging-<hex>/.old-<hex>
	//     suffix per invocation so concurrent pushes don't
	//     interleave each other's tar extraction
	//   - EXIT-FAITHFUL (r11): set -e propagates any non-zero
	//     status; the prior `&&-chain ; find || true` shape
	//     could silently return success on actual failures
	//
	// Pipeline steps:
	//   1. set -e — any unhandled non-zero status aborts the script
	//   2. rm -rf $STAGING (clean prior failed attempt)
	//   3. mkdir + tar (extract to staging)
	//   4. If $DEST exists, mv $DEST → $DEST.old-<hex>  (backup)
	//   5. mv $STAGING → $DEST                          (commit)
	//      On failure: restore backup, exit 1
	//   6. rm -rf $BACKUP                                (success)
	//   7. find ... orphan cleanup (best-effort, || true)
	//
	// The earlier `&&-chained ... ; find ... || true` shape had a
	// hidden correctness bug (Bugbot r11): POSIX `;` runs the
	// next command regardless of prior status, and `|| true` then
	// forces exit 0. A failed tar mid-script would silently
	// return success to the exec subprocess, and the CLI would
	// report the "Uploaded N files" success line on what was
	// actually a failed push.
	//
	// set -e fixes that: any unguarded non-zero exits the script
	// with that status. The find's `|| true` is still fine
	// because under set -e a `cmd || fallback` is treated as
	// "cmd may fail" and doesn't trigger the immediate exit.
	//
	// Failure modes (recap):
	//   - tar fails: set -e aborts; $DEST untouched (transactional from r7)
	//   - backup mv fails: same — aborts before any change to $DEST
	//   - main mv fails: explicit if/then rolls back the backup
	//     then `exit 1` (set -e doesn't preempt the explicit
	//     handler because of the `||`)
	//   - final rm fails: set -e exits, but the customer's new
	//     data is already in place — the leftover .old-<hex>
	//     gets picked up by the find sweep on next invocation
	//   - find fails: `|| true` suppresses (cleanup is best-effort)
	remoteCmd := []string{
		"/bin/sh", "-c",
		fmt.Sprintf(
			"set -e\n"+
				"rm -rf %q\n"+
				"mkdir -p %q\n"+
				"/bin/tar -xf - -C %q\n"+
				"if [ -e %q ]; then mv %q %q; fi\n"+
				"if ! mv %q %q; then\n"+
				"  if [ -e %q ]; then mv %q %q; fi\n"+
				"  exit 1\n"+
				"fi\n"+
				"rm -rf %q\n"+
				"find %q -maxdepth 1 \\( -name %q -o -name %q \\) -mmin +60 -exec rm -rf {} + 2>/dev/null || true\n",
			staging,            // rm
			staging,            // mkdir
			staging,            // tar -C
			dest, dest, backup, // if exists, backup
			staging, dest, // main mv
			backup, backup, dest, // rollback
			backup,                                                 // success cleanup
			parentDir, tableBase+".staging-*", tableBase+".old-*"), // find
	}

	// Use a synchronous pipe so the tar Writer's writes block on
	// the exec stream's reads — no in-memory buffering of the
	// whole archive. For a 1 GiB dataset this is the difference
	// between using 1 GiB of laptop RAM and using a few KB.
	pr, pw := io.Pipe()

	// Wire the progress sink on the WRITE side so every byte the
	// tar Writer emits (headers + bodies) counts toward the bar.
	countedPW := &progressWriter{w: pw, p: progress}

	// Capture stderr from the in-cluster tar so failures surface
	// with the actual remote message.
	var stderrBuf bytes.Buffer

	// Kick off the tar build in a goroutine. The Pipe.Writer MUST
	// be closed when we're done — without that, the Pipe.Reader
	// side blocks forever waiting for more bytes.
	//
	// CloseWithError vs Close: CloseWithError preserves the tar
	// error across the pipe so the exec side sees the cause.
	tarErrCh := make(chan error, 1)
	go func() {
		err := writeLayoutTar(countedPW, layout)
		if err != nil {
			_ = pw.CloseWithError(err)
		} else {
			_ = pw.Close()
		}
		tarErrCh <- err
	}()

	streamErr := exec.Exec(ctx, namespace, podName, containerName,
		remoteCmd, pr, nil, &stderrBuf)

	// Close the reader side. If the executor returned without
	// fully draining stdin (early exit, ctx cancellation, remote
	// tar dying), the tar-write goroutine is blocked on its next
	// pipe.Write — closing the reader unblocks it with
	// io.ErrClosedPipe so it can finish and report on tarErrCh.
	// Without this, every error path deadlocks.
	_ = pr.Close()

	// Drain the tar goroutine — even on stream error, it must
	// finish or we leak. Buffered channel size 1 means this
	// receive won't block (the goroutine already sent or will
	// send imminently after CloseWithError or the pr.Close above).
	tarErr := <-tarErrCh

	// Order matters here. Bugbot r11 caught the previous logic:
	// when the LOCAL tar build fails (e.g. r8's stream-time size
	// cap recheck rejecting a file that grew on disk), the
	// CloseWithError on the pipe causes exec to see "broken pipe"
	// and return a generic stream error. If we report streamErr
	// first, the customer sees "streaming failed" instead of the
	// actually-actionable "dataset exceeded v0.1 cap" diagnostic
	// from the tar side.
	//
	// Check tarErr first — when both are non-nil, the tar side
	// is usually the upstream cause. streamErr-only (no tarErr)
	// is the genuine network/RBAC/remote-tar-failed case where
	// the exec wording is the right surface.
	if tarErr != nil {
		return fmt.Errorf("building tar archive: %w", tarErr)
	}
	if streamErr != nil {
		hint := ""
		if remote := stderrBuf.String(); remote != "" {
			hint = fmt.Sprintf(" (remote tar stderr: %s)", remote)
		}
		return fmt.Errorf("streaming files to %s/%s: %w%s", namespace, podName, streamErr, hint)
	}
	return nil
}

// writeLayoutTar packages the layout's files into the writer as a
// flat tar archive with this structure:
//
//	labels.csv          (file)
//	images/<basename>   (file, per layout.Images)
//
// The destination tar is unpacked with `tar -xf - -C /data/shared/<table>`,
// so file paths are RELATIVE to that root — that's why we write
// "labels.csv" not "/data/shared/<table>/labels.csv".
//
// Mode is 0644 on all files (read-only for the ingestor SA). Tar's
// type flag is TypeReg (regular file). No symlinks, no hard links,
// no extended attrs — the layout we accept doesn't have any.
//
// Uses a named return + deferred Close so a tar trailer-write error
// (truncated archive — GNU tar refuses to extract these) propagates
// even on the happy path. errcheck would otherwise flag the bare
// `defer tw.Close()`, and rightly: silently dropping that error
// means a truncated stream looks identical to a successful one
// from this function's caller.
func writeLayoutTar(w io.Writer, layout *LocalLayout) (err error) {
	tw := tar.NewWriter(w)
	defer func() {
		// If we already have an error, preserve it; the close error
		// is less useful than whatever caused the early return.
		// Otherwise surface the close error so a failed trailer
		// write doesn't silently corrupt the archive.
		if cerr := tw.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing tar writer: %w", cerr)
		}
	}()

	// Running total enforces the v0.1 MaxTotalBytes cap at STREAM
	// time, not just at Discover time. A file that grew between
	// pre-flight and now (TOCTOU) — or just a sum miscount — would
	// otherwise sneak past the cap silently. Bugbot flagged on
	// PR-b r8 as Medium.
	var totalBytes int64

	// labels.csv first (small, sanity-checks the stream quickly).
	n, err := writeTarFile(tw, layout.LabelsCSV, "labels.csv")
	if err != nil {
		return fmt.Errorf("packaging labels.csv: %w", err)
	}
	totalBytes += n
	if totalBytes > MaxTotalBytes {
		return fmt.Errorf(
			"dataset exceeded v0.1 total cap of %s during stream "+
				"(labels.csv alone is %s; pre-flight likely raced with "+
				"a file growing on disk)",
			HumanBytes(MaxTotalBytes), HumanBytes(totalBytes))
	}

	// Then each image. We write them in the order Discover returned
	// (filesystem-walk order, not sorted) — sorting would make the
	// stream deterministic across runs but adds zero customer value.
	for _, abs := range layout.Images {
		// The destination filename inside images/ is the file's
		// basename — strips the customer's local path so a push
		// from /home/alice/datasets/cats_dogs/images/001.jpg
		// becomes images/001.jpg in the tar (and on the PVC).
		//
		// Use path.Join (always forward-slash) NOT filepath.Join
		// (OS-native separator) for the tar HEADER name. USTAR/POSIX
		// tar requires forward slashes; a Windows-built CLI using
		// filepath.Join would emit `images\001.jpg` headers that the
		// Linux stage Pod's `tar -xf` either rejects or extracts as
		// a flat-named file. Bugbot flagged on PR-b round 6.
		dst := path.Join("images", filepath.Base(abs))
		n, err := writeTarFile(tw, abs, dst)
		if err != nil {
			return fmt.Errorf("packaging %s: %w", abs, err)
		}
		totalBytes += n
		if totalBytes > MaxTotalBytes {
			return fmt.Errorf(
				"dataset exceeded v0.1 total cap of %s after streaming %s "+
					"(reached %s; file growth between pre-flight and stream)",
				HumanBytes(MaxTotalBytes), dst, HumanBytes(totalBytes))
		}
	}

	// Generic sidecar directories (texts/, sequences/, and — later —
	// annotations/, masks/), each staged under "<name>/<basename>".
	// Sorted by dir name for deterministic stream order.
	for _, name := range sortedKeys(layout.Sidecars) {
		for _, abs := range layout.Sidecars[name] {
			dst := path.Join(name, filepath.Base(abs))
			n, err := writeTarFile(tw, abs, dst)
			if err != nil {
				return fmt.Errorf("packaging %s: %w", abs, err)
			}
			totalBytes += n
			if totalBytes > MaxTotalBytes {
				return fmt.Errorf(
					"dataset exceeded v0.1 total cap of %s after streaming %s (reached %s)",
					HumanBytes(MaxTotalBytes), dst, HumanBytes(totalBytes))
			}
		}
	}

	// tw.Close() in the defer above writes the tar footer
	// (two zero blocks). Without that, GNU tar treats the archive
	// as truncated and refuses to extract.
	return nil
}

// sortedKeys returns a map's string keys in sorted order, for
// deterministic iteration when packaging Sidecars.
func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// writeTarFile writes one file from `src` into tw under the
// archive-relative name `dst`. Streams the file body — no full-
// read into memory — so a single 500 MiB image doesn't balloon
// the CLI's memory.
//
// Returns the file's declared size (from os.Lstat) so the caller
// can maintain a running total against MaxTotalBytes — closes the
// stream-time half of the TOCTOU window Bugbot flagged on r8.
//
// Uses os.Lstat (not Stat) + an explicit symlink reject as a
// defense-in-depth second layer behind walk.go's rejectSymlink
// (Bugbot r4). Also re-checks the single-file size cap at stream
// time: a file that grew between Discover and now would otherwise
// upload past the advertised 500 MiB cap (Bugbot r8).
func writeTarFile(tw *tar.Writer, src, dst string) (int64, error) {
	st, err := os.Lstat(src)
	if err != nil {
		return 0, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		// Reaching here means Discover let a symlink through —
		// shouldn't happen in production, but the explicit error
		// keeps the security property locally enforceable.
		return 0, fmt.Errorf("refusing to stream symbolic link %q (defense-in-depth; should have been rejected by Discover)", src)
	}
	// Stream-time recheck of the single-file cap. Discover caught this in
	// pre-flight; if we see it again here, the file grew between then and now.
	if err := checkFileSize(dst, st.Size()); err != nil {
		return 0, err
	}
	hdr := &tar.Header{
		Name:     dst,
		Size:     st.Size(),
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		// We don't carry ModTime forward — the customer's local
		// mtime has no useful semantic on the PVC, and zeroing it
		// keeps the tar bit-for-bit reproducible across runs
		// (helps tests, makes future content-hash checks easier).
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, err
	}
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	// Read-only file Close errors are not meaningful — there's
	// nothing the caller can do, and the underlying file descriptor
	// gets returned to the OS regardless. Explicit-discard pattern,
	// same as the Fprintf writers elsewhere.
	defer func() { _ = f.Close() }()
	// io.CopyN caps the actual stream at the declared header size.
	// Without the cap, a file that grew between Lstat above and
	// the Copy here would overflow the header — the tar would be
	// corrupted (header says N bytes, body has > N). Cap-and-trust
	// is the safe choice: if the file shrank, the tar trailer
	// surfaces the mismatch; if the file grew, we just stop at
	// the declared size and let the new bytes get re-pushed next
	// invocation. Closes the body-side half of the TOCTOU window.
	if _, err := io.CopyN(tw, f, st.Size()); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	return st.Size(), nil
}
