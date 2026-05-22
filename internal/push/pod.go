package push

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// DefaultStagePodImage is the alpine image the ephemeral stage Pod
// runs. Pinned by digest at CLI build time so a customer pulling
// v0.1.x at any future date gets bit-for-bit identical behavior —
// no "alpine just shipped a glibc-on-musl regression and now my
// stage Pod crashloops" surprises.
//
// alpine:3.20 was the current 3.x stable when Phase 3 PR-b landed
// (May 2026). The image is tiny (~8 MiB), has tar/sh/busybox built
// in, and is one of the most-mirrored images on the planet — air-
// gapped customers can almost always pull it from their internal
// mirror without extra config.
//
// Override via `tracebloc dataset push --stage-pod-image=...` for:
//
//   - Customers on registries that don't proxy docker.io
//   - Customers running a curated base image with extra audit
//     instrumentation
//   - Custom forks that need a specific busybox build
//
// Bumping the pinned digest is a v0.2 task; track it alongside
// the kube-deps refresh.
const DefaultStagePodImage = "alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// StagePodLabel{Key,Value} mark every Pod the CLI creates so the
// orphan-scan logic (orphan.go) can find leftover Pods from a
// previously crashed `dataset push` invocation. Using the
// kubernetes.io/managed-by label means the chart's own resources
// (managed-by=Helm) don't get caught in our scan.
const (
	StagePodManagedByLabel = "app.kubernetes.io/managed-by"
	StagePodManagedByValue = "tracebloc-cli"

	// StagePodComponentLabel narrows the orphan scan to just stage
	// Pods (not, e.g., a future `tracebloc cluster doctor` Pod).
	StagePodComponentLabel = "tracebloc.io/component"
	StagePodComponentValue = "stage-pod"

	// StagePodTableLabel records which table the Pod was staging
	// for. Useful in the orphan-warning output so the customer
	// knows which `dataset push` invocation it came from.
	StagePodTableLabel = "tracebloc.io/table"
)

// StagePodActiveDeadline is the in-cluster self-kill timer. The CLI
// also deletes the Pod via defer + SIGINT handler, but those only
// fire if the CLI process is still alive — a hard kill (`kill -9`,
// OS crash, network partition between laptop and cluster) leaves
// the Pod stranded. activeDeadlineSeconds means the kubelet kills
// it even if the CLI never gets the chance.
//
// 10 minutes is roughly 6x our largest expected v0.1 transfer
// (1 GiB at a conservative 2 MB/s gives ~8.5 min). Customers
// pushing close to the cap will hit the deadline; they're already
// pointed at the v0.2 cloud-source story by the size-cap
// diagnostic. v0.2 should make this configurable per push.
const StagePodActiveDeadline = 600

// StagePodReadyTimeout is how long we wait for the Pod to become
// Running + Ready after CREATE. Most clusters spawn an alpine Pod
// in 5-15 seconds; 60 seconds covers image-pull from a slow mirror
// + scheduler back-pressure on busy clusters. Beyond that, something
// is wrong (no image-pull access, no schedulable node, PSP/PSA
// rejection) and the customer wants the diagnostic, not a longer
// wait.
const StagePodReadyTimeout = 60 * time.Second

// PodSpecOptions controls the ephemeral stage Pod construction.
// Fields are intentionally narrow — every knob is one a customer
// could plausibly need to turn for an air-gapped or hardened-
// security setup. Adding fields should require a real use case.
type PodSpecOptions struct {
	// Namespace is where the Pod gets created — always the
	// discovered parent-release namespace.
	Namespace string

	// PVCClaimName is the shared PVC to mount at /data/shared.
	// Discovered by cluster.DiscoverSharedPVC (always "client-pvc"
	// today, but routed through a field so a future per-customer
	// override doesn't require touching this signature).
	PVCClaimName string

	// PVCMountPath is where to mount the PVC inside the Pod —
	// "/data/shared" by chart convention.
	PVCMountPath string

	// Table is the destination table name. Used to compose the Pod
	// name and the on-PVC subdirectory. MUST have already passed
	// ValidateTableName; pod-name composition relies on the same
	// character-class restrictions.
	Table string

	// Image overrides DefaultStagePodImage. Empty = use default.
	Image string

	// ServiceAccountName is the SA the Pod runs as. Phase 2's
	// discovery surfaces this as `ingestor` (the chart default) or
	// whatever the customer's `--ingestor-sa` flag overrides to.
	// Using the chart's existing SA means the Pod inherits any
	// imagePullSecrets and PSA exemptions the admin already
	// configured for it.
	ServiceAccountName string
}

// BuildStagePodSpec produces the corev1.Pod for an ephemeral stage
// Pod, fully parametrized but with no cluster side-effects.
// Separated from CreateStagePod so unit tests can assert the spec
// shape without needing a fake clientset for every assertion.
//
// Security context follows the Kubernetes Pod Security Standards
// "restricted" profile — the strictest preset, accepted on every
// PSA-enabled namespace including the chart's recommended config.
// This is intentional: the stage Pod runs in the customer's
// namespace and writes to their PVC, so being a model citizen for
// PSA defaults reduces "the Pod won't even start on my cluster"
// surface area.
func BuildStagePodSpec(opts PodSpecOptions) *corev1.Pod {
	image := opts.Image
	if image == "" {
		image = DefaultStagePodImage
	}

	suffix, _ := randomSuffix(4) // 4 bytes → 8 hex chars
	podName := fmt.Sprintf("tracebloc-stage-%s-%s", opts.Table, suffix)

	// Pod-level security context: runAsNonRoot is the only field
	// PSA's restricted profile *requires* at the Pod level (the
	// rest are container-level). Setting it here means every
	// future container (if we ever add one) inherits the
	// non-root constraint.
	runAsNonRoot := true
	runAsUser := int64(65532) // distroless's "nonroot" UID; works on any cluster

	// Container-level security context: every PSA-restricted
	// requirement. Reads top-to-bottom: capabilities dropped,
	// read-only root FS, no privilege escalation, no privileged,
	// seccomp default. None of these prevent tar from working —
	// tar reads stdin + writes to the PVC mount (which is RW),
	// and doesn't need any caps to run as a non-root user.
	allowPrivEsc := false
	privileged := false
	readOnlyRootFS := true

	activeDeadline := int64(StagePodActiveDeadline)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: opts.Namespace,
			Labels: map[string]string{
				StagePodManagedByLabel: StagePodManagedByValue,
				StagePodComponentLabel: StagePodComponentValue,
				StagePodTableLabel:     opts.Table,
			},
			Annotations: map[string]string{
				// Annotations are searchable via `kubectl describe`
				// but don't constrain scheduling — perfect for
				// breadcrumbs that help post-mortem an orphan.
				"tracebloc.io/created-at": time.Now().UTC().Format(time.RFC3339),
				"tracebloc.io/created-by": "tracebloc-cli",
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: opts.ServiceAccountName,
			RestartPolicy:      corev1.RestartPolicyNever,

			// 10-min in-cluster self-kill — see comment on the const.
			ActiveDeadlineSeconds: &activeDeadline,

			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &runAsNonRoot,
				RunAsUser:    &runAsUser,
				FSGroup:      &runAsUser, // PVC writes need this group
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},

			Containers: []corev1.Container{{
				Name:  "stage",
				Image: image,
				// `sleep` keeps the Pod alive long enough for the
				// CLI to open an exec stream and tar files in.
				// activeDeadlineSeconds caps the worst case; the
				// CLI deletes the Pod the moment the stream
				// finishes (or fails).
				//
				// Why not the busybox `sleep infinity` idiom: it
				// causes some PSA configurations to flag the Pod
				// because the container holds the SIGTERM until
				// activeDeadlineSeconds fires (sleep traps signals
				// imperfectly). A finite sleep matched to the
				// deadline is gentler.
				Command: []string{"/bin/sleep", fmt.Sprintf("%d", StagePodActiveDeadline)},

				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &allowPrivEsc,
					Privileged:               &privileged,
					ReadOnlyRootFilesystem:   &readOnlyRootFS,
					RunAsNonRoot:             &runAsNonRoot,
					RunAsUser:                &runAsUser,
					Capabilities: &corev1.Capabilities{
						// PSA restricted requires ALL capabilities
						// dropped, then optionally add back
						// NET_BIND_SERVICE. tar doesn't need it.
						Drop: []corev1.Capability{"ALL"},
					},
					SeccompProfile: &corev1.SeccompProfile{
						Type: corev1.SeccompProfileTypeRuntimeDefault,
					},
				},

				VolumeMounts: []corev1.VolumeMount{{
					Name:      "shared",
					MountPath: opts.PVCMountPath,
				}, {
					// tar needs a writable working dir for its
					// temporary state; with ReadOnlyRootFilesystem
					// it can't use /tmp on the root FS. An
					// emptyDir at /tmp is the standard pattern.
					Name:      "tmp",
					MountPath: "/tmp",
				}},

				// Conservative resource requests: tar of typical
				// image_classification data uses <100 MiB RAM and
				// negligible CPU. Setting requests lets the
				// scheduler place us; setting limits keeps a
				// runaway tar (e.g. corrupted archive triggering
				// a busy loop) from impacting the cluster.
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			}},

			Volumes: []corev1.Volume{{
				Name: "shared",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: opts.PVCClaimName,
					},
				},
			}, {
				Name: "tmp",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			}},
		},
	}
}

// CreateStagePod creates the Pod in the cluster and returns the
// metadata.name the API server assigned. The returned name is what
// callers pass to WaitForStagePodReady + DeleteStagePod.
//
// Take-name-from-API-response is deliberate: even though
// BuildStagePodSpec pre-computes a name, the API server can in
// principle rewrite it (e.g. via a mutating admission webhook
// renaming with a prefix). Reading back from the response is the
// safe contract.
func CreateStagePod(ctx context.Context, cs kubernetes.Interface, opts PodSpecOptions) (string, error) {
	pod := BuildStagePodSpec(opts)
	created, err := cs.CoreV1().Pods(opts.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		// Most-common failure path: PSA rejects the Pod because
		// the customer's namespace policy is stricter than even
		// "restricted." We surface the raw API error so the customer
		// sees the PSA violation list verbatim — that's the most
		// actionable thing.
		return "", fmt.Errorf("creating stage Pod in namespace %q: %w", opts.Namespace, err)
	}
	return created.Name, nil
}

// WaitForStagePodReady polls until the Pod's status reports
// Ready=True (the canonical "containers started and not yet
// terminated" signal). Times out per StagePodReadyTimeout with a
// diagnostic that tries to help — image-pull failures and
// scheduler back-pressure are the dominant slow paths and have
// distinct symptom strings.
//
// Returns nil on Ready, the pod object on success so callers don't
// have to re-Get for it.
func WaitForStagePodReady(ctx context.Context, cs kubernetes.Interface, namespace, podName string) (*corev1.Pod, error) {
	var lastObserved *corev1.Pod

	// Poll every 1s — image-pull failures surface in seconds, and
	// the Ready transition itself is instant once kubelet has
	// pulled+started. Tighter polling burns API server CPU for no
	// benefit; looser polling delays the user.
	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, StagePodReadyTimeout, true,
		func(ctx context.Context) (bool, error) {
			p, err := cs.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				// Transient errors (network blips, brief API
				// unavailability) shouldn't terminate the poll.
				// Returning (false, nil) keeps the poll going.
				// Persistent errors will exhaust the timeout and
				// surface there with last-observed context.
				return false, nil
			}
			lastObserved = p
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					return true, nil
				}
			}
			return false, nil
		})

	if err == nil {
		return lastObserved, nil
	}

	// Timeout or context-cancelled. Try to give a useful hint
	// from the last-observed Pod status.
	hint := podReadyTimeoutHint(lastObserved)
	return nil, fmt.Errorf(
		"stage Pod %s/%s did not become Ready within %s: %w%s",
		namespace, podName, StagePodReadyTimeout, err, hint)
}

// podReadyTimeoutHint extracts the most useful diagnostic from a
// last-observed Pod status. Designed to match against the two
// dominant slow-path scenarios:
//
//  1. Image pull is slow or failed (ImagePullBackOff,
//     ErrImagePull) — surface the registry message.
//  2. Pod is unschedulable (PodScheduled=False with reason) —
//     surface the scheduler's message.
func podReadyTimeoutHint(p *corev1.Pod) string {
	if p == nil {
		return " (no Pod status observed — check API server connectivity)"
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return fmt.Sprintf(" (last container state: %s — %s)",
				cs.State.Waiting.Reason, cs.State.Waiting.Message)
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return fmt.Sprintf(" (scheduling: %s — %s)", c.Reason, c.Message)
		}
	}
	return fmt.Sprintf(" (Pod phase: %s)", p.Status.Phase)
}

// DeleteStagePod removes the Pod. Called via defer in the orchestrator
// so it runs on both success and failure (including SIGINT, which
// is wired up to cancel the parent context and let the defer fire
// in normal stack unwind).
//
// Uses background propagation + a tiny grace period because we
// don't care about giving sleep a graceful shutdown — we just want
// the Pod gone so the next push doesn't see an orphan.
func DeleteStagePod(ctx context.Context, cs kubernetes.Interface, namespace, podName string) error {
	gracePeriod := int64(0)
	propagation := metav1.DeletePropagationBackground
	err := cs.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
		PropagationPolicy:  &propagation,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		// Not-found is fine: the Pod might have already self-killed
		// via activeDeadlineSeconds, or a parallel `kubectl delete`
		// got there first. Either way, our goal (no orphan) is
		// achieved. Returning the error here would mask the
		// upstream cause of failure that triggered the defer.
		return fmt.Errorf("deleting stage Pod %s/%s: %w", namespace, podName, err)
	}
	return nil
}

// randomSuffix returns a hex string of length 2*n. Used to make
// stage Pod names unique across parallel `dataset push` invocations
// for the same table — without this, two CLI runs would race on the
// same Pod name and one would fail the Create with AlreadyExists.
//
// Cryptographic randomness is overkill for collision avoidance
// (8 hex chars = 32 bits of entropy is way more than needed) but
// crypto/rand is the simpler import compared to math/rand which
// needs explicit seeding.
func randomSuffix(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
