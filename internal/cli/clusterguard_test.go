package cli

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

// ---------------------------------------------------------------------------
// The property under test (backend#2863): invoked with NO flags, a mutating
// command acts on the cluster the secure environment runs on — or refuses. The
// bug was that it acted on whatever the ambient current-context pointed at.
//
// Every test here constructs the WRONG-CLUSTER case explicitly. The pre-fix code
// had no notion of a recorded cluster at all, so each of these fails on it: not
// by a message change, but by the command proceeding.
// ---------------------------------------------------------------------------

// kubeSystem returns a kube-system namespace object carrying uid, which is the
// anchor cluster.ClusterID reads (the same one the backend client record keys on).
func kubeSystem(uid string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(uid)},
	}
}

// withAnchor records uid as this machine's cluster in the temp config dir. The
// caller must already have a config dir (withClientBackend / t.Setenv).
func withAnchor(t *testing.T, uid string) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Current() hands back a THROWAWAY &Profile{} when no env is selected, so a
	// write to it is silently discarded — select one first or the anchor never
	// lands and every refusal test below passes for the wrong reason.
	if cfg.CurrentEnv == "" {
		cfg.CurrentEnv = "test"
	}
	cfg.Current().ActiveClientClusterID = uid
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Current().ActiveClientClusterID; got != uid {
		t.Fatalf("the anchor did not persist (got %q, want %q) — the tests using it"+
			" would pass by accident", got, uid)
	}
}

// withClusterID fakes the identity read for the resolveClusterTarget path.
func withClusterID(t *testing.T, id string, err error) {
	t.Helper()
	orig := clusterIDFromFn
	t.Cleanup(func() { clusterIDFromFn = orig })
	clusterIDFromFn = func(context.Context, kubernetes.Interface) (string, error) {
		return id, err
	}
}

// A mutating resolve against a cluster that is NOT the recorded one refuses, and
// refuses with the two ids named — a message that says only "wrong cluster" makes
// the user guess which of their contexts was right.
func TestResolveClusterTarget_Mutating_WrongCluster_Refuses(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withAnchor(t, "AAAAAAAA-1111-2222-3333-444444444444")
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01"), kubeSystem("BBBBBBBB-9999-8888-7777-666666666666")))
	withClusterID(t, "BBBBBBBB-9999-8888-7777-666666666666", nil)

	withAccountClients(t, nil, errors.New("offline (test): no backend"))

	var out bytes.Buffer
	_, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true, false)
	if err == nil {
		t.Fatal("a mutating command must refuse a cluster that is not the recorded one")
	}
	if got := ExitCodeFromError(err); got != exitLocalEnv {
		t.Errorf("exit code = %d, want %d (a local-environment problem)", got, exitLocalEnv)
	}
	msg := err.Error()
	for _, want := range []string{
		"not the cluster your secure environment runs on",
		"BBBBBBBB", // reached — so the user can see WHERE it went
		"AAAAAAAA", // expected — so they can see which context is right
		"--context/--kubeconfig",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must name %q; got:\n%s", want, msg)
		}
	}
}

// The same resolve with mutates=false PROCEEDS. Being wrong about which cluster
// you are READING is a confusing answer, not a destructive act — and gating reads
// too would break `data list` on a machine whose anchor predates the field.
func TestResolveClusterTarget_ReadOnly_WrongCluster_Proceeds(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withAnchor(t, "AAAAAAAA-1111-2222-3333-444444444444")
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01"), kubeSystem("BBBBBBBB")))
	withClusterID(t, "BBBBBBBB", nil)

	var out bytes.Buffer
	if _, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, false, false); err != nil {
		t.Fatalf("a read-only command must not be gated on cluster identity: %v", err)
	}
}

// Matching cluster → proceeds. Without this the refusal above could be satisfied
// by a guard that refuses everything, which would pass the mismatch test and
// break every legitimate invocation.
func TestResolveClusterTarget_Mutating_RightCluster_Proceeds(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withAnchor(t, "SAME-CLUSTER-UID")
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01"), kubeSystem("SAME-CLUSTER-UID")))
	withClusterID(t, "SAME-CLUSTER-UID", nil)
	// API offline: the recorded anchor matching the live cluster is the positive
	// local evidence that must let the command through even when the API can't name it.
	withAccountClients(t, nil, errors.New("offline (test): no backend"))

	var out bytes.Buffer
	if _, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true, false); err != nil {
		t.Fatalf("the recorded cluster must be usable: %v", err)
	}
}

// An UNREADABLE identity refuses for a mutating command: we are about to write to
// a cluster we cannot name. Every caller already needs API access, so this costs
// nothing legitimate — and the alternative (proceed) is the bug with extra steps.
func TestResolveClusterTarget_Mutating_UnreadableID_Refuses(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withAnchor(t, "AAAAAAAA")
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01")))
	withClusterID(t, "", errors.New("namespaces \"kube-system\" is forbidden"))

	var out bytes.Buffer
	_, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true, false)
	if err == nil {
		t.Fatal("an unidentifiable cluster must not be written to")
	}
	if !strings.Contains(err.Error(), "Refusing rather than writing to an unidentified cluster") {
		t.Errorf("the refusal must say why it refused; got: %v", err)
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("the underlying cause must survive for diagnosis; got: %v", err)
	}
}

// The security asymmetry, pinned as a mutation-proof (Lukas' review): the whole
// safety story rests on "--i-know-the-target overrides an UNKNOWN target, never a
// known-WRONG one." So a wrong-cluster mismatch must STILL refuse even with the ack
// flag set. If a future edit "for symmetry" made the mismatch branch consult
// ackTarget, this reddens — the flag would otherwise become a wrong-cluster bypass.
func TestResolveClusterTarget_Mutating_WrongCluster_AckDoesNotOverride(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withAnchor(t, "AAAAAAAA-1111-2222-3333-444444444444")
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01"), kubeSystem("BBBBBBBB-9999-8888-7777-666666666666")))
	withClusterID(t, "BBBBBBBB-9999-8888-7777-666666666666", nil)
	withAccountClients(t, nil, errors.New("offline (test): no backend"))

	var out bytes.Buffer
	_, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true, true /* --i-know-the-target */)
	if err == nil {
		t.Fatal("--i-know-the-target must NOT override a KNOWN-WRONG cluster — only an unknown one")
	}
	if !strings.Contains(err.Error(), "not the cluster your secure environment runs on") {
		t.Errorf("the mismatch refusal must stand even with the ack flag; got: %v", err)
	}
}

// The unreadable-ID + ack branch (Lukas' review #2): with --i-know-the-target, a
// cluster whose identity can't even be read proceeds with a warning rather than
// refusing — the ack overrides the one unverifiable case where there is nothing
// left to check.
func TestResolveClusterTarget_Mutating_UnreadableID_AckProceeds(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01")))
	withClusterID(t, "", errors.New("namespaces \"kube-system\" is forbidden"))

	var out bytes.Buffer
	if _, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true, true /* --i-know-the-target */); err != nil {
		t.Fatalf("--i-know-the-target must let an unreadable-identity cluster through: %v", err)
	}
	if got := out.String(); !strings.Contains(got, knowTargetFlag) {
		t.Errorf("proceeding on an unreadable identity must say why (named --%s); got:\n%s", knowTargetFlag, got)
	}
}

// A 426 (this CLI is too old for the server) must HARD-STOP, never be swallowed as
// "couldn't confirm" and then wave the mutation through on a matching local anchor
// (Bugbot / learned rule). The anchor here MATCHES the live cluster, so without the
// hard-stop the guard would proceed via the offline anchor-match path; it must
// instead refuse and point at `tracebloc upgrade`, and --i-know-the-target must not
// override it (upgrading is the only way through).
func TestResolveClusterTarget_Mutating_UpgradeRequired_HardStops(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withAnchor(t, "MATCHING-UID") // would otherwise proceed via anchor-match (3b)
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01"), kubeSystem("MATCHING-UID")))
	withClusterID(t, "MATCHING-UID", nil)
	withAccountClients(t, nil, &api.UpgradeRequiredError{MinVersion: "0.11.0"})

	var out bytes.Buffer
	_, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true, true /* even with ack */)
	if err == nil {
		t.Fatal("a 426 must hard-stop the mutation, not be swallowed and proceed on the local anchor")
	}
	var ue *api.UpgradeRequiredError
	if !errors.As(err, &ue) {
		t.Errorf("the 426 must propagate as an UpgradeRequiredError so the exit says 'upgrade'; got: %v", err)
	}
	if !strings.Contains(err.Error(), "tracebloc upgrade") {
		t.Errorf("the refusal must point at the upgrade, not `client create`; got: %v", err)
	}
}

// A Ctrl-C DURING the backend verify (its context cancelled) must be a quiet
// interrupt (exit 130), NOT swallowed into reached=false — which, on a MATCHING
// recorded anchor, would authorize the mutation the operator just aborted (Bugbot),
// or refuse with a misleading "couldn't reach / run login" at exit 3.
func TestResolveClusterTarget_Mutating_CtrlCDuringVerify_Interrupts(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withAnchor(t, "MATCHING-UID") // matches → without the check, 3b would PROCEED
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01"), kubeSystem("MATCHING-UID")))
	withClusterID(t, "MATCHING-UID", nil)
	// The lookup returns a cancellation error, mirroring an aborted in-flight request.
	withAccountClients(t, nil, context.Canceled)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the operator hit Ctrl-C

	var out bytes.Buffer
	_, err := resolveClusterTarget(ctx, ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true, false)
	if err == nil {
		t.Fatal("a Ctrl-C during verify must abort, not authorize the mutation on a matching anchor")
	}
	if got := ExitCodeFromError(err); got != exitInterrupted {
		t.Errorf("exit code = %d, want %d (quiet Ctrl-C), not a refusal/proceed; err: %v", got, exitInterrupted, err)
	}
	// exitInterrupted is a SILENT exit — main() must print nothing (no bare
	// "Error: context canceled"), same as every other interrupt path.
	if !IsSilentError(err) {
		t.Errorf("a Ctrl-C exit must be silent (no inner err), else main() prints 'Error: context canceled'; err: %v", err)
	}
	if strings.Contains(out.String(), "matches this machine's recorded cluster") {
		t.Errorf("the anchor-match proceed must NOT fire after an abort; got:\n%s", out.String())
	}
}

// withAccountClients stubs the backend clients lookup the target verifier
// (verifyTargetFromAPI) makes, so a guard test can drive "the API names this
// cluster" / "the API can't confirm" without standing up a server. An error models
// offline / not-signed-in / a backend failure.
func withAccountClients(t *testing.T, clients []api.ProvisionedClient, err error) {
	t.Helper()
	orig := listAccountClientsFn
	t.Cleanup(func() { listAccountClientsFn = orig })
	listAccountClientsFn = func(context.Context) ([]api.ProvisionedClient, error) {
		return clients, err
	}
}

// withActiveClientName records a display name (but NO cluster anchor) for the
// active client, so a test can prove the guard's printed identity comes from the
// API and not from this local cache.
func withActiveClientName(t *testing.T, name string) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentEnv == "" {
		cfg.CurrentEnv = "test"
	}
	cfg.Current().ActiveClientName = name
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

// backend#2983 direction 1: with NO local anchor — the exact field condition that
// fell open — the guard VERIFIES the target against the API and prints the fleet's
// identity (name + namespace), an identity that comes from the API record and not
// from local state. Here the local cache carries a DECOY name; the printed identity
// must be the API's, and the command proceeds.
func TestResolveClusterTarget_Mutating_VerifiesIdentityFromAPI(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withActiveClientName(t, "stale-local-decoy") // no ActiveClientClusterID → no anchor
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("hasan-prod"), kubeSystem("KUBE-UID-PROD")))
	withClusterID(t, "KUBE-UID-PROD", nil)
	withAccountClients(t, []api.ProvisionedClient{
		{ID: 9, Name: "other-box", Namespace: "other-ns", ClusterID: "SOME-OTHER-UID"},
		{ID: 7, Name: "hasan-prod", Namespace: "tracebloc-templates-prod", ClusterID: "KUBE-UID-PROD"},
	}, nil)

	var out bytes.Buffer
	if _, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true, false); err != nil {
		t.Fatalf("an API-verified target must proceed: %v", err)
	}
	got := out.String()
	for _, want := range []string{"hasan-prod", "tracebloc-templates-prod"} {
		if !strings.Contains(got, want) {
			t.Errorf("the printed identity must be the API's %q; got:\n%s", want, got)
		}
	}
	// The name came from the API record, not the local cache — the whole point of
	// backend#2983 direction 1 (local state is what was missing in the field).
	if strings.Contains(got, "stale-local-decoy") {
		t.Errorf("the identity must come from the API, not the local cache; got:\n%s", got)
	}
}

// backend#2983 direction 2, the API-UNREACHABLE case: no anchor, and the API can't
// be reached (offline / not signed in) — a scripted --no-input caller has no human
// to ask, so this fails closed rather than mutating a cluster identified only by a
// localhost port. And it must NOT assert an absence it never confirmed: a
// couldn't-reach refusal names sign-in / context, never `client create` on an
// unconfirmed cluster (the #515 adopt-vs-mint trap; Bugbot).
func TestResolveClusterTarget_Mutating_Unverifiable_Unreachable_FailsClosed(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01"), kubeSystem("KUBE-UID-X")))
	withClusterID(t, "KUBE-UID-X", nil)
	withAccountClients(t, nil, errors.New("dial tcp: connect: connection refused"))

	var out bytes.Buffer
	_, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true, false /* ack: as under --no-input */)
	if err == nil {
		t.Fatal("an unverifiable target must NOT proceed without --i-know-the-target — this is the backend#2983 fail-open")
	}
	if got := ExitCodeFromError(err); got != exitLocalEnv {
		t.Errorf("exit code = %d, want %d (a local-environment problem)", got, exitLocalEnv)
	}
	msg := err.Error()
	for _, want := range []string{"couldn't verify which cluster this is", "couldn't be reached", knowTargetFlag} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must name %q so it isn't a dead end; got:\n%s", want, msg)
		}
	}
	// Must NOT claim a confirmed absence we never confirmed, nor push `client create`
	// on an unconfirmed cluster (the #515 trap the fix closes).
	for _, forbidden := range []string{"has no client registered", "client create"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("a couldn't-REACH refusal must not assert absence / recommend %q; got:\n%s", forbidden, msg)
		}
	}
}

// The other half: the API WAS reached and answered, but no account client is
// anchored to this cluster's UID (here every record is anchored elsewhere; the
// legacy empty-ClusterID record is the same shape). This is NOT a confirmed absence
// — discovery already found a client release on the cluster — so the refusal must
// say the on-cluster client "isn't linked to any client in your account", NEVER
// "tracebloc has no client registered" (Bugbot). `client create` is the right fix
// because it adopts the on-cluster client and records the anchor.
func TestResolveClusterTarget_Mutating_ReachedButUnmatched_FailsClosed(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01"), kubeSystem("KUBE-UID-X")))
	withClusterID(t, "KUBE-UID-X", nil)
	// API answered, but no client is anchored to KUBE-UID-X (one anchored elsewhere,
	// one legacy with an empty ClusterID — neither can match by UID).
	withAccountClients(t, []api.ProvisionedClient{
		{ID: 3, Name: "someone-else", Namespace: "other", ClusterID: "A-DIFFERENT-UID"},
		{ID: 4, Name: "legacy-box", Namespace: "legacy", ClusterID: ""},
	}, nil)

	var out bytes.Buffer
	_, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true, false)
	if err == nil {
		t.Fatal("a cluster whose client isn't linked to the account, with no anchor, must fail closed")
	}
	if got := ExitCodeFromError(err); got != exitLocalEnv {
		t.Errorf("exit code = %d, want %d", got, exitLocalEnv)
	}
	msg := err.Error()
	for _, want := range []string{"isn't linked to any client in your account", "client create", knowTargetFlag} {
		if !strings.Contains(msg, want) {
			t.Errorf("a reached-but-unmatched refusal should name %q; got:\n%s", want, msg)
		}
	}
	// Must NOT assert an absence: discovery already found a client release here, and a
	// legacy empty-ClusterID client can never match by UID (Bugbot).
	if strings.Contains(msg, "has no client registered") {
		t.Errorf("must not assert absence when a client release IS on the cluster; got:\n%s", msg)
	}
}

// The escape hatch: --i-know-the-target lets an operator through an unverifiable
// target on purpose, with a warning naming what little is known. Without this the
// fail-closed above would be a wall for the legitimate air-gapped / not-yet-
// registered case.
func TestResolveClusterTarget_Mutating_Unverifiable_AckProceeds(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01"), kubeSystem("KUBE-UID-X")))
	withClusterID(t, "KUBE-UID-X", nil)
	withAccountClients(t, nil, errors.New("dial tcp: connect: connection refused"))

	var out bytes.Buffer
	if _, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true, true /* --i-know-the-target */); err != nil {
		t.Fatalf("--i-know-the-target must let an unverifiable target through: %v", err)
	}
	if got := out.String(); !strings.Contains(got, knowTargetFlag) {
		t.Errorf("proceeding on an unverified target must say why (it named --%s); got:\n%s", knowTargetFlag, got)
	}
}

// The backend#2983 scenario end-to-end at the command boundary: `data ingest`
// under --no-input (AckTarget=false), against a cluster the API can't confirm and
// with no recorded anchor, must FAIL CLOSED before staging anything — not warn and
// write, which is what a CI job hit in the field. connectIngestTarget is the money
// path's pre-flight; the guard fires inside it, before the PVC discovery and the
// destination-table read, so nothing is touched by the time it refuses.
func TestConnectIngestTarget_Unverifiable_NoInput_FailsClosed(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	// A reachable cluster (kube-system UID readable), hosting a client, but the API
	// can't say which fleet it is and this machine never recorded one.
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01"), kubeSystem("KUBE-UID-CI")))
	withAccountClients(t, nil, errors.New("dial tcp: connect: connection refused"))

	var out bytes.Buffer
	a := &runDataIngestArgs{Printer: ui.New(&out), AckTarget: false} // --no-input passes no ack
	target, _, cancelled, err := connectIngestTarget(context.Background(), a)
	if err == nil {
		t.Fatal("data ingest --no-input against an unverifiable target must fail closed, not stage")
	}
	if target != nil || cancelled {
		t.Errorf("nothing may be handed back on a fail-closed refusal (target=%v cancelled=%v)", target, cancelled)
	}
	if got := ExitCodeFromError(err); got != exitLocalEnv {
		t.Errorf("exit code = %d, want %d — a CI job must get a non-zero exit, not a green run", got, exitLocalEnv)
	}
	if !strings.Contains(err.Error(), "couldn't verify which cluster this is") {
		t.Errorf("the refusal must say the target couldn't be verified; got: %v", err)
	}
}

// The destructive twin the issue also called out: `data delete` drops a MySQL
// table and removes files on the shared PVC. An unverifiable target must refuse
// BEFORE the dataset list is even read — the teardown never gets a chance to run.
func TestRunDataDelete_Unverifiable_FailsClosedBeforeTeardown(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01"), kubeSystem("KUBE-UID-D")))
	withAccountClients(t, nil, errors.New("dial tcp: connect: connection refused"))
	// If anything past the guard runs, the delete is proceeding — fail loudly.
	origList := listDatasetsFn
	t.Cleanup(func() { listDatasetsFn = origList })
	listDatasetsFn = func(context.Context, kubernetes.Interface, *rest.Config, string) ([]string, error) {
		t.Fatal("the delete reached the dataset list — the guard did not fail closed before teardown")
		return nil, nil
	}

	var out bytes.Buffer
	// Yes:true is the scriptable delete; the guard must refuse regardless.
	err := runDataDelete(context.Background(), runDataDeleteArgs{
		Table: "probe", Printer: ui.New(&out), Yes: true, AckTarget: false,
	})
	if err == nil {
		t.Fatal("data delete against an unverifiable target must refuse, not drop the table")
	}
	if got := ExitCodeFromError(err); got != exitLocalEnv {
		t.Errorf("exit code = %d, want %d", got, exitLocalEnv)
	}
	if !strings.Contains(err.Error(), "couldn't verify which cluster this is") {
		t.Errorf("the refusal must say the target couldn't be verified; got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// `delete` — the one command where the mistake is unrecoverable, and the one
// that reported it in the field: it hands the raw kubeconfig flags to
// `helm uninstall` and never resolved a target at all.
// ---------------------------------------------------------------------------

// withDeleteClusterID fakes the delete path's own identity read.
func withDeleteClusterID(t *testing.T, id string, err error) {
	t.Helper()
	orig := clusterIDForDelete
	t.Cleanup(func() { clusterIDForDelete = orig })
	clusterIDForDelete = func(context.Context, cluster.KubeconfigOptions) (string, error) {
		return id, err
	}
}

// Offboarding against a cluster that is not this machine's refuses, and refuses
// BEFORE the credential is revoked and before any teardown step runs — so the
// machine is left exactly as it was and the command is re-runnable.
func TestDelete_WrongCluster_RefusesBeforeAnyChange(t *testing.T) {
	revoked := false
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/revoke") {
			revoked = true
		}
		_, _ = w.Write([]byte(`[]`))
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	withAnchor(t, "MY-OWN-CLUSTER")
	withDeleteClusterID(t, "SOMEONE-ELSES-CLUSTER", nil)
	fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
	fn.install(t)

	var out bytes.Buffer
	err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true})
	if err == nil {
		t.Fatal("offboarding the wrong cluster must be refused")
	}
	if !strings.Contains(err.Error(), "not the cluster your secure environment runs on") {
		t.Errorf("unexpected error: %v", err)
	}
	// The ways this command is destructive, none of which may have happened.
	if revoked {
		t.Error("the machine credential was revoked despite the refusal")
	}
	if len(fn.calls) != 0 {
		t.Errorf("teardown steps ran against the wrong cluster — the field bug: %v", fn.calls)
	}
}

// A cluster we cannot reach must NOT block offboarding: a dead cluster is the
// main reason to offboard, and blocking would leave the machine unremovable.
// This is the deliberate asymmetry against the mutating-data commands above.
func TestDelete_UnreachableCluster_StillOffboards(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	withAnchor(t, "MY-OWN-CLUSTER")
	withDeleteClusterID(t, "", errors.New("dial tcp 127.0.0.1:6443: connect: connection refused"))
	fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
	fn.install(t)

	var out bytes.Buffer
	if err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true}); err != nil {
		t.Fatalf("an unreachable cluster must not block offboarding: %v", err)
	}
}

// A machine with no recorded anchor still offboards — same reason as the resolve
// path: a config written before this field existed must not be stuck.
func TestDelete_NoAnchor_StillOffboards(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	// deliberately no withAnchor
	called := false
	orig := clusterIDForDelete
	t.Cleanup(func() { clusterIDForDelete = orig })
	clusterIDForDelete = func(context.Context, cluster.KubeconfigOptions) (string, error) {
		called = true
		return "ANY", nil
	}
	fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
	fn.install(t)

	var out bytes.Buffer
	if err := runDelete(context.Background(), ui.New(&out), nil, deleteOpts{yes: true}); err != nil {
		t.Fatalf("an unrecorded anchor must not block offboarding: %v", err)
	}
	if called {
		t.Error("with nothing to compare against, the identity read is pointless work" +
			" — and on an unreachable cluster it costs the read timeout")
	}
}

// Offboarding this machine clears the anchor along with the rest of the active
// client. A stale anchor would make the NEXT install's commands refuse against a
// cluster that is now legitimately theirs.
func TestDelete_ClearsTheAnchor(t *testing.T) {
	withClientBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	setActiveForDelete(t, "5", "gpu-box-01", "gpu-box-01")
	withAnchor(t, "MY-OWN-CLUSTER")
	withDeleteClusterID(t, "MY-OWN-CLUSTER", nil)
	fn := &fakeNodeboot{executable: filepath.Join(t.TempDir(), "tracebloc")}
	fn.install(t)

	var out bytes.Buffer
	if err := runDelete(context.Background(), ui.New(&out), nil,
		deleteOpts{yes: true, keepData: true}); err != nil { // keepData: config survives to be read
		t.Fatalf("offboard failed: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Current().ActiveClientClusterID; got != "" {
		t.Errorf("the cluster anchor survived the offboard as %q — the next install"+
			" on this machine would refuse its own cluster", got)
	}
}

// ---------------------------------------------------------------------------
// Anti-rot: the decision must be forced on every cluster-touching call site,
// including ones added later. This does NOT restate the list of commands — it
// DERIVES the call sites from the source and requires each to carry a recorded
// intent, so a new `resolveClusterTarget` call reddens until someone decides.
// ---------------------------------------------------------------------------

// mutationIntent records, per production call site, whether that command mutates
// the cluster. Adding a call site without adding a row here fails the test below;
// so does removing one. The VALUES are the decision under review — a reviewer
// reads this map and asks "does this command write to the cluster?".
var mutationIntent = map[string]bool{
	"seal.go":                true,  // rolls the jobs-manager with a sealed envelope
	"data_delete.go":         true,  // drops a table, removes files from the shared PVC
	"resources_set.go":       true,  // rewrites the resource envelope and restarts
	"data_ingest_cluster.go": true,  // stages a private dataset onto the cluster
	"data_list.go":           false, // reads the catalog
	"resources.go":           false, // prints the current envelope
}

func TestEveryClusterCallSiteDeclaresMutationIntent(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "clustertarget.go" {
			continue // the definition itself, and the tests, are not call sites
		}
		fset := token.NewFileSet()
		af, perr := parser.ParseFile(fset, f, nil, 0)
		if perr != nil {
			// Fail closed: an unparseable file is "cannot tell", not "agrees".
			t.Fatalf("parsing %s: %v", f, perr)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || (id.Name != "resolveClusterTarget" && id.Name != "resolveClusterTargetFn") {
				return true
			}
			if len(call.Args) != 8 {
				t.Errorf("%s: resolveClusterTarget called with %d args, want 8 — the"+
					" mutates parameter is what forces the decision", f, len(call.Args))
				return true
			}
			lit, ok := call.Args[6].(*ast.Ident)
			if !ok || (lit.Name != "true" && lit.Name != "false") {
				t.Errorf("%s: mutates must be a literal true/false at the call site, so a"+
					" reader sees the decision without tracing a variable", f)
				return true
			}
			want, declared := mutationIntent[f]
			if !declared {
				t.Errorf("%s calls resolveClusterTarget but has no row in mutationIntent."+
					" Decide whether it writes to the cluster and add one — a command that"+
					" mutates without the guard is backend#2863 all over again.", f)
				return true
			}
			found[f] = true
			if got := lit.Name == "true"; got != want {
				t.Errorf("%s passes mutates=%v, but mutationIntent says %v", f, got, want)
			}
			return true
		})
	}
	for f := range mutationIntent {
		if !found[f] {
			t.Errorf("mutationIntent has a row for %s, which no longer calls"+
				" resolveClusterTarget — stale rows hide a removed guard", f)
		}
	}
}

// `delete` does not go through resolveClusterTarget (it shells out to helm), so
// the sweep above cannot see it. Assert its guard by behavior, not by grepping:
// the wrong-cluster test above is the real check; this one only pins that the
// seam exists so the guard cannot be quietly dropped while its tests keep
// passing against a no-op.
func TestDeleteGuardUsesTheRealIdentityRead(t *testing.T) {
	// A test seam that defaults to something OTHER than the production function
	// would make every delete test above vacuous.
	if clusterIDForDelete == nil {
		t.Fatal("clusterIDForDelete is nil — delete's identity guard cannot fire")
	}
	got, err := clusterIDForDelete(context.Background(), cluster.KubeconfigOptions{
		Path: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	// The real cluster.ClusterID must FAIL on a nonexistent kubeconfig. A stub
	// that returns a value would pass the tests above while guarding nothing.
	if err == nil {
		t.Errorf("the default clusterIDForDelete returned %q for a nonexistent"+
			" kubeconfig — it is not the real identity read", got)
	}
}

// The anchor is only as good as its recording. setActiveClient must capture the
// cluster alongside the namespace — an install that records the namespace but not
// the cluster leaves the guard permanently in warn-and-proceed, which looks
// identical to a working guard in every log.
func TestSetActiveClient_RecordsTheClusterAnchor(t *testing.T) {
	var p config.Profile
	setActiveClient(&p, &api.ProvisionedClient{
		ID: 7, Name: "gpu-box-01", Namespace: "gpu-box-01",
		ClusterID: "KUBE-SYSTEM-UID-7",
	})
	if p.ActiveClientClusterID != "KUBE-SYSTEM-UID-7" {
		t.Errorf("ActiveClientClusterID = %q, want the client's ClusterID —"+
			" without it every mutating command degrades to unverified", p.ActiveClientClusterID)
	}
}

// A client provisioned WITHOUT an anchor (the degraded create path, when the
// cluster was unreachable at create time) records an empty anchor rather than a
// wrong one. Empty means "unverified, warn"; a fabricated value would mean
// "verified" and refuse the user's own cluster forever.
func TestSetActiveClient_NoBackendAnchor_StaysEmpty(t *testing.T) {
	var p config.Profile
	setActiveClient(&p, &api.ProvisionedClient{ID: 7, Name: "x", Namespace: "x"})
	if p.ActiveClientClusterID != "" {
		t.Errorf("ActiveClientClusterID = %q, want empty for an unanchored client",
			p.ActiveClientClusterID)
	}
}

// There must be exactly ONE place that makes a client active. The guard is silent
// when the anchor is missing (deliberately — see clusterguard.go), so a SECOND
// write path that set the id and namespace without the cluster would disable the
// guard for anyone who took that path, and nothing would say so. Derived from the
// source, so a new path reddens here rather than being noticed in the field.
func TestActiveClientHasOneWritePath(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var writers []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		af, perr := parser.ParseFile(fset, f, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", f, perr) // fail closed
		}
		ast.Inspect(af, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "ActiveClientID" {
					continue
				}
				// delete.go's clear-on-offboard is a teardown, not an activation.
				if f == "delete.go" {
					continue
				}
				writers = append(writers, f+":"+fset.Position(as.Pos()).String())
			}
			return true
		})
	}
	if len(writers) != 1 {
		t.Errorf("ActiveClientID is assigned in %d places outside delete.go: %v\n"+
			"Only setActiveClient may activate a client, because it is what records the"+
			" cluster anchor. A second path silently disables the backend#2863 guard.",
			len(writers), writers)
	}
	if len(writers) == 1 && !strings.HasPrefix(writers[0], "client.go:") {
		t.Errorf("the single write path moved to %s — confirm it still records"+
			" ActiveClientClusterID", writers[0])
	}
}
