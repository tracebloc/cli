package push

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

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

	// Use a synchronous pipe so the tar Writer's writes block on
	// the exec stream's reads — no in-memory buffering of the
	// whole archive. For a 1 GiB dataset this is the difference
	// between using 1 GiB of laptop RAM and using a few KB.
	pr, pw := io.Pipe()

	// Wire the progress sink on the WRITE side so every byte the
	// tar Writer emits (headers + bodies) counts toward the bar.
	// Doing it here (not on individual file reads) means we don't
	// have to instrument the tar.Writer internals.
	countedPW := &progressWriter{w: pw, p: progress}

	// Capture stderr from the in-cluster tar so failures (e.g.
	// "no space left on device" on a near-full PVC) surface with
	// the actual remote message, not a generic "exec failed."
	var stderrBuf bytes.Buffer

	// Kick off the tar build in a goroutine. The Pipe.Writer MUST
	// be closed when we're done — without that, the Pipe.Reader
	// side blocks forever waiting for more bytes.
	//
	// CloseWithError vs Close: CloseWithError preserves the tar
	// error across the pipe so the exec side sees the cause
	// (otherwise the executor reports EOF and the real tar error
	// gets swallowed).
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

	// Compose the remote command. We mkdir -p the destination
	// (idempotent — re-push overwrites) and pipe stdin into tar.
	// `-C` makes tar extract there instead of cwd. Using sh -c
	// rather than a multi-argv form because the && sequencing is
	// what we want, and the table name is already
	// ValidateTableName-guarded so it can't shell-escape.
	//
	// `exec /bin/tar` replaces the shell with tar in the same
	// process, so the only thing waiting on tar's stdout is the
	// kubelet — one less indirection in the pipe chain.
	dest := StagedPrefix(table)
	remoteCmd := []string{
		"/bin/sh", "-c",
		fmt.Sprintf("mkdir -p %q && exec /bin/tar -xf - -C %q", dest, dest),
	}

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

	// Stream errors trump tar errors: if exec failed, the tar
	// goroutine likely failed because of a broken pipe, which is
	// downstream noise. Customer wants the exec error.
	if streamErr != nil {
		hint := ""
		if remote := stderrBuf.String(); remote != "" {
			hint = fmt.Sprintf(" (remote tar stderr: %s)", remote)
		}
		return fmt.Errorf("streaming files to %s/%s: %w%s", namespace, podName, streamErr, hint)
	}
	if tarErr != nil {
		return fmt.Errorf("building tar archive: %w", tarErr)
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

	// labels.csv first (small, sanity-checks the stream quickly).
	if err := writeTarFile(tw, layout.LabelsCSV, "labels.csv"); err != nil {
		return fmt.Errorf("packaging labels.csv: %w", err)
	}

	// Then each image. We write them in the order Discover returned
	// (filesystem-walk order, not sorted) — sorting would make the
	// stream deterministic across runs but adds zero customer value.
	for _, abs := range layout.Images {
		// The destination filename inside images/ is the file's
		// basename — strips the customer's local path so a push
		// from /home/alice/datasets/cats_dogs/images/001.jpg
		// becomes images/001.jpg in the tar (and on the PVC).
		dst := filepath.Join("images", filepath.Base(abs))
		if err := writeTarFile(tw, abs, dst); err != nil {
			return fmt.Errorf("packaging %s: %w", abs, err)
		}
	}

	// tw.Close() in the defer above writes the tar footer
	// (two zero blocks). Without that, GNU tar treats the archive
	// as truncated and refuses to extract.
	return nil
}

// writeTarFile writes one file from `src` into tw under the
// archive-relative name `dst`. Streams the file body — no full-
// read into memory — so a single 500 MiB image doesn't balloon
// the CLI's memory.
//
// Uses os.Lstat (not Stat) + an explicit symlink reject as a
// defense-in-depth second layer behind walk.go's rejectSymlink.
// The Discover-side check is the primary fix for the
// symlink-bypass-size-caps hole Bugbot flagged on PR-b round 4;
// this layer guards against a future refactor that calls
// writeTarFile directly without going through Discover.
func writeTarFile(tw *tar.Writer, src, dst string) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		// Reaching here means Discover let a symlink through —
		// shouldn't happen in production, but the explicit error
		// keeps the security property locally enforceable.
		return fmt.Errorf("refusing to stream symbolic link %q (defense-in-depth; should have been rejected by Discover)", src)
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
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	// Read-only file Close errors are not meaningful — there's
	// nothing the caller can do, and the underlying file descriptor
	// gets returned to the OS regardless. Explicit-discard pattern,
	// same as the Fprintf writers elsewhere.
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(tw, f); err != nil {
		return err
	}
	return nil
}
