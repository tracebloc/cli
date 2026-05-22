package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SharedPVCClaimName is the chart's hardcoded shared-data PVC name.
//
// From tracebloc/client's templates/_helpers.tpl:
//
//	{{- define "tracebloc.clientDataPvc" -}}
//	client-pvc
//	{{- end }}
//
// The helper isn't parameterized by release name (yet) — every
// installation of the chart creates a PVC literally named
// "client-pvc". We probe by claim name rather than by labels here
// because the chart's labels include the helm release name and we
// already discovered the release via DiscoverParentRelease — once
// we have the namespace, the name is unambiguous.
//
// If a customer renames the PVC (out-of-band patch, mostly), they
// need the v0.2 follow-up that reads the name from
// jobs-manager's volume-mount spec instead of hardcoding. Tracked
// as a future ticket alongside #7 (the ingestor-SA-name discovery).
const SharedPVCClaimName = "client-pvc"

// SharedPVCMountPath is where jobs-manager mounts the shared PVC
// inside its container. The CLI's staging Pod (Phase 3 PR-b) uses
// the same mount path so any tooling that introspects "where files
// live" in either context agrees. From jobs-manager-deployment.yaml:
//
//	volumeMounts:
//	  - name: shared-volume
//	    mountPath: "/data/shared"
const SharedPVCMountPath = "/data/shared"

// SharedPVC describes the chart's shared-data PVC after discovery.
// Carries enough metadata for Phase 3 PR-b to construct a stage Pod
// that can mount the same claim.
type SharedPVC struct {
	// ClaimName is the metadata.name of the PVC, always
	// SharedPVCClaimName today. Wrapped in a field rather than
	// re-using the constant so a future "discovered via labels"
	// implementation can vary the name without changing callers.
	ClaimName string

	// MountPath is the in-container directory where the chart's
	// pods mount this claim — SharedPVCMountPath today. Same
	// rationale as ClaimName for being a field.
	MountPath string

	// AccessModes is the resolved access-mode list on the PVC's
	// spec. Surfaces in `tracebloc cluster info` (eventually) and
	// drives the stage-Pod scheduling story:
	//
	//   - ReadWriteMany: stage Pod can schedule on any node
	//   - ReadWriteOnce: stage Pod must land on the same node
	//     as whatever else is using the PVC (jobs-manager,
	//     mysql-client). PR-b will surface a warning here so
	//     RWO clusters get diagnostic guidance up front.
	AccessModes []corev1.PersistentVolumeAccessMode

	// Phase is the PVC's current bind phase. We check Bound — an
	// Unbound PVC means the cluster never provisioned storage
	// (e.g. missing StorageClass), and the stage Pod would hang
	// indefinitely waiting for a volume.
	Phase corev1.PersistentVolumeClaimPhase
}

// DiscoverSharedPVC verifies the chart's shared-data PVC exists in
// the given namespace and returns its metadata. Returns a friendly
// error if the PVC isn't there or isn't Bound — both are real
// situations Phase 3's pre-flight catches before we waste time
// constructing a Pod that can't mount anything.
func DiscoverSharedPVC(ctx context.Context, cs kubernetes.Interface, namespace string) (*SharedPVC, error) {
	pvc, err := cs.CoreV1().PersistentVolumeClaims(namespace).
		Get(ctx, SharedPVCClaimName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The 80% case for "the CLI's pre-flight failed against
			// a cluster that has SOME tracebloc release installed
			// but not the parent client chart" — the parent-release
			// check (DiscoverParentRelease) catches the no-release
			// case first, so reaching here usually means the chart
			// was installed with a customized PVC name.
			return nil, fmt.Errorf(
				"no PersistentVolumeClaim named %q found in namespace %q. "+
					"The chart's _helpers.tpl pins this name; if your install "+
					"renamed it out-of-band, the CLI doesn't yet support that "+
					"(read-name-from-jobs-manager is a v0.2 follow-up). "+
					"Verify with: kubectl get pvc -n %s",
				SharedPVCClaimName, namespace, namespace)
		}
		// Forbidden / network / other — surface as-is so the
		// customer can RBAC-debug. Wrapping rather than substituting
		// because the underlying %w already carries the useful info.
		return nil, fmt.Errorf("reading PVC %s/%s: %w",
			namespace, SharedPVCClaimName, err)
	}

	if pvc.Status.Phase != corev1.ClaimBound {
		// Unbound PVCs are a real cluster-config issue — pre-flight
		// surfacing of this is much more helpful than the stage Pod
		// silently pending forever. The most common cause is a
		// missing StorageClass (the chart's default is the cluster
		// default, which may not exist on EKS without
		// gp2/gp3-csi configured).
		return nil, fmt.Errorf(
			"PVC %s/%s is in phase %q, not Bound. "+
				"The shared volume hasn't been provisioned — "+
				"check that the cluster has a usable StorageClass "+
				"(kubectl get sc) and that the PVC's storageClassName matches.",
			namespace, SharedPVCClaimName, pvc.Status.Phase)
	}

	return &SharedPVC{
		ClaimName:   pvc.Name,
		MountPath:   SharedPVCMountPath,
		AccessModes: pvc.Spec.AccessModes,
		Phase:       pvc.Status.Phase,
	}, nil
}

// IsReadWriteMany reports whether the PVC accepts simultaneous
// mounts from multiple nodes. The Phase 3 stage Pod cares about
// this because RWO claims force same-node scheduling — and if the
// existing mounter (jobs-manager) is on a different node than
// where the scheduler wants to put our stage Pod, the Pod will
// pend indefinitely. PR-b will surface a pre-flight warning when
// this returns false; the dataset push still proceeds (eventually
// succeeds when the scheduler co-locates), it just takes longer.
func (p *SharedPVC) IsReadWriteMany() bool {
	for _, m := range p.AccessModes {
		if m == corev1.ReadWriteMany {
			return true
		}
	}
	return false
}
