package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoad_Faults pins the corrupt / unreadable branches of Load (config.go:120,
// was 74% — only happy/missing/migrate were covered). Token-persistence
// integrity: a garbled or unreadable config must surface a clear error, never a
// silent empty config that reads as "not signed in".
func TestLoad_Faults(t *testing.T) {
	t.Run("corrupt JSON -> parse error", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{ not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "parsing") {
			t.Fatalf("corrupt config must fail to parse, got %v", err)
		}
	})
	t.Run("unreadable path (a directory) -> read error", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
		if err := os.Mkdir(filepath.Join(dir, "config.json"), 0o700); err != nil { // config.json is a dir
			t.Fatal(err)
		}
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "reading") {
			t.Fatalf("an unreadable config must surface a read error, got %v", err)
		}
	})
}

// TestSave_Faults pins Save's fault + prune arms (config.go:190, was ~65%). The
// atomic-write path is the most safety-relevant in the CLI — a failure must
// error, never truncate the token file.
func TestSave_Faults(t *testing.T) {
	t.Run("un-creatable dir (parent is a file) -> error", func(t *testing.T) {
		tmp := t.TempDir()
		blocker := filepath.Join(tmp, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TRACEBLOC_CONFIG_DIR", filepath.Join(blocker, "sub")) // parent is a file
		err := (&Config{CurrentEnv: "dev", Profiles: map[string]*Profile{"dev": {Token: "t"}}}).Save()
		if err == nil || !strings.Contains(err.Error(), "creating") {
			t.Fatalf("Save into an un-creatable dir must error, got %v", err)
		}
	})
	t.Run("read-only dir -> temp-file creation error", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod-based write-fault needs a non-root euid")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil { // r-x: MkdirAll no-ops, CreateTemp fails
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // let t.TempDir clean up
		t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
		if err := (&Config{CurrentEnv: "dev", Profiles: map[string]*Profile{"dev": {Token: "t"}}}).Save(); err == nil {
			t.Fatal("Save into a read-only dir must error, not silently succeed")
		}
	})
	t.Run("prunes fully-empty profiles", func(t *testing.T) {
		t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
		c := &Config{CurrentEnv: "dev", Profiles: map[string]*Profile{
			"dev": {Token: "keep"},
			"stg": {}, // fully empty → pruned
		}}
		if err := c.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if _, ok := c.Profiles["stg"]; ok {
			t.Error("a fully-empty profile must be pruned on Save")
		}
		if _, ok := c.Profiles["dev"]; !ok {
			t.Error("a non-empty profile must survive Save")
		}
		// And it round-trips: the pruned profile is absent on reload.
		got, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if _, ok := got.Profiles["stg"]; ok {
			t.Error("the pruned profile must not be on disk")
		}
	})
}

// TestClearAll pins clearAll (config.go:242, was 67%): remove the config,
// tolerate a missing file, and surface a genuine remove failure.
func TestClearAll(t *testing.T) {
	t.Run("removes an existing config", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := clearAll(); err != nil {
			t.Fatalf("clearAll: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("config must be gone after clearAll")
		}
	})
	t.Run("missing config is a no-op", func(t *testing.T) {
		t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
		if err := clearAll(); err != nil {
			t.Errorf("a missing config must clear cleanly, got %v", err)
		}
	})
	t.Run("un-removable path -> error", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("TRACEBLOC_CONFIG_DIR", dir)
		// config.json as a NON-EMPTY directory → os.Remove refuses it.
		cfgDir := filepath.Join(dir, "config.json")
		if err := os.Mkdir(cfgDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "child"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := clearAll(); err == nil {
			t.Error("a non-empty directory at the config path must surface a remove error")
		}
	})
}
