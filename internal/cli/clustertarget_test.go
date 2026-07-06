package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
	"github.com/tracebloc/cli/internal/config"
)

func TestSetActiveClient_CachesNamespaceAndName(t *testing.T) {
	p := &config.Profile{}
	setActiveClient(p, &api.ProvisionedClient{ID: 7, Name: "Lab A", Namespace: "lab-a", Location: "FR"})
	if p.ActiveClientID != "7" || p.ActiveClientNamespace != "lab-a" || p.ActiveClientName != "Lab A" {
		t.Errorf("profile after setActiveClient = %+v", p)
	}
}

func TestBackfillActiveClientCache(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	clients := []api.ProvisionedClient{
		{ID: 1039, Name: "aws-ubuntu", Namespace: "tracebloc-amazon"},
		{ID: 1053, Name: "asad-macbook", Namespace: "asad-macbook-2"},
	}

	// Pre-cache config (old binary): id set, namespace empty → backfilled.
	cfg := &config.Config{CurrentEnv: "prod", Profiles: map[string]*config.Profile{
		"prod": {Token: "t", ActiveClientID: "1039"},
	}}
	backfillActiveClientCache(cfg, clients)
	if cfg.Current().ActiveClientNamespace != "tracebloc-amazon" || cfg.Current().ActiveClientName != "aws-ubuntu" {
		t.Errorf("backfill = %+v, want ns=tracebloc-amazon name=aws-ubuntu", cfg.Current())
	}

	// Already cached → left untouched (no clobber).
	cached := &config.Config{CurrentEnv: "prod", Profiles: map[string]*config.Profile{
		"prod": {Token: "t", ActiveClientID: "1039", ActiveClientNamespace: "custom-ns", ActiveClientName: "custom"},
	}}
	backfillActiveClientCache(cached, clients)
	if cached.Current().ActiveClientNamespace != "custom-ns" {
		t.Errorf("backfill clobbered an existing cache: %+v", cached.Current())
	}

	// Active id not in the list → no-op (no crash).
	orphan := &config.Config{CurrentEnv: "prod", Profiles: map[string]*config.Profile{
		"prod": {Token: "t", ActiveClientID: "9999"},
	}}
	backfillActiveClientCache(orphan, clients)
	if orphan.Current().ActiveClientNamespace != "" {
		t.Errorf("orphan id should not backfill, got %+v", orphan.Current())
	}
}

func TestClientStateLabel(t *testing.T) {
	cases := map[int]string{
		clientStatusOnline:  "online",
		clientStatusOffline: "offline",
		clientStatusPending: "pending",
		99:                  "unknown",
	}
	for status, want := range cases {
		if got := clientStateLabel(status); got != want {
			t.Errorf("clientStateLabel(%d) = %q, want %q", status, got, want)
		}
	}
}

// writeActiveClientConfig writes a signed-in dev profile whose active client
// carries the given namespace + name, into a temp config dir.
func writeActiveClientConfig(t *testing.T, namespace, name string) {
	t.Helper()
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	cfg := &config.Config{CurrentEnv: "dev", Profiles: map[string]*config.Profile{
		"dev": {Token: "tok", ActiveClientID: "5", ActiveClientNamespace: namespace, ActiveClientName: name},
	}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestBindActiveClientNamespace_DefaultsFromActiveClient(t *testing.T) {
	writeActiveClientConfig(t, "munich-radiology", "Munich Radiology")
	opts := cluster.KubeconfigOptions{}
	b := bindActiveClientNamespace(&opts)
	if !b.applied {
		t.Fatal("binding not applied")
	}
	if opts.Namespace != "munich-radiology" {
		t.Errorf("opts.Namespace = %q, want munich-radiology", opts.Namespace)
	}
	if b.name != "Munich Radiology" || b.namespace != "munich-radiology" {
		t.Errorf("binding = %+v", b)
	}
}

func TestBindActiveClientNamespace_ExplicitOverrideWins(t *testing.T) {
	writeActiveClientConfig(t, "munich-radiology", "Munich Radiology")

	// --namespace set → don't touch it.
	optsNS := cluster.KubeconfigOptions{Namespace: "chosen"}
	if b := bindActiveClientNamespace(&optsNS); b.applied || optsNS.Namespace != "chosen" {
		t.Errorf("with --namespace: applied=%v ns=%q, want false/chosen", b.applied, optsNS.Namespace)
	}

	// --context set → user is steering the cluster; don't bind.
	optsCtx := cluster.KubeconfigOptions{Context: "some-ctx"}
	if b := bindActiveClientNamespace(&optsCtx); b.applied || optsCtx.Namespace != "" {
		t.Errorf("with --context: applied=%v ns=%q, want false/empty", b.applied, optsCtx.Namespace)
	}
}

func TestBindActiveClientNamespace_NoActiveClient(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir()) // no config written → nothing cached
	opts := cluster.KubeconfigOptions{}
	if b := bindActiveClientNamespace(&opts); b.applied || opts.Namespace != "" {
		t.Errorf("no active client: applied=%v ns=%q, want false/empty", b.applied, opts.Namespace)
	}
}

func TestActiveClientBinding_Explain(t *testing.T) {
	noRelease := &exitError{code: 4, err: &noParentReleaseError{errors.New("no release")}}
	pvcMissing := &exitError{code: 4, err: errors.New("shared PVC not bound")}

	// Applied + "no release here" → rewritten to the §7.3 guidance.
	bound := activeClientBinding{applied: true, name: "gpu-box-01", namespace: "gpu-box-01"}
	got := bound.explain(noRelease)
	if got == noRelease {
		t.Fatal("expected a rewritten error")
	}
	if !strings.Contains(got.Error(), "another machine") || !strings.Contains(got.Error(), "gpu-box-01") {
		t.Errorf("rewritten error = %q", got.Error())
	}
	var ee *exitError
	if !errors.As(got, &ee) || ee.Code() != 4 {
		t.Errorf("rewritten error should stay exit 4, got %v", got)
	}

	// Applied but a PVC failure (release WAS found) → pass through untouched.
	if bound.explain(pvcMissing) != pvcMissing {
		t.Error("PVC-missing error should not be rewritten")
	}

	// Not applied → always pass through.
	if (activeClientBinding{}).explain(noRelease) != noRelease {
		t.Error("unbound explain should pass the error through")
	}
}
