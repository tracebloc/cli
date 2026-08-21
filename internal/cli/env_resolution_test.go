package cli

// The environment / base-URL resolution family (backend#2171).
//
// The CLI answers "which backend am I talking to?" in several places, and each
// recut of the release train produced one more finding about a site that answered
// it differently from its neighbour: cli#528 (the telemetry label resolved through
// ResolveEnv while the client used the config), #542 (a spool path resolved twice),
// #540 (the label and the sink resolved twice). The pattern is always the same —
// a SECOND resolution of a question already answered — so these tests pin the
// invariants rather than the individual sites:
//
//  1. sessionEnv is the ONLY function that turns a config into a session env, and
//     it normalises (trim + lower-case) like api.ResolveEnv does.
//  2. Callers take the resolved value; they never re-derive it.
//
// NOT covered here, deliberately: api.BaseURL's unknown/empty -> prod fail-open.
// That behaviour is shared with the installer's `_backend_url` and diverges from
// client-runtime's controller.py (which refuses), so changing it is a three-
// component decision tracked on backend#2171, not a CLI-local cleanup.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/telemetry"
)

// --- 1. sessionEnv is the single, normalising resolution point ----------------

// TestSessionEnvNormalisesTheStoredEnv: sessionEnv used to return cfg.CurrentEnv
// verbatim, which made it the one env-resolving function in the CLI whose output
// was not normalised — api.ResolveEnv lower-cases both its argument and
// $CLIENT_ENV, api.BaseURL lower-cases, spoolEnvSlug trims and lower-cases.
//
// Verbatim is invisible where the value only reaches api.BaseURL (which
// lower-cases again) and load-bearing everywhere else: a value that is COMPARED
// (`auth status --check`) or TRIMMED by one consumer and not another (BaseURL does
// not trim; " dev " therefore fell through to PROD).
func TestSessionEnvNormalisesTheStoredEnv(t *testing.T) {
	for _, tc := range []struct {
		stored string
		want   string
	}{
		{"dev", "dev"},
		{"Dev", "dev"},   // migrateV1 stores a v1 `env` verbatim
		{" dev ", "dev"}, // hand-written / fixture config
		{"PROD", "prod"},
		{"banana", "banana"}, // unknown is preserved, not coerced — see BaseURL note above
	} {
		t.Run(tc.stored, func(t *testing.T) {
			t.Setenv("CLIENT_ENV", "")
			cfg := &config.Config{CurrentEnv: tc.stored}
			if got := sessionEnv(cfg); got != tc.want {
				t.Fatalf("sessionEnv(current_env=%q) = %q, want %q — sessionEnv must "+
					"normalise like api.ResolveEnv, or its callers disagree about the "+
					"same session", tc.stored, got, tc.want)
			}
		})
	}
}

// TestSessionEnvFallsBackToClientEnvOnlyWhenUnset pins the precedence the task
// brief calls load-bearing: config `current_env` BEATS $CLIENT_ENV, and
// $CLIENT_ENV is consulted only when `current_env` is absent. The offboard e2e
// fixture writes `"current_env": "prod"` into its config, so a change that let
// $CLIENT_ENV win would silently repoint that suite.
func TestSessionEnvFallsBackToClientEnvOnlyWhenUnset(t *testing.T) {
	t.Setenv("CLIENT_ENV", "dev")

	if got := sessionEnv(&config.Config{CurrentEnv: "prod"}); got != api.EnvProd {
		t.Fatalf("sessionEnv(current_env=prod) with CLIENT_ENV=dev = %q, want %q — "+
			"the signed-in env must beat $CLIENT_ENV", got, api.EnvProd)
	}
	if got := sessionEnv(&config.Config{}); got != api.EnvDev {
		t.Fatalf("sessionEnv(no current_env) with CLIENT_ENV=dev = %q, want %q — "+
			"$CLIENT_ENV is the legacy/empty-config fallback", got, api.EnvDev)
	}
	t.Setenv("CLIENT_ENV", "")
	if got := sessionEnv(&config.Config{}); got != api.EnvProd {
		t.Fatalf("sessionEnv(no current_env, no CLIENT_ENV) = %q, want %q", got, api.EnvProd)
	}
}

// --- 2. no caller re-derives the session env ----------------------------------

// TestClusterDoctorProbesTheSessionEnv: `cluster doctor` built its API client
// from cfg.CurrentEnv directly instead of sessionEnv — a second answer to the
// question authedClient already answers. The whitespace case makes the two
// answers differ for real: api.BaseURL lower-cases but does NOT trim, so
// newAPIClient(" dev ") lands on the prod default while every other
// authenticated command talks to dev-api.
//
// A doctor that probes prod with a dev token reports "session expired" for a
// session that is fine — the worst possible output from a diagnostic.
func TestClusterDoctorProbesTheSessionEnv(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	t.Setenv("CLIENT_ENV", "")
	if err := (&config.Config{CurrentEnv: " dev ", Profiles: map[string]*config.Profile{
		" dev ": {Token: "tok", Email: "ds@co"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}

	var gotEnv string
	orig := newAPIClient
	newAPIClient = func(env string) *api.Client {
		gotEnv = env
		// No BaseURL: the WhoAmI below must fail locally rather than reach any
		// real host. What is under test is the env, not the probe's verdict.
		return &api.Client{HTTP: &http.Client{Timeout: time.Millisecond}}
	}
	t.Cleanup(func() { newAPIClient = orig })

	// doctor exits non-zero here (no cluster, failed probe); the assertion is on
	// the env it built the client for, which is decided before any of that.
	_, _ = runCmd(t, "cluster", "doctor")

	if gotEnv != api.EnvDev {
		t.Fatalf("cluster doctor built its API client for %q, want %q — it must resolve "+
			"through sessionEnv like authedClient, not read cfg.CurrentEnv raw "+
			"(api.BaseURL(%q) is the PROD default, so this probes the wrong backend)",
			gotEnv, api.EnvDev, gotEnv)
	}
}

// TestAuthCheckComparesTheResolvedEnv: `auth status --check` compared the raw
// cfg.CurrentEnv against api.ResolveEnv's already-normalised target, so a config
// carrying a non-normalised env failed the probe for the very session it is
// signed in to — and the installer, whose contract is this exit code, would then
// re-run `login` (or skip provisioning) against a session that works.
func TestAuthCheckComparesTheResolvedEnv(t *testing.T) {
	probed := false
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo/" {
			probed = true
			_, _ = w.Write([]byte(`{"email":"ds@co","account":"Acme"}`))
		}
	})
	// withTestBackend isolates the config dir; write a non-normalised env into it.
	if err := (&config.Config{CurrentEnv: "Dev", Profiles: map[string]*config.Profile{
		"Dev": {Token: "tok", Email: "ds@co"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}

	if _, err := runCmd(t, "auth", "status", "--check", "--env", "dev"); err != nil {
		t.Fatalf("--check --env dev must accept a session stored as \"Dev\" "+
			"(sessionEnv resolves it to dev and the client talks to dev-api), got: %v", err)
	}
	if !probed {
		t.Error("the backend was never probed — the env comparison rejected a session " +
			"that differs from the target only in case")
	}
}

// TestAuthCheckStillRejectsARealEnvMismatch is the other half: normalising the
// comparison must not make it lenient about the thing it exists to catch.
func TestAuthCheckStillRejectsARealEnvMismatch(t *testing.T) {
	probed := false
	withTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo/" {
			probed = true
		}
	})
	saveSignedIn(t, "tok") // CurrentEnv=dev
	if _, err := runCmd(t, "auth", "status", "--check", "--env", "stg"); ExitCodeFromError(err) != 1 {
		t.Fatalf("exit code = %d, want 1 — a dev session must not satisfy a stg target",
			ExitCodeFromError(err))
	}
	if probed {
		t.Error("must not probe the backend on a genuine env mismatch")
	}
}

// TestTheRecordIsLabelledWithTheEnvItWasHanded is the #540 finding.
//
// RecordCommandOutcome resolves the env once and derives BOTH the record's label
// and the sink (spool path + POST destination) from that one value.
// recordCommandOutcome used to call telemetryEnv(signedInEnv()) again for the
// emitter, so the label came from a second, independent config read: two reads
// that merely tend to agree, and disagree the moment a `login` lands between them
// — which is precisely the "labelled stg, posted to prod" leak the comment above
// RecordCommandOutcome claims to prevent.
//
// The invariant is structural, because a race between two config reads is not a
// deterministic test: recordCommandOutcome must label the record with the env it
// was GIVEN. Handing it an env that disagrees with the config on disk is how we
// tell "used the parameter" from "read the config again".
func TestTheRecordIsLabelledWithTheEnvItWasHanded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
	t.Setenv("CLIENT_ENV", "")
	// On disk: dev. A second resolution inside recordCommandOutcome would find
	// this and label the record "dev".
	body := `{"version":2,"current_env":"dev","profiles":{"dev":{"token":"x"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := signedInEnv(); got != api.EnvDev {
		t.Fatalf("signedInEnv() = %q, want %q — the fixture no longer matches the "+
			"on-disk config layout, so this test would pass vacuously", got, api.EnvDev)
	}

	var res map[string]string
	sink := telemetry.Sink(func(r map[string]string, _ map[string]any) { res = r })

	root := NewRootCmd(testBuildInfo())
	// Handed: stg — standing in for "the value the sink was built from", which is
	// what the caller resolved before the config changed under it.
	if err := recordCommandOutcome(
		root, root, testBuildInfo(), 0, time.Second,
		func(string) string { return "" }, api.EnvStg, sink,
	); err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("nothing was delivered")
	}
	if got := res["deployment.environment"]; got != api.EnvStg {
		t.Fatalf("deployment.environment = %q, want %q — the emitter must be labelled "+
			"with the env it was handed (the one the sink was built from), not with a "+
			"second read of the config", got, api.EnvStg)
	}
}

// --- 3. the guard: no NEW resolution site can appear unnoticed ----------------

// resolutionSites is the closed set of places in internal/cli allowed to answer
// "which backend?" from ambient state (the config file or $CLIENT_ENV). Everything
// else must take a resolved env as an argument.
//
// This is the rule the last three recuts each re-litigated one site at a time. It
// is here rather than in a reviewer's head because the failure mode is additive:
// every new site looks locally correct, and only the SECOND one is a bug.
var resolutionSites = map[string]string{
	// sessionEnv: config current_env, else $CLIENT_ENV, else prod. The one chain.
	"client.go": "sessionEnv — the single config -> session-env resolution",
	// The --env FLAG, a different question: the env the human/installer NAMED,
	// which login persists and `auth status --check` validates against the session.
	"auth.go": "api.ResolveEnv(envFlag) — the explicit --env flag, validated by IsKnownEnv",
	// telemetryEnv/signedInEnv read the config, but both delegate the chain to
	// sessionEnv; they only map the result onto a label.
	"telemetry.go": "telemetryEnv/signedInEnv — delegate to sessionEnv, map to a label",
}

func TestNoNewEnvironmentResolutionSiteAppears(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// Reading ambient state to answer "which backend?": the config's current_env,
	// or the $CLIENT_ENV chain. api.BaseURL/IsKnownEnv are pure mappings over an
	// argument and are deliberately NOT listed — they resolve nothing.
	needles := []string{"CurrentEnv", "ResolveEnv(", `Getenv("CLIENT_ENV")`}

	found := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		// Comments discuss these names constantly (they are the whole subject);
		// only code counts.
		code := stripGoComments(string(src))
		for _, n := range needles {
			if !strings.Contains(code, n) {
				continue
			}
			found++
			if why, ok := resolutionSites[name]; !ok {
				t.Errorf("%s resolves an environment from ambient state (%q), but is not a "+
					"sanctioned resolution site.\n"+
					"Take the resolved env as an ARGUMENT instead — the CLI must answer "+
					"\"which backend?\" once per invocation and thread it. If this really is "+
					"a new resolution point, add it to resolutionSites with the reason and "+
					"say how it cannot disagree with sessionEnv.", name, n)
			} else if why == "" {
				t.Errorf("%s is allowlisted with no reason", name)
			}
			break
		}
	}
	// A guard that matches nothing is not a guard: if the needles ever stop
	// matching (a rename), fail loudly rather than pass vacuously.
	if found == 0 {
		t.Fatal("the guard matched no files at all — its needles are stale and it is " +
			"no longer checking anything")
	}
	for name := range resolutionSites {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("resolutionSites lists %s, which does not exist — stale allowlist", name)
		}
	}
}

// stripGoComments removes // and /* */ comments so the guard reads code, not prose.
func stripGoComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			if n := strings.IndexByte(src[i:], '\n'); n >= 0 {
				i += n
			} else {
				i = len(src)
			}
		case strings.HasPrefix(src[i:], "/*"):
			if n := strings.Index(src[i+2:], "*/"); n >= 0 {
				i += n + 4
			} else {
				i = len(src)
			}
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}
