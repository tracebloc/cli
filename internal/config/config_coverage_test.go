package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearHomeAndConfigDir removes every source of a config dir so Dir()/Path()
// fail — the lever that covers the error-propagation branches of Dir, Path,
// Load and Save (os.UserHomeDir errors when $HOME is empty).
func clearHomeAndConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("TRACEBLOC_CONFIG_DIR", "")
	t.Setenv("HOME", "")
}

func TestCurrent_EmptyAndSet(t *testing.T) {
	// No current env → a fresh empty profile (the read-only "not signed in" view),
	// never nil.
	if got := (&Config{}).Current(); got == nil || *got != (Profile{}) {
		t.Errorf("Current() with no env = %+v, want a fresh empty Profile", got)
	}
	// With a current env → that env's live profile.
	c := &Config{CurrentEnv: "dev", Profiles: map[string]*Profile{"dev": {Token: "t"}}}
	if got := c.Current(); got == nil || got.Token != "t" {
		t.Errorf("Current() = %+v, want the dev profile", got)
	}
}

func TestProfile_NilMapAndReuse(t *testing.T) {
	c := &Config{} // nil Profiles map
	p := c.Profile("dev")
	if p == nil {
		t.Fatal("Profile must create and store an empty profile")
	}
	if c.Profiles == nil {
		t.Error("Profile must initialize the Profiles map")
	}
	p.Token = "x"
	if c.Profile("dev") != p {
		t.Error("Profile must return the same live pointer on re-fetch")
	}
}

func TestDir_DefaultAndError(t *testing.T) {
	// Default (no override) → <home>/.tracebloc.
	t.Setenv("TRACEBLOC_CONFIG_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir with a home set: %v", err)
	}
	if want := filepath.Join(home, ".tracebloc"); dir != want {
		t.Errorf("Dir() = %q, want %q", dir, want)
	}
	// No override AND no home → UserHomeDir fails.
	clearHomeAndConfigDir(t)
	if _, err := Dir(); err == nil {
		t.Error("Dir() with no home must error")
	}
}

func TestPath_Error(t *testing.T) {
	clearHomeAndConfigDir(t)
	if _, err := Path(); err == nil {
		t.Error("Path() must propagate Dir()'s error")
	}
}

// TestLoad_ConfigUnmarshalError covers the SECOND unmarshal (into Config): the
// probe parses (version 2, non-empty profiles → no migrate) but the full decode
// fails because profiles is a JSON array, not an object.
func TestLoad_ConfigUnmarshalError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"version":2,"profiles":[1,2]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "parsing") {
		t.Errorf("a v2 file with a non-object profiles must fail to parse, got %v", err)
	}
}

// TestLoad_NullProfilesGetsEmptyMap covers the `c.Profiles == nil` → empty-map
// arm (a v2 file that omits the profiles object).
func TestLoad_NullProfilesGetsEmptyMap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"version":2,"current_env":"dev"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Profiles == nil {
		t.Error("Load must initialize a nil Profiles map to empty")
	}
}

func TestLoad_HomeError(t *testing.T) {
	clearHomeAndConfigDir(t)
	if _, err := Load(); err == nil {
		t.Error("Load() must propagate Path()'s error")
	}
}

// TestMigrateV1_UnmarshalError covers migrateV1's own decode-failure arm: a file
// that probes as v1 (no version, no profiles) but whose fields don't fit the v1
// struct (env as a number, not a string).
func TestMigrateV1_UnmarshalError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"env":123}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "parsing") {
		t.Errorf("a v1 record with a non-string env must fail to migrate-parse, got %v", err)
	}
}

func TestSave_ErrorBranches(t *testing.T) {
	t.Run("Dir error propagates", func(t *testing.T) {
		clearHomeAndConfigDir(t)
		if err := (&Config{CurrentEnv: "dev", Profiles: map[string]*Profile{"dev": {Token: "t"}}}).Save(); err == nil {
			t.Error("Save() with no home must error")
		}
	})
	t.Run("rename onto a non-empty directory fails", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
		// config.json as a NON-EMPTY directory → the final os.Rename(tmp, path) fails.
		cfgDir := filepath.Join(dir, "config.json")
		if err := os.Mkdir(cfgDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "child"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := (&Config{CurrentEnv: "dev", Profiles: map[string]*Profile{"dev": {Token: "t"}}}).Save(); err == nil {
			t.Error("Save must fail when the rename target is a non-empty directory")
		}
	})
}
