package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	in := &Config{CurrentEnv: "dev", Profiles: map[string]*Profile{
		"dev": {Email: "a@b.com", Token: "tok123", ActiveClientID: "edge_1"},
	}}
	if err := in.Save(); err != nil {
		t.Fatal(err)
	}
	out, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if out.CurrentEnv != "dev" || !out.SignedIn() {
		t.Fatalf("round-trip: %+v", out)
	}
	p := out.Profile("dev")
	if p.Email != "a@b.com" || p.Token != "tok123" || p.ActiveClientID != "edge_1" {
		t.Errorf("round-trip profile mismatch: %+v", p)
	}
}

func TestSaveIs0600(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	c := &Config{CurrentEnv: "prod", Profiles: map[string]*Profile{"prod": {Token: "secret"}}}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	p, _ := Path()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600 (it holds tokens)", fi.Mode().Perm())
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatalf("missing config should not error: %v", err)
	}
	if c.SignedIn() {
		t.Errorf("missing config should be empty, got %+v", c)
	}
}

func TestClear(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	_ = (&Config{CurrentEnv: "prod", Profiles: map[string]*Profile{"prod": {Token: "x"}}}).Save()
	if err := Clear(); err != nil {
		t.Fatal(err)
	}
	if c, _ := Load(); c.SignedIn() {
		t.Error("after Clear, should not be signed in")
	}
	if err := Clear(); err != nil {
		t.Errorf("Clear on a missing file should be nil, got %v", err)
	}
}

// TestMigrateV1ToV2 pins the v1 (flat cli#83 schema) → v2 migration: the single
// record is wrapped under profiles[env], no data loss, and the next Save rewrites
// the file as v2 on disk.
func TestMigrateV1ToV2(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
	v1 := `{"env":"dev","email":"a@b.com","token":"tok123","active_client_id":"7"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 2 {
		t.Errorf("version = %d, want 2", c.Version)
	}
	if c.CurrentEnv != "dev" {
		t.Errorf("current_env = %q, want dev", c.CurrentEnv)
	}
	if p := c.Profile("dev"); p.Token != "tok123" || p.Email != "a@b.com" || p.ActiveClientID != "7" {
		t.Errorf("migrated dev profile = %+v, want token/email/active carried over", p)
	}
	if !c.SignedIn() {
		t.Error("a migrated v1 record with a token should be signed in")
	}
	// Persisting rewrites the file as v2.
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if !strings.Contains(string(raw), `"version": 2`) || !strings.Contains(string(raw), `"profiles"`) {
		t.Errorf("on-disk file is not v2 after save:\n%s", raw)
	}
}

// TestMigrateV1EmptyEnvDefaultsProd: a v1 token with no env migrates under the
// historical default (prod), not a profile keyed by the empty string.
func TestMigrateV1EmptyEnvDefaultsProd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.CurrentEnv != "prod" || c.Profile("prod").Token != "t" {
		t.Errorf("empty-env v1 should migrate under prod, got current=%q profiles=%+v", c.CurrentEnv, c.Profiles)
	}
}

// TestProfilesAreEnvScoped_NoClobber is the R10 fix: `login --env X` switches
// current_env and writes X's profile WITHOUT touching the other envs' active
// client pointers. Simulates dev → prod → dev and asserts dev's active client
// survives (the v1 flat schema clobbered it).
func TestProfilesAreEnvScoped_NoClobber(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())

	// Sign into dev, pick a dev client.
	c, _ := Load()
	c.CurrentEnv = "dev"
	c.Profile("dev").Token = "dev-tok"
	c.Profile("dev").ActiveClientID = "11"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	// `login --env prod`: switch env, write prod's profile.
	c, _ = Load()
	c.CurrentEnv = "prod"
	c.Profile("prod").Token = "prod-tok"
	c.Profile("prod").ActiveClientID = "22"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	// `login --env dev` again: switch back.
	c, _ = Load()
	c.CurrentEnv = "dev"
	c.Profile("dev").Token = "dev-tok2"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	c, _ = Load()
	if got := c.Profile("dev").ActiveClientID; got != "11" {
		t.Errorf("dev active_client_id = %q, want 11 (clobbered across envs — R10)", got)
	}
	if got := c.Profile("prod").ActiveClientID; got != "22" {
		t.Errorf("prod active_client_id = %q, want 22 (other env touched)", got)
	}
}

// TestSignedInRequiresCurrentEnvToken: a profile that exists for a non-current
// env doesn't count as signed in; only the current env's token does.
func TestSignedInRequiresCurrentEnvToken(t *testing.T) {
	c := &Config{CurrentEnv: "dev", Profiles: map[string]*Profile{"prod": {Token: "x"}}}
	if c.SignedIn() {
		t.Error("signed-in should be false when only a non-current env has a token")
	}
	c.Profile("dev").Token = "y"
	if !c.SignedIn() {
		t.Error("signed-in should be true once the current env has a token")
	}
}
