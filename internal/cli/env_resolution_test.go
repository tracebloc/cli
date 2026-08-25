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
	"errors"
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracebloc/cli/internal/api"
	"github.com/tracebloc/cli/internal/cluster"
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

	// HERMETIC BY CONSTRUCTION, and this matters more than it looks. Past the
	// session probe, `cluster doctor` loads the real kubeconfig and calls the real
	// doctor.Run, whose checkBackendEgress probes backendHost("") — i.e. it issues
	// a live GET to https://api.tracebloc.io/. On a developer machine with a real
	// k3d cluster that is a genuine production request from a unit test. Failing
	// loadClusterFn returns right after the session probe, which is everything
	// this test needs: the env is decided before it.
	origLoad := loadClusterFn
	loadClusterFn = func(cluster.KubeconfigOptions) (*cluster.ResolvedConfig, error) {
		return nil, errors.New("no cluster (stubbed: keeps this test off the network)")
	}
	t.Cleanup(func() { loadClusterFn = origLoad })

	// doctor exits non-zero here (stubbed no-cluster); the assertion is on the env
	// it built the client for, which is decided before that.
	_, _ = runCmd(t, "cluster", "doctor")

	if gotEnv != api.EnvDev {
		t.Fatalf("cluster doctor built its API client for %q, want %q — it must resolve "+
			"through sessionEnv like authedClient, not read cfg.CurrentEnv raw "+
			"(api.BaseURL(%q) is the PROD default, so this probes the wrong backend)",
			gotEnv, api.EnvDev, gotEnv)
	}
}

// TestClusterDoctorFailsClosedOnUnknownEnv is the backend#2171 fail-closed
// contract for `cluster doctor`: a session whose current_env is unrecognised must
// NOT build a client and probe with the stored token — that dials api.BaseURL's
// prod fallback. doctor hard-stops with the re-login remedy instead, and never
// reaches the probe.
func TestClusterDoctorFailsClosedOnUnknownEnv(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	t.Setenv("CLIENT_ENV", "")
	if err := (&config.Config{CurrentEnv: "staging", Profiles: map[string]*config.Profile{
		"staging": {Token: "tok", Email: "ds@co"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}

	var built []string
	orig := newAPIClient
	newAPIClient = func(env string) *api.Client {
		built = append(built, env)
		return &api.Client{HTTP: &http.Client{Timeout: time.Millisecond}}
	}
	t.Cleanup(func() { newAPIClient = orig })

	// Belt-and-braces: keep the test off the network even if the fail-closed return
	// ever regresses to fall through to the cluster load (which probes prod live).
	origLoad := loadClusterFn
	loadClusterFn = func(cluster.KubeconfigOptions) (*cluster.ResolvedConfig, error) {
		return nil, errors.New("no cluster (stubbed: keeps this test off the network)")
	}
	t.Cleanup(func() { loadClusterFn = origLoad })

	out, err := runCmd(t, "cluster", "doctor")
	if ExitCodeFromError(err) == 0 {
		t.Fatalf("cluster doctor must fail closed on an unknown env, got exit 0:\n%s", out)
	}
	if len(built) != 0 {
		t.Errorf("cluster doctor built a client for %v — an unknown env must never reach "+
			"the probe (it resolves to prod, sending the token there): backend#2171", built)
	}
	if !strings.Contains(out, "unrecognised backend") {
		t.Errorf("want the unrecognised-backend message, got:\n%s", out)
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

// TestAuthStatusShowsTheEnvTheClientWillUse: `auth status` printed the raw
// stored env as its "backend" field, so the human-facing answer to "which
// backend am I on?" could differ from the one --check computes and the one
// authedClient dials — two answers to the same question, in the same file.
func TestAuthStatusShowsTheEnvTheClientWillUse(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	t.Setenv("CLIENT_ENV", "")
	if err := (&config.Config{CurrentEnv: " Dev ", Profiles: map[string]*config.Profile{
		" Dev ": {Token: "tok", Email: "ds@co"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, "auth", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, api.EnvDev) || strings.Contains(out, " Dev ") {
		t.Fatalf("auth status reported the stored env verbatim, not the resolved one.\n"+
			"want the %q the client actually dials, got:\n%s", api.EnvDev, out)
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

// --- the token boundary fails closed on an unrecognised env (backend#2171) ----

// TestAuthedClientRejectsAnUnknownSessionEnv is the backend#2171 fix: a config
// whose current_env is a typo / renamed / stale value must NOT resolve to prod and
// carry the stored token there. api.BaseURL maps every unknown env to
// https://api.tracebloc.io, so before the knownSessionEnv gate authedClient built
// a client for that prod URL and attached the token — sending the credential to
// production while the user believed another backend.
func TestAuthedClientRejectsAnUnknownSessionEnv(t *testing.T) {
	const badEnv = "staging" // the real-world footgun: "staging", not "stg"

	// Anchor the premise the fix defends against: this env WOULD route to prod. If
	// api.BaseURL ever stops defaulting unknowns to prod, this test still holds, but
	// the failure it guards would have changed shape — so assert the mapping here.
	if got := api.BaseURL(badEnv); got != "https://api.tracebloc.io" {
		t.Fatalf("premise broken: api.BaseURL(%q) = %q, want the prod URL — this test "+
			"guards that authedClient never lets an unknown env reach that mapping", badEnv, got)
	}

	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	t.Setenv("CLIENT_ENV", "")
	if err := (&config.Config{CurrentEnv: badEnv, Profiles: map[string]*config.Profile{
		badEnv: {Token: "tok", Email: "ds@co"},
	}}).Save(); err != nil {
		t.Fatal(err)
	}

	// A token-bearing client must never be BUILT for an unknown env — building it is
	// precisely what attaches the token to a prod BaseURL. Record every env
	// newAPIClient is asked for; the fix must leave this empty.
	var built []string
	orig := newAPIClient
	newAPIClient = func(env string) *api.Client {
		built = append(built, env)
		return &api.Client{BaseURL: api.BaseURL(env)}
	}
	t.Cleanup(func() { newAPIClient = orig })

	client, _, err := authedClient()
	if err == nil {
		t.Fatalf("authedClient() returned no error for current_env=%q — it must fail "+
			"closed rather than target prod (backend#2171)", badEnv)
	}
	if client != nil {
		t.Errorf("authedClient() returned a non-nil client (BaseURL %q) on the failure "+
			"path — no token-bearing client may be built for an unknown env", client.BaseURL)
	}
	if !strings.Contains(err.Error(), badEnv) {
		t.Errorf("error %q does not name the bad env %q — the message must tell the user "+
			"which env is unrecognised", err, badEnv)
	}
	if len(built) != 0 {
		t.Errorf("newAPIClient was called for %v — an unknown env must never reach the "+
			"client, and thus never reach api.BaseURL's prod fallback", built)
	}
}

// TestKnownSessionEnvGatesUnknownButKeepsKnown pins the gate directly: known envs
// (including the case/whitespace variants sessionEnv normalises) pass through, and
// only unrecognised values are rejected. An EMPTY config resolves to the prod
// DEFAULT — a known env — so a legacy/empty session still works: the bug is a
// present-but-wrong env, not the absence of one.
func TestKnownSessionEnvGatesUnknownButKeepsKnown(t *testing.T) {
	t.Setenv("CLIENT_ENV", "")
	for _, tc := range []struct {
		stored  string
		wantEnv string
		wantErr bool
	}{
		{"dev", api.EnvDev, false},
		{"Dev", api.EnvDev, false},   // normalised (case)
		{" stg ", api.EnvStg, false}, // normalised (whitespace)
		{"prod", api.EnvProd, false},
		{"", api.EnvProd, false}, // empty -> prod default, a known env
		{"staging", "", true},    // the footgun: "staging" != "stg"
		{"banana", "", true},
	} {
		t.Run(tc.stored, func(t *testing.T) {
			got, err := knownSessionEnv(&config.Config{CurrentEnv: tc.stored})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("knownSessionEnv(%q) = %q, nil — want an error, since an "+
						"unknown env must not resolve to prod", tc.stored, got)
				}
				if want := strings.ToLower(strings.TrimSpace(tc.stored)); !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must name the bad env %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("knownSessionEnv(%q) errored: %v — a known env must pass", tc.stored, err)
			}
			if got != tc.wantEnv {
				t.Fatalf("knownSessionEnv(%q) = %q, want %q", tc.stored, got, tc.wantEnv)
			}
		})
	}
}

// --- 3. the guard: no NEW resolution site can appear unnoticed ----------------

// resolutionSites is the closed set of files in this MODULE allowed to answer
// "which backend?" from ambient state (the config file or $CLIENT_ENV).
// Everything else must take a resolved env as an argument.
//
// This is the rule the last three recuts each re-litigated one site at a time. It
// is here rather than in a reviewer's head because the failure mode is additive:
// every new site looks locally correct, and only the SECOND one is a bug.
//
// Keys are repo-relative because the walk is repo-wide. It used to walk only
// internal/cli, which left the guard blind in 16 of the 17 packages under
// internal/ — i.e. blind exactly where a new site is most likely to land, in a
// package written by someone who never reads internal/cli (Lukas on #551).
var resolutionSites = map[string]string{
	// The primitive: ResolveEnv is the --env/$CLIENT_ENV/prod chain, and the only
	// os.Getenv("CLIENT_ENV") in the module.
	"internal/api/client.go": "api.ResolveEnv — the primitive chain, and the only $CLIENT_ENV read",
	// The --env FLAG, a different question: the env the human/installer NAMED,
	// which login persists (the one cfg.CurrentEnv WRITE) and `auth status --check`
	// validates against the session.
	"internal/cli/auth.go": "api.ResolveEnv(envFlag) — the explicit --env flag, validated by IsKnownEnv",
	// sessionEnv: config current_env, else $CLIENT_ENV, else prod. The one chain
	// every session-env consumer in internal/cli resolves through.
	"internal/cli/client.go": "sessionEnv — the single config -> session-env resolution",
	// Storage. Profiles are keyed by the RAW stored string, so this layer must not
	// normalise; it hands the raw value out and sessionEnv normalises it.
	"internal/config/config.go": "the on-disk current_env field, its accessors, and the v1 migration",
	// The CLUSTER's CLIENT_ENV, read off the jobs-manager Deployment — a
	// deliberately different question from this CLI's session env.
	"internal/doctor/doctor.go": "the cluster's own CLIENT_ENV, for the egress probe's target host",
	// internal/cli/telemetry.go is deliberately ABSENT, and the staleness check
	// below is what keeps it that way: telemetryEnv/signedInEnv delegate the whole
	// chain to sessionEnv and name no needle, so an entry for it would be inert —
	// a licence to re-admit a raw read in the very file whose double resolution
	// this PR exists to remove (Bugbot on #551).
}

// envNeedles are the ways a file reads ambient state to answer "which backend?".
// api.BaseURL/IsKnownEnv are pure mappings over an argument and are deliberately
// absent — they resolve nothing.
//
// Deliberately BROAD (bare identifiers, and CLIENT_ENV unquoted so it matches
// help text too): a false positive is a loud line in a diff, a false negative is
// the bug this guard exists to catch. Fail closed.
var envNeedles = []string{"CurrentEnv", "ResolveEnv", "CLIENT_ENV"}

// matchesAnyNeedle is THE matcher, called from both directions — the detection
// sweep and the allowlist audit. One function on purpose: two copies of "does
// this file resolve an env?" is the same shape as the two copies of "which env?"
// that this PR exists to remove, and it would let detection and allowlisting
// drift apart exactly where nobody looks.
func matchesAnyNeedle(code string) (string, bool) {
	for _, needle := range envNeedles {
		if strings.Contains(code, needle) {
			return needle, true
		}
	}
	return "", false
}

// envCodeOf tokenises one file, or reports why it could not.
func envCodeOf(path string) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return goCodeTokens(string(src))
}

func TestNoNewEnvironmentResolutionSiteAppears(t *testing.T) {
	const root = "../.." // this test's package dir -> the module root

	// --- detection: what actually resolves an env, module-wide ---------------
	matched := map[string]string{} // repo-relative path -> the needle that hit
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", ".worktrees", "vendor", "node_modules", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		code, err := envCodeOf(path)
		if err != nil {
			// "cannot tell" is not "clean": abort rather than read a file whose
			// tokenisation failed as a string that matches nothing.
			return fmt.Errorf("%s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if needle, ok := matchesAnyNeedle(code); ok {
			matched[filepath.ToSlash(rel)] = needle
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking the module: %v", walkErr)
	}

	// --- every match must be sanctioned --------------------------------------
	for rel, needle := range matched {
		if _, ok := resolutionSites[rel]; !ok {
			t.Errorf("%s resolves an environment from ambient state (%q), but is not a "+
				"sanctioned resolution site.\n"+
				"Take the resolved env as an ARGUMENT instead — the CLI must answer "+
				"\"which backend?\" once per invocation and thread it. If this really is "+
				"a new resolution point, add it to resolutionSites with the reason and "+
				"say how it cannot disagree with sessionEnv.", rel, needle)
		}
	}

	// --- and every sanctioned entry must still EARN its place ----------------
	//
	// THE ALLOWLIST MUST STAY A RECORD, NOT BECOME A LICENCE. Asking only whether a
	// sanctioned file EXISTS is the defect class this whole PR is about: checking
	// the form of a thing instead of the property the form exists to guarantee. An
	// entry whose file no longer resolves anything is inert — it verifies nothing,
	// while silently pre-approving the next ambient read in that file. My own
	// consolidation did exactly that to the telemetry.go entry (Bugbot on #551),
	// in the one file whose double resolution this PR removes.
	//
	// Per ENTRY rather than per suite, so the failure names the entry to delete.
	// This also subsumes the "needles went stale" backstop: rename a needle and
	// every entry goes inert at once, which is loud and specific rather than a
	// single global counter hitting zero.
	for rel, why := range resolutionSites {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("resolutionSites lists %s, which cannot be read — stale allowlist", rel)
			continue
		}
		if why == "" {
			t.Errorf("resolutionSites[%q] is allowlisted with no reason", rel)
		}
		if _, ok := matched[rel]; !ok {
			t.Errorf("%s is allowlisted as a resolution site but resolves nothing any more "+
				"— drop the entry, or the guard silently permits the next ambient read "+
				"here.", rel)
		}
	}

	// The one thing the per-entry loop cannot see: an EMPTY allowlist makes it
	// vacuous, and a needle rename plus an empty allowlist would then pass in
	// silence. Anchor both.
	if len(resolutionSites) == 0 || len(matched) == 0 {
		t.Fatalf("the guard checked nothing: %d sanctioned entries, %d files matched",
			len(resolutionSites), len(matched))
	}
}

// goCodeTokens renders src as its Go TOKENS, with comments dropped — so the
// guard reads code, never prose (these names are the subject of half the
// comments in the package).
//
// SCANS GO AS GO, because the hand-rolled comment stripper this replaces was
// fail-open on string literals, and Lukas demonstrated both holes on #551:
// the "//" inside a "https://…" literal started a comment that ate the rest of
// the line (needle included), and a "/*" inside a literal like "/*.json"
// swallowed every needle below it to the next "*/" or EOF. Seven non-test files
// in internal/cli already carry an https:// literal, so that was one future line
// away, not a contrived shape.
//
// Literals are KEPT in the output rather than dropped: a needle inside a string
// then reads as a loud false positive, which is cheap — the opposite direction
// fails silently, which is the bug.
//
// SCOPE OF THE ERROR, stated precisely because the first version of this comment
// overclaimed it: go/scanner is LEXICAL, not syntactic, so this returns an error
// only on lexical faults — an unterminated string literal or an unterminated
// /* comment. `func f( {` scans clean and is reported as normal code. That is the
// right guarantee rather than a weak one: the faults it does catch are exactly
// the ones that would desynchronise literal/comment boundaries and hand the
// needles a misread file, which is the failure this function exists to prevent.
// A file that tokenises correctly but does not compile still yields correct
// needles, and `go build` is the check for whether it compiles.
func goCodeTokens(src string) (string, error) {
	var fset token.FileSet
	var sc scanner.Scanner
	scanErrs := 0
	f := fset.AddFile("", fset.Base(), len(src))
	// mode 0: comment tokens are not emitted at all.
	sc.Init(f, []byte(src), func(token.Position, string) { scanErrs++ }, 0)
	var b strings.Builder
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		if lit != "" {
			b.WriteString(lit)
		} else {
			b.WriteString(tok.String())
		}
		b.WriteByte(' ')
	}
	if scanErrs > 0 {
		return "", fmt.Errorf("%d scan error(s) — cannot tell what this file reads", scanErrs)
	}
	return b.String(), nil
}
