package push

import (
	"context"
	"fmt"
	"io"
	"time"

	"k8s.io/client-go/kubernetes"
)

// StageOptions packages every dependency Stage needs into a single
// struct so callers (cli.runDatasetPush) can build it incrementally
// without juggling 10 positional args.
//
// Required fields are pinned in the doc; the nil-able Progress + Out
// fields get default behaviors (NoOpProgress / io.Discard) so the
// happy path is short.
type StageOptions struct {
	// Client is the discovered kubernetes.Interface.
	Client kubernetes.Interface

	// Executor is how the tar stream actually reaches the Pod.
	// Production: a *SPDYExecutor backed by the rest.Config.
	// Tests: a fakeExecutor (see stream_test.go).
	Executor Executor

	// Namespace + IngestorSAName come from parent-release discovery
	// (IngestorSAName is read from the ingestionAuthz ConfigMap, #7).
	Namespace      string
	IngestorSAName string

	// PVCClaimName + PVCMountPath come from Phase 3 PR-a's PVC
	// discovery (always "client-pvc" + "/data/shared" today).
	PVCClaimName string
	PVCMountPath string

	// Layout is the validated local files-to-stage.
	Layout *LocalLayout

	// Table is the destination table name. MUST have already
	// passed ValidateTableName.
	Table string

	// StagePodImage overrides the alpine default. Empty = default.
	StagePodImage string

	// Progress is the customer-facing transfer bar. Nil = no-op
	// (use this in tests / non-TTY output).
	Progress Progress

	// Out is where Stage prints non-progress diagnostic output
	// (orphan warnings, copy-channel status, etc.). Nil = io.Discard.
	// Progress bar output is separate — schollz writes to its own
	// configured writer (typically the same one).
	Out io.Writer
}

// Stage is Phase 3 PR-b's top-level entrypoint. Does the full
// dance: orphan scan → create stage Pod → wait Ready → tar-over-
// exec stream → defer-delete (SIGINT-safe).
//
// On success, the layout's files are on the cluster's PVC at
// /data/shared/<table>/{labels.csv, images/...} and the stage Pod
// has been cleaned up. Caller proceeds to Phase 4's submit step.
//
// On error, the deferred cleanup still fires — no orphan Pod left
// behind even if the stream fails partway. Phase 4's idempotency
// key handles the "retry the whole push" scenario; for v0.1 we
// don't try to resume a partial transfer.
//
// SIGINT handling: the caller is expected to wire a signal handler
// that cancels the passed-in ctx (cli/main.go pattern). When ctx
// is cancelled, the in-flight Exec stream returns ctx.Err(), the
// deferred DeleteStagePod runs with a FRESH context (so it can
// reach the API even after our parent ctx is cancelled), and the
// error propagates with the SIGINT signal preserved.
func Stage(ctx context.Context, opts StageOptions) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Progress == nil {
		opts.Progress = NoOpProgress{}
	}

	// 1. Orphan scan — best-effort. We log either the warning or
	//    the scan-failure and proceed regardless. Customers who
	//    care about orphans get to see them; customers whose RBAC
	//    forbids List Pods just don't get the warning (no impact
	//    on the push itself).
	if orphans, err := FindOrphanStagePods(ctx, opts.Client, opts.Namespace); err != nil {
		_, _ = fmt.Fprintf(opts.Out, "(orphan-scan skipped: %v)\n", err)
	} else if warning := FormatOrphansWarning(orphans); warning != "" {
		_, _ = fmt.Fprint(opts.Out, warning)
	}

	// 2. Create the stage Pod. From here on, cleanup matters.
	podName, err := CreateStagePod(ctx, opts.Client, PodSpecOptions{
		Namespace:          opts.Namespace,
		PVCClaimName:       opts.PVCClaimName,
		PVCMountPath:       opts.PVCMountPath,
		Table:              opts.Table,
		Image:              opts.StagePodImage,
		ServiceAccountName: opts.IngestorSAName,
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(opts.Out, "Opened a secure channel to your workspace's storage.\n")

	// 3. Defer cleanup. The deferred call uses a FRESH context with
	//    its own deadline — if the parent ctx is cancelled (SIGINT,
	//    timeout), we still want the Pod deleted. Without this, a
	//    Ctrl-C right after pod-create would leak an orphan.
	//
	//    30s deadline on the cleanup is a safety net for the case
	//    where the API server itself is unreachable — better to
	//    print "failed to clean up stage Pod" than hang forever.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if delErr := DeleteStagePod(cleanupCtx, opts.Client, opts.Namespace, podName); delErr != nil {
			_, _ = fmt.Fprintf(opts.Out,
				"WARNING: failed to delete stage Pod %s/%s (it will be activeDeadline-killed in <=%ds): %v\n",
				opts.Namespace, podName, StagePodActiveDeadline, delErr)
		}
	}()

	// 4. Wait for the Pod to be Ready (containers up, ready to
	//    accept exec). Times out at StagePodReadyTimeout with
	//    diagnostic hints from container statuses (image pull,
	//    scheduling) when we have them.
	_, _ = fmt.Fprintf(opts.Out, "Preparing the copy (up to %s)…\n", StagePodReadyTimeout)
	if _, err := WaitForStagePodReady(ctx, opts.Client, opts.Namespace, podName); err != nil {
		return err
	}

	// 5. Stream the tar. This is where actual bytes flow. The
	//    progress bar (if TTY) renders during this call.
	_, _ = fmt.Fprintf(opts.Out, "Copying %d files (%s) into %q…\n",
		opts.Layout.FileCount(), HumanBytes(opts.Layout.TotalBytes), opts.Table)

	if err := StreamLayout(ctx, opts.Executor,
		opts.Namespace, podName, "stage",
		opts.Layout, opts.Table, opts.Progress); err != nil {
		return err
	}

	// 6. Print "done" message. The deferred cleanup runs after this.
	_, _ = fmt.Fprintf(opts.Out, "Copied %d files into %q.\n",
		opts.Layout.FileCount(), opts.Table)
	return nil
}
