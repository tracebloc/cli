package push

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// stageOpts is the minimal PodSpecOptions for happy-path tests —
// individual cases mutate one field to exercise their specific
// branch.
func stageOpts() PodSpecOptions {
	return PodSpecOptions{
		Namespace:          "tracebloc",
		PVCClaimName:       "client-pvc",
		PVCMountPath:       "/data/shared",
		Table:              "cats_dogs",
		ServiceAccountName: "ingestor",
	}
}

// mustBuildStagePod wraps BuildStagePodSpec for tests that don't
// care about the (rare) crypto/rand failure path — failing the
// test if it ever fires is the right escape from "if err != nil"
// noise in every assertion. The dedicated TestBuildStagePodSpec_
// PropagatesRandError below covers the error path directly.
func mustBuildStagePod(t *testing.T, opts PodSpecOptions) *corev1.Pod {
	t.Helper()
	p, err := BuildStagePodSpec(opts)
	if err != nil {
		t.Fatalf("BuildStagePodSpec: %v", err)
	}
	return p
}

// TestDNS1123SafeTableSegment is the High-severity regression pin
// for the Bugbot finding on PR-b. Every realistic table name in
// tracebloc docs uses snake_case (cats_dogs_train, chest_xrays_train);
// without the transform these would produce Pod names K8s rejects
// post-pre-flight, which is the worst-of-both-worlds UX. The
// transform's job: take any ValidateTableName-passed name and
// produce a DNS-1123 subdomain-safe segment for the Pod name.
func TestDNS1123SafeTableSegment(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Canonical happy path: snake_case → kebab-case.
		{"cats_dogs", "cats-dogs"},
		{"chest_xrays_train", "chest-xrays-train"},
		// Uppercase: must lowercase.
		{"MyTable", "mytable"},
		{"ABC", "abc"},
		// Already lowercase no-underscore: identity.
		{"single", "single"},
		// Mixed: lowercase + underscore → hyphen.
		{"Table_123", "table-123"},
		// Edge: leading underscore must be stripped (would
		// otherwise produce "-leading", and while the full Pod
		// name `tracebloc-stage--leading-<hex>` is valid DNS-1123
		// the consecutive hyphens are ugly).
		{"_leading_underscore", "leading-underscore"},
		// Edge: trailing underscore stripped.
		{"trailing_", "trailing"},
		// Edge: digit-led is valid (DNS-1123 allows it).
		{"9starts_with_digit", "9starts-with-digit"},
		// Truncation: input > 30 chars caps at 30, then trims
		// any trailing hyphen the cut might have left.
		{strings.Repeat("a", 50), strings.Repeat("a", 30)},
		// Pathological: all-underscores → fallback "tbl".
		{"_", "tbl"},
		{"___", "tbl"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := dns1123SafeTableSegment(c.in)
			if got != c.want {
				t.Errorf("dns1123SafeTableSegment(%q) = %q, want %q", c.in, got, c.want)
			}
			// Defensive: the result must satisfy DNS-1123
			// subdomain rules for a Pod-name segment. Cheap
			// regex check covers the contract.
			if !isDNS1123SafeSegment(got) {
				t.Errorf("dns1123SafeTableSegment(%q) = %q, which violates DNS-1123",
					c.in, got)
			}
		})
	}
}

// isDNS1123SafeSegment is a test-only helper: returns true iff s
// matches the DNS-1123 subdomain segment regex (lowercase alnum +
// hyphen, must start AND end with alnum). Matches what the K8s
// validation library checks for Pod names.
func isDNS1123SafeSegment(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
			// Hyphen forbidden at boundaries.
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// TestBuildStagePodSpec_Defaults pins the spec fields PR-b's
// post-create logic depends on: the SA name, the PVC mount, the
// activeDeadline, and the labels orphan.go keys off.
func TestBuildStagePodSpec_Defaults(t *testing.T) {
	p := mustBuildStagePod(t, stageOpts())

	if p.Namespace != "tracebloc" {
		t.Errorf("Namespace = %q, want tracebloc", p.Namespace)
	}
	// Pod name uses the DNS-1123-safe transform of the table name:
	// "cats_dogs" → "cats-dogs". Bugbot flagged the raw-name version
	// on PR-b as High severity (snake_case is the canonical naming
	// style in tracebloc docs but K8s Pod names reject underscores).
	if !strings.HasPrefix(p.Name, "tracebloc-stage-cats-dogs-") {
		t.Errorf("Name = %q, want prefix tracebloc-stage-cats-dogs-", p.Name)
	}
	if got, wantLen := len(p.Name), len("tracebloc-stage-cats-dogs-")+8; got != wantLen {
		t.Errorf("len(Name) = %d, want %d (8-hex-char random suffix)", got, wantLen)
	}
	if p.Spec.ServiceAccountName != "ingestor" {
		t.Errorf("ServiceAccountName = %q, want ingestor", p.Spec.ServiceAccountName)
	}
	if p.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", p.Spec.RestartPolicy)
	}
	if p.Spec.ActiveDeadlineSeconds == nil || *p.Spec.ActiveDeadlineSeconds != int64(StagePodActiveDeadline) {
		t.Errorf("ActiveDeadlineSeconds = %v, want %d", p.Spec.ActiveDeadlineSeconds, StagePodActiveDeadline)
	}
}

// TestStagePodActiveDeadline_CoversFullLifecycle pins the floor on
// activeDeadlineSeconds. The earlier 600s value was too tight: the
// timer starts at Pod CREATION, but image pull (up to 60s) +
// readiness wait (up to 60s) + the worst-case stream (1 GiB at
// 2 MB/s = ~8.5 min) leaves no margin, so the kubelet could
// terminate near-cap pushes mid-transfer. Bugbot flagged as High
// on PR-b round 6. This test pins the floor at 1500s (25 min) so
// a future regression bumping it back near 600 gets caught.
func TestStagePodActiveDeadline_CoversFullLifecycle(t *testing.T) {
	const minDeadline = 1500
	if StagePodActiveDeadline < minDeadline {
		t.Errorf("StagePodActiveDeadline = %d, want at least %d "+
			"(must cover image-pull + readiness + worst-case 1 GiB stream "+
			"+ margin; activeDeadline starts at Pod creation not stream start)",
			StagePodActiveDeadline, minDeadline)
	}
}

// TestBuildStagePodSpec_DefaultImage pins the digest-pinned image —
// if this ever drifts to a tag-only reference, air-gapped customers
// would silently get whatever alpine:3.20 resolved to that day.
func TestBuildStagePodSpec_DefaultImage(t *testing.T) {
	p := mustBuildStagePod(t, stageOpts())
	got := p.Spec.Containers[0].Image
	if got != DefaultStagePodImage {
		t.Errorf("Image = %q, want %q (default)", got, DefaultStagePodImage)
	}
	if !strings.Contains(got, "@sha256:") {
		t.Errorf("Image = %q, want digest-pinned (@sha256:...); air-gapped customers depend on this", got)
	}
}

// TestBuildStagePodSpec_OverrideImage pins the --stage-pod-image
// flag's contract end-to-end at the spec layer.
func TestBuildStagePodSpec_OverrideImage(t *testing.T) {
	opts := stageOpts()
	opts.Image = "internal-mirror.example.com/alpine:3.20@sha256:abc123"
	p := mustBuildStagePod(t, opts)
	if got := p.Spec.Containers[0].Image; got != opts.Image {
		t.Errorf("Image = %q, want override %q", got, opts.Image)
	}
}

// TestBuildStagePodSpec_LabelsForOrphanScan pins the labels orphan.go
// will key off. If these ever drift, orphan-pod detection silently
// misses leftover Pods from crashed pushes.
func TestBuildStagePodSpec_LabelsForOrphanScan(t *testing.T) {
	p := mustBuildStagePod(t, stageOpts())
	wantLabels := map[string]string{
		StagePodManagedByLabel: StagePodManagedByValue,
		StagePodComponentLabel: StagePodComponentValue,
		StagePodTableLabel:     "cats_dogs",
	}
	for k, want := range wantLabels {
		if got := p.Labels[k]; got != want {
			t.Errorf("Label %s = %q, want %q", k, got, want)
		}
	}
}

// TestBuildStagePodSpec_RestrictedPSA is the security-regression
// pin: every PSA-restricted requirement must hold, otherwise the
// stage Pod gets rejected on hardened namespaces (which is the
// majority of production tracebloc deployments).
func TestBuildStagePodSpec_RestrictedPSA(t *testing.T) {
	p := mustBuildStagePod(t, stageOpts())

	// Pod-level: runAsNonRoot, seccomp RuntimeDefault.
	psc := p.Spec.SecurityContext
	if psc == nil {
		t.Fatal("Pod has no SecurityContext")
	}
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Errorf("Pod.SecurityContext.RunAsNonRoot = %v, want true", psc.RunAsNonRoot)
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("Pod.SeccompProfile = %v, want RuntimeDefault", psc.SeccompProfile)
	}

	// Container-level: every PSA restricted constraint.
	if len(p.Spec.Containers) != 1 {
		t.Fatalf("len(Containers) = %d, want 1", len(p.Spec.Containers))
	}
	c := p.Spec.Containers[0]
	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("Container has no SecurityContext")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("AllowPrivilegeEscalation = %v, want false", sc.AllowPrivilegeEscalation)
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Errorf("Privileged = %v, want false", sc.Privileged)
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("ReadOnlyRootFilesystem = %v, want true", sc.ReadOnlyRootFilesystem)
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 {
		t.Fatalf("Capabilities.Drop = %v, want [ALL]", sc.Capabilities)
	}
	if sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("Capabilities.Drop = %v, want [ALL]", sc.Capabilities.Drop)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("Container SeccompProfile = %v, want RuntimeDefault", sc.SeccompProfile)
	}
}

// TestBuildStagePodSpec_PVCMount pins the volume + mountPath that
// the tar stream writes into. If the mount path here ever drifts
// from cluster.SharedPVCMountPath, the tar would write to the
// wrong location and Phase 4's ingestor Job would see "no files."
func TestBuildStagePodSpec_PVCMount(t *testing.T) {
	p := mustBuildStagePod(t, stageOpts())

	// Volume side: the PVC reference.
	var foundVol bool
	for _, v := range p.Spec.Volumes {
		if v.Name == "shared" {
			foundVol = true
			if v.PersistentVolumeClaim == nil {
				t.Errorf("shared volume has no PVC source")
			} else if v.PersistentVolumeClaim.ClaimName != "client-pvc" {
				t.Errorf("PVC ClaimName = %q, want client-pvc", v.PersistentVolumeClaim.ClaimName)
			}
		}
	}
	if !foundVol {
		t.Error("Pod has no shared volume")
	}

	// Mount side: where the container sees it.
	var foundMount bool
	for _, m := range p.Spec.Containers[0].VolumeMounts {
		if m.Name == "shared" {
			foundMount = true
			if m.MountPath != "/data/shared" {
				t.Errorf("MountPath = %q, want /data/shared", m.MountPath)
			}
		}
	}
	if !foundMount {
		t.Error("stage container has no shared mount")
	}
}

// TestBuildStagePodSpec_TmpEmptyDir pins the writable /tmp emptyDir
// that tar needs (since the root FS is read-only). Without this,
// tar would fail to create its working state and the stream would
// die mid-transfer.
func TestBuildStagePodSpec_TmpEmptyDir(t *testing.T) {
	p := mustBuildStagePod(t, stageOpts())
	var foundEmptyDir, foundMount bool
	for _, v := range p.Spec.Volumes {
		if v.Name == "tmp" {
			foundEmptyDir = true
			if v.EmptyDir == nil {
				t.Error("tmp volume is not an EmptyDir")
			}
		}
	}
	for _, m := range p.Spec.Containers[0].VolumeMounts {
		if m.Name == "tmp" && m.MountPath == "/tmp" {
			foundMount = true
		}
	}
	if !foundEmptyDir || !foundMount {
		t.Errorf("tmp emptyDir mount missing (vol=%v, mount=%v)", foundEmptyDir, foundMount)
	}
}

// TestBuildStagePodSpec_RandomSuffixCollisionAvoidance: two specs
// built back-to-back must have distinct names so parallel pushes
// don't race on Create.
func TestBuildStagePodSpec_RandomSuffixCollisionAvoidance(t *testing.T) {
	a := mustBuildStagePod(t, stageOpts())
	b := mustBuildStagePod(t, stageOpts())
	if a.Name == b.Name {
		t.Errorf("back-to-back BuildStagePodSpec produced identical name %q; "+
			"random-suffix collision avoidance is broken", a.Name)
	}
}

// TestCreateStagePod_HappyPath: the fake clientset accepts a Create
// and returns the created object. We pin that CreateStagePod
// surfaces the assigned name back to the caller.
func TestCreateStagePod_HappyPath(t *testing.T) {
	cs := fake.NewClientset()
	name, err := CreateStagePod(context.Background(), cs, stageOpts())
	if err != nil {
		t.Fatalf("CreateStagePod: %v", err)
	}
	if !strings.HasPrefix(name, "tracebloc-stage-cats-dogs-") {
		t.Errorf("returned name = %q, want prefix tracebloc-stage-cats-dogs-", name)
	}
	// Cross-check: the Pod actually exists in the fake cluster.
	if _, err := cs.CoreV1().Pods("tracebloc").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Errorf("Pod not found after CreateStagePod: %v", err)
	}
}

// TestCreateStagePod_APIErrorSurfaces: PSA rejections, RBAC denials,
// etc. surface verbatim so the customer sees the actionable cluster-
// side message.
func TestCreateStagePod_APIErrorSurfaces(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "pods",
		func(_ k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				corev1.Resource("pods"), "",
				errors.New("user cannot create pods"))
		})

	_, err := CreateStagePod(context.Background(), cs, stageOpts())
	if err == nil {
		t.Fatal("CreateStagePod returned nil error on forbidden Create")
	}
	if !strings.Contains(err.Error(), "creating stage Pod") {
		t.Errorf("error missing CLI framing: %v", err)
	}
}

// TestWaitForStagePodReady_HappyPath: a Pod that immediately reports
// Ready=True passes through.
func TestWaitForStagePodReady_HappyPath(t *testing.T) {
	cs := fake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tracebloc-stage-cats_dogs-abc12345",
			Namespace: "tracebloc",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p, err := WaitForStagePodReady(ctx, cs, "tracebloc", "tracebloc-stage-cats_dogs-abc12345")
	if err != nil {
		t.Fatalf("WaitForStagePodReady: %v", err)
	}
	if p == nil {
		t.Fatal("returned nil Pod on success")
	}
}

// TestWaitForStagePodReady_TimeoutHint: when the Pod is stuck (e.g.
// ImagePullBackOff), the timeout error should surface the waiting-
// state reason from container statuses. This is the dominant slow
// path — air-gapped customers without the right pull secret.
func TestWaitForStagePodReady_TimeoutHint(t *testing.T) {
	cs := fake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stuck-pod",
			Namespace: "tracebloc",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "stage",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "ImagePullBackOff",
						Message: "Back-off pulling image \"alpine:3.20@sha256:...\"",
					},
				},
			}},
		},
	})

	// Tight ctx timeout so the test doesn't actually wait 60s.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := WaitForStagePodReady(ctx, cs, "tracebloc", "stuck-pod")
	if err == nil {
		t.Fatal("WaitForStagePodReady returned nil on stuck Pod")
	}
	if !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Errorf("error missing ImagePullBackOff hint: %v", err)
	}
}

// TestWaitForStagePodReady_NotFoundIsTerminal: if the Pod gets
// deleted out-of-band mid-wait (admin cleanup, parallel test
// teardown, etc.), the poll must short-circuit instead of waiting
// the full timeout. Bugbot flagged the prior "everything transient"
// behavior as Medium on PR-b.
func TestWaitForStagePodReady_NotFoundIsTerminal(t *testing.T) {
	cs := fake.NewClientset() // empty — Get returns NotFound

	// Generous overall ctx; the test should return WAY before
	// StagePodReadyTimeout (60s) because NotFound terminates the
	// poll immediately. If the fix regresses, this test takes the
	// full 60s and gets caught by go test's -timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := WaitForStagePodReady(ctx, cs, "tracebloc", "ghost-pod")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WaitForStagePodReady returned nil on NotFound")
	}
	if elapsed > 3*time.Second {
		t.Errorf("WaitForStagePodReady waited %s for NotFound; expected immediate return", elapsed)
	}
}

// TestWaitForStagePodReady_ForbiddenIsTerminal: same as NotFound but
// for RBAC denial — the customer's kubeconfig might lose `get pods`
// permission mid-push (token rotation, RBAC change). The poll must
// surface that immediately, not spin until timeout.
func TestWaitForStagePodReady_ForbiddenIsTerminal(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("get", "pods",
		func(_ k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				corev1.Resource("pods"), "test-pod",
				errors.New("user cannot get pods"))
		})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := WaitForStagePodReady(ctx, cs, "tracebloc", "test-pod")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WaitForStagePodReady returned nil on Forbidden")
	}
	if elapsed > 3*time.Second {
		t.Errorf("WaitForStagePodReady waited %s for Forbidden; expected immediate return", elapsed)
	}
}

// TestWaitForStagePodReady_FailedPhaseIsTerminal: a Pod that
// crashes at startup (PSA rejection, ImagePullBackOff that escalates
// to ErrImagePull, OOMKilled during container start) lands in
// Phase=Failed and will never become Ready. The poll must
// short-circuit instead of waiting the full 60s. Bugbot flagged
// the missing check as Medium on PR-b round 4.
func TestWaitForStagePodReady_FailedPhaseIsTerminal(t *testing.T) {
	cs := fake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crashed-pod",
			Namespace: "tracebloc",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "stage",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Reason:   "OOMKilled",
						ExitCode: 137,
					},
				},
			}},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := WaitForStagePodReady(ctx, cs, "tracebloc", "crashed-pod")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WaitForStagePodReady returned nil on Phase=Failed")
	}
	if elapsed > 3*time.Second {
		t.Errorf("WaitForStagePodReady waited %s for Phase=Failed; expected immediate return", elapsed)
	}
	if !strings.Contains(err.Error(), "Failed") {
		t.Errorf("error missing Phase=Failed signal: %v", err)
	}
}

// TestDeleteStagePod_NotFoundIsOK: the Pod might be gone already
// (activeDeadlineSeconds fired, or someone kubectl-deleted it).
// Not-found shouldn't error — our goal is "Pod doesn't exist," which
// is satisfied.
func TestDeleteStagePod_NotFoundIsOK(t *testing.T) {
	cs := fake.NewClientset() // empty
	if err := DeleteStagePod(context.Background(), cs, "tracebloc", "nope"); err != nil {
		t.Errorf("DeleteStagePod on absent Pod = %v, want nil", err)
	}
}

// TestDeleteStagePod_HappyPath: delete a real Pod, confirm it's gone.
func TestDeleteStagePod_HappyPath(t *testing.T) {
	cs := fake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "to-delete", Namespace: "tracebloc"},
	})
	if err := DeleteStagePod(context.Background(), cs, "tracebloc", "to-delete"); err != nil {
		t.Fatalf("DeleteStagePod: %v", err)
	}
	_, err := cs.CoreV1().Pods("tracebloc").Get(context.Background(), "to-delete", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Pod still exists after delete (err=%v)", err)
	}
}
