package config

import (
	"os"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	in := &Config{Env: "dev", Email: "a@b.com", Token: "tok123", ActiveClientID: "edge_1"}
	if err := in.Save(); err != nil {
		t.Fatal(err)
	}
	out, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if *out != *in {
		t.Fatalf("round-trip mismatch: %+v != %+v", out, in)
	}
	if !out.SignedIn() {
		t.Error("SignedIn should be true when a token is present")
	}
}

func TestSaveIs0600(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	if err := (&Config{Token: "secret"}).Save(); err != nil {
		t.Fatal(err)
	}
	p, _ := Path()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600 (it holds a token)", fi.Mode().Perm())
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
	_ = (&Config{Token: "x"}).Save()
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
