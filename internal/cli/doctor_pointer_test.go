package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/doctor"
	"github.com/tracebloc/cli/internal/ui"
)

// #515 — a WRONG active-client pointer must not read as "no environment".
// Split from doctor_test.go, which is already at its file budget.

// stubDoctorForNamespace makes doctor's cluster I/O deterministic: the kubeconfig
// resolves to a caller-chosen server URL with the namespace opts asked for (or
// kubeconfigNS when nothing was pinned — exactly how cluster.Load layers an
// explicit namespace over the context's own), and the probe reports a healthy
// environment in envNS and ReachNoEnv anywhere else. It returns the list of
// namespaces the probe ran against, so a test can assert what was and wasn't
// re-probed rather than inferring it from the rendered text.
func stubDoctorForNamespace(t *testing.T, serverURL, kubeconfigNS, envNS string) *[]string {
	t.Helper()
	origLoad, origCS, origRun := loadClusterFn, newClientsetFn, doctorRunFn
	t.Cleanup(func() { loadClusterFn, newClientsetFn, doctorRunFn = origLoad, origCS, origRun })

	loadClusterFn = func(o cluster.KubeconfigOptions) (*cluster.ResolvedConfig, error) {
		ns := o.Namespace
		if ns == "" {
			ns = kubeconfigNS
		}
		return &cluster.ResolvedConfig{
			Namespace: ns, Context: "test-ctx", ServerURL: serverURL, RestConfig: &rest.Config{},
		}, nil
	}
	newClientsetFn = func(*cluster.ResolvedConfig) (kubernetes.Interface, error) {
		return fake.NewSimpleClientset(), nil
	}
	var probed []string
	doctorRunFn = func(_ context.Context, _ kubernetes.Interface, o doctor.Options) []doctor.Result {
		probed = append(probed, o.Namespace)
		if o.Namespace != envNS {
			return []doctor.Result{{Name: "Cluster reachable", Status: doctor.StatusFail, Reach: doctor.ReachNoEnv}}
		}
		return []doctor.Result{
			{Name: "Cluster reachable", Status: doctor.StatusOK, Reach: doctor.ReachOK},
			{Name: "Pod health", Status: doctor.StatusOK},
		}
	}
	return &probed
}

// okWhoAmI stubs the session probe so these tests reach the cluster stage.
func okWhoAmI(t *testing.T) {
	t.Helper()
	stubBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.io","account":"Acme"}`))
	})
}

// The field symptom: a healthy local k3d install whose active-client pointer
// names a namespace that isn't on this cluster. doctor bound the wrong pointer,
// probed only it, and told the user to reinstall over a working environment
// (#401 fixed only the EMPTY-pointer case).
func TestDoctor_WrongPointerOnLocalCluster_FindsTheEnvironment(t *testing.T) {
	writeActiveClientConfig(t, "stale-ns", "Stale")
	okWhoAmI(t)
	probed := stubDoctorForNamespace(t, "https://127.0.0.1:6550", "lukas-02", "lukas-02")

	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false)
	if strings.Contains(out.String(), "No secure environment") {
		t.Errorf("must not recommend a reinstall over a healthy install:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "lukas-02") {
		t.Errorf("doctor should name the environment that IS here:\n%s", out.String())
	}
	if len(*probed) != 2 || (*probed)[0] != "stale-ns" || (*probed)[1] != "lukas-02" {
		t.Errorf("probe namespaces = %v, want [stale-ns lukas-02] (bound pointer first, then the kubeconfig's own)", *probed)
	}

	// Bugbot (#515): finding the environment is only HALF the story. The pointer
	// is still stale, so `data ingest`/`resources`/`seal` keep exiting 4 — doctor
	// must say so and must not green the machine.
	if strings.Contains(out.String(), "Everything looks good") {
		t.Errorf("a stale pointer means data commands still fail — this is not 'ready to run training':\n%s", out.String())
	}
	// …and the readiness LINE must carry it too: a green "✔ Ready to run
	// training" beside a warning that contradicts it is the same unearned
	// success, just moved up the screen.
	if strings.Contains(out.String(), "✔ Ready to run training") {
		t.Errorf("the readiness line must not tick green while the pointer is stale:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Not ready") {
		t.Errorf("want the readiness line to carry the finding:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "stale-ns") {
		t.Errorf("doctor must name the stale pointer, not just the environment it found:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "client create") {
		t.Errorf("doctor must name the repoint:\n%s", out.String())
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.Code() != 2 {
		t.Fatalf("a stale pointer is a problem doctor found → want exit 2, got %v", err)
	}
}

// The other side of the same coin: with NO stale pointer, a healthy machine
// still gets its green line and exit 0. Without this, the assertion above could
// be satisfied by doctor never saying "Everything looks good" at all.
func TestDoctor_HealthyPointer_StillGreen(t *testing.T) {
	writeActiveClientConfig(t, "lukas-02", "Lukas") // pointer matches reality
	okWhoAmI(t)
	probed := stubDoctorForNamespace(t, "https://127.0.0.1:6550", "lukas-02", "lukas-02")

	var out bytes.Buffer
	if err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false); err != nil {
		t.Fatalf("a healthy machine with a correct pointer must exit 0, got %v", err)
	}
	if !strings.Contains(out.String(), "Everything looks good") {
		t.Errorf("want the green verdict when nothing is stale:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "✔ Ready to run training") {
		t.Errorf("control: the readiness line must still tick green when nothing is stale:\n%s", out.String())
	}
	if strings.Contains(out.String(), "client create") {
		t.Errorf("no repoint advice when the pointer is correct:\n%s", out.String())
	}
	if len(*probed) != 1 {
		t.Errorf("probe namespaces = %v, want one (nothing to re-probe)", *probed)
	}
}

// The ownership gate is what keeps the fallback honest, so it gets its own test:
// on a REMOTE/shared cluster a pointer miss stays "no secure environment" and the
// re-probe never runs — the client sitting in another namespace there may well be
// a colleague's (§7.5).
func TestDoctor_WrongPointerOnRemoteCluster_StaysGated(t *testing.T) {
	writeActiveClientConfig(t, "stale-ns", "Stale")
	okWhoAmI(t)
	probed := stubDoctorForNamespace(t, "https://k8s.corp.example:6443", "colleague-07", "colleague-07")

	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false)
	if err == nil {
		t.Fatal("a pointer miss on a remote cluster is still a problem — want a non-zero exit")
	}
	if !strings.Contains(out.String(), "No secure environment") {
		t.Errorf("remote cluster must keep the honest no-environment message:\n%s", out.String())
	}
	if strings.Contains(out.String(), "colleague-07") {
		t.Errorf("a remote cluster's other namespace must never be named as yours:\n%s", out.String())
	}
	if len(*probed) != 1 {
		t.Errorf("probe namespaces = %v, want exactly one (no re-probe off a remote cluster)", *probed)
	}
}

// A user who pinned --namespace themselves is never second-guessed: no binding
// was applied, so nothing re-probes and the miss stands as they asked for it.
func TestDoctor_ExplicitNamespaceMiss_IsNotReprobed(t *testing.T) {
	writeActiveClientConfig(t, "stale-ns", "Stale")
	okWhoAmI(t)
	probed := stubDoctorForNamespace(t, "https://127.0.0.1:6550", "lukas-02", "lukas-02")

	var out bytes.Buffer
	err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "chosen-ns", false)
	if err == nil {
		t.Fatal("an explicit --namespace miss must still fail")
	}
	if len(*probed) != 1 || (*probed)[0] != "chosen-ns" {
		t.Errorf("probe namespaces = %v, want only the namespace the user pinned", *probed)
	}
	if !strings.Contains(out.String(), "No secure environment") {
		t.Errorf("want the no-environment message for an explicitly-pinned miss:\n%s", out.String())
	}
}

// A machine that genuinely has nothing must keep the installer advice: the
// re-probe runs, finds no environment either, and the original results stand.
func TestDoctor_LocalClusterWithNothing_KeepsInstallerAdvice(t *testing.T) {
	writeActiveClientConfig(t, "stale-ns", "Stale")
	okWhoAmI(t)
	probed := stubDoctorForNamespace(t, "https://127.0.0.1:6550", "default", "nowhere")

	var out bytes.Buffer
	if err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false); err == nil {
		t.Fatal("a machine with no environment must still exit non-zero")
	}
	if !strings.Contains(out.String(), "No secure environment") {
		t.Errorf("want the no-environment message when the re-probe finds nothing too:\n%s", out.String())
	}
	if len(*probed) != 2 {
		t.Errorf("probe namespaces = %v, want the bound namespace and the kubeconfig's own", *probed)
	}
}

// Bugbot (#515): the re-probe used to adopt on anything that wasn't ReachNoEnv,
// which swept in ReachUnreachable and ReachError — both of which mean "we could
// not tell". Adopting either would name an unconfirmed namespace as a secure
// environment and tell the user this cluster already runs a client, pushing
// `client create` into a MINT on a cluster that may host nothing.
//
// The input domain is derived from doctor.ReachState's declared surface rather
// than hand-picked, plus the ABSENT case (reachStateOf's lenient default is
// exactly what must not apply here). Only ReachOK may adopt.
func TestDoctor_ReProbeAdoptsOnlyOnConfirmedReach(t *testing.T) {
	// Every non-OK member of the enum, and the missing-check case.
	cases := []struct {
		name    string
		results []doctor.Result
	}{
		{"unreachable", []doctor.Result{{Name: "Cluster reachable", Status: doctor.StatusFail, Reach: doctor.ReachUnreachable}}},
		{"error (RBAC/NotFound)", []doctor.Result{{Name: "Cluster reachable", Status: doctor.StatusFail, Reach: doctor.ReachError}}},
		{"no env", []doctor.Result{{Name: "Cluster reachable", Status: doctor.StatusFail, Reach: doctor.ReachNoEnv}}},
		{"check absent entirely", []doctor.Result{{Name: "Pod health", Status: doctor.StatusOK}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeActiveClientConfig(t, "stale-ns", "Stale")
			okWhoAmI(t)

			origLoad, origCS, origRun := loadClusterFn, newClientsetFn, doctorRunFn
			t.Cleanup(func() { loadClusterFn, newClientsetFn, doctorRunFn = origLoad, origCS, origRun })
			loadClusterFn = func(o cluster.KubeconfigOptions) (*cluster.ResolvedConfig, error) {
				ns := o.Namespace
				if ns == "" {
					ns = "unproven-ns"
				}
				return &cluster.ResolvedConfig{Namespace: ns, ServerURL: "https://127.0.0.1:6550", RestConfig: &rest.Config{}}, nil
			}
			newClientsetFn = func(*cluster.ResolvedConfig) (kubernetes.Interface, error) {
				return fake.NewSimpleClientset(), nil
			}
			doctorRunFn = func(_ context.Context, _ kubernetes.Interface, o doctor.Options) []doctor.Result {
				if o.Namespace == "stale-ns" { // the bound pointer always misses
					return []doctor.Result{{Name: "Cluster reachable", Status: doctor.StatusFail, Reach: doctor.ReachNoEnv}}
				}
				return c.results // the re-probe's inconclusive answer
			}

			var out bytes.Buffer
			err := runClusterDoctor(context.Background(), ui.New(&out, ui.WithColor(false)), "", "", "", false)
			got := out.String()
			if strings.Contains(got, "unproven-ns") {
				t.Errorf("an unconfirmed namespace must never be named as a secure environment:\n%s", got)
			}
			if strings.Contains(got, "client create") {
				t.Errorf("advising the repoint here can push a MINT — the cluster was never confirmed to run a client:\n%s", got)
			}
			if !strings.Contains(got, "No secure environment") {
				t.Errorf("an unconfirmed re-probe must keep the honest no-environment path:\n%s", got)
			}
			if err == nil {
				t.Error("want a non-zero exit when nothing was confirmed")
			}
		})
	}
}

// reachConfirmedOK is the guard above, tested directly against the whole
// declared enum so a future ReachState member can't quietly slip through the
// "could not tell" side. Mutation coverage cannot see a vocabulary gap.
func TestReachConfirmedOK(t *testing.T) {
	res := func(r doctor.ReachState) []doctor.Result {
		return []doctor.Result{{Name: "Cluster reachable", Reach: r}}
	}
	if !reachConfirmedOK(res(doctor.ReachOK)) {
		t.Error("ReachOK is the one positive confirmation")
	}
	for _, r := range []doctor.ReachState{doctor.ReachUnreachable, doctor.ReachNoEnv, doctor.ReachError} {
		if reachConfirmedOK(res(r)) {
			t.Errorf("Reach %v must not count as confirmed", r)
		}
	}
	if reachConfirmedOK([]doctor.Result{{Name: "Pod health"}}) {
		t.Error("an ABSENT reachability check is 'could not tell', not OK — reachStateOf's lenient default must not leak in here")
	}
	if reachConfirmedOK(nil) {
		t.Error("no results at all is not a confirmation")
	}
}

// localEnvNamespace is the doctor-side half of the #401 carve-out; its three
// refusals are what stop the re-probe from ever naming someone else's client.
func TestLocalEnvNamespace(t *testing.T) {
	set := func(rc *cluster.ResolvedConfig, err error) {
		t.Helper()
		orig := loadClusterFn
		t.Cleanup(func() { loadClusterFn = orig })
		loadClusterFn = func(cluster.KubeconfigOptions) (*cluster.ResolvedConfig, error) { return rc, err }
	}

	t.Run("local server with a namespace", func(t *testing.T) {
		set(&cluster.ResolvedConfig{Namespace: "lukas-02", ServerURL: "https://127.0.0.1:6550"}, nil)
		ns, ok := localEnvNamespace(cluster.KubeconfigOptions{})
		if !ok || ns != "lukas-02" {
			t.Errorf("= %q,%v; want lukas-02,true", ns, ok)
		}
	})

	t.Run("remote server is refused", func(t *testing.T) {
		set(&cluster.ResolvedConfig{Namespace: "colleague-07", ServerURL: "https://k8s.corp.example:6443"}, nil)
		if ns, ok := localEnvNamespace(cluster.KubeconfigOptions{}); ok {
			t.Errorf("= %q,%v; a remote cluster must be refused", ns, ok)
		}
	})

	t.Run("empty namespace is refused", func(t *testing.T) {
		set(&cluster.ResolvedConfig{Namespace: "", ServerURL: "https://127.0.0.1:6550"}, nil)
		if ns, ok := localEnvNamespace(cluster.KubeconfigOptions{}); ok {
			t.Errorf("= %q,%v; an empty namespace is not a reading", ns, ok)
		}
	})

	t.Run("load failure is refused", func(t *testing.T) {
		set(nil, context.DeadlineExceeded)
		if ns, ok := localEnvNamespace(cluster.KubeconfigOptions{}); ok {
			t.Errorf("= %q,%v; an unreadable kubeconfig is not a reading", ns, ok)
		}
	})
}
