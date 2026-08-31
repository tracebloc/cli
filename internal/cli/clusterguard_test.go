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

	var out bytes.Buffer
	_, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true)
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
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, false); err != nil {
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

	var out bytes.Buffer
	if _, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true); err != nil {
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
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true)
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

// NO anchor recorded → warn and proceed. Configs written before this field
// existed must not be locked out of their own commands, and the warning has to
// name the way to fix it or it is just noise.
func TestResolveClusterTarget_Mutating_NoAnchor_WarnsAndProceeds(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	withClusterSeams(t, fake.NewSimpleClientset(jmDep("gpu-box-01")))
	withClusterID(t, "WHATEVER", nil)

	var out bytes.Buffer
	if _, err := resolveClusterTarget(context.Background(), ui.New(&out),
		cluster.KubeconfigOptions{}, activeClientBinding{}, false, false, true); err != nil {
		t.Fatalf("an unrecorded anchor must not block: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "hasn't recorded which cluster") {
		t.Errorf("an unverified mutation must say so; got:\n%s", got)
	}
	if !strings.Contains(got, "client create") {
		t.Errorf("the warning must name how to record it; got:\n%s", got)
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
			if len(call.Args) != 7 {
				t.Errorf("%s: resolveClusterTarget called with %d args, want 7 — the"+
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
