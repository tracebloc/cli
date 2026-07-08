// Package config persists the tracebloc CLI's user state at
// ~/.tracebloc/config.json (mode 0600, it holds tokens). RFC-0001 (backend#830).
//
// The on-disk format is v2 (RFC-0001 Appendix C.8): env-scoped profiles, so a
// user signed into dev / stg / prod keeps an independent token + active-client
// pointer per env. `login --env X` switches current_env without touching the
// other profiles — fixing the v1 bug (R10) where the single flat
// {env,token,active_client_id} record let a `login --env` strand the previous
// env's active_client_id and silently target the wrong client.
//
//	{ "version": 2, "current_env": "prod",
//	  "profiles": {
//	    "dev":  { "email", "token", "expires_at", "active_client_id" },
//	    "stg":  { … }, "prod": { … } } }
//
// A v1 file (the flat cli#83 schema) auto-migrates to v2 on first read, with no
// data loss — its single record is wrapped under profiles[env].
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// schemaVersion is the current on-disk format. Anything lower (incl. a v1 file
// with no "version" key, which decodes as 0) is migrated on Load.
const schemaVersion = 2

// defaultEnv is the env a v1 record migrates under when its env was unset — the
// historical v1 default (mirrors api.EnvProd; kept literal to avoid importing api
// into this lower-level package).
const defaultEnv = "prod"

// Profile is one env's signed-in state. Every field is omitempty so a
// partially-configured profile stays small and forward-compatible.
type Profile struct {
	Email          string `json:"email,omitempty"`            // who is signed in (display only)
	FirstName      string `json:"first_name,omitempty"`       // signed-in user's first name; auto-names clients (cli#137)
	Token          string `json:"token,omitempty"`            // user token from device login
	ExpiresAt      string `json:"expires_at,omitempty"`       // token expiry (RFC 3339), when known
	ActiveClientID string `json:"active_client_id,omitempty"` // client this machine enrolls as, for THIS env

	// ActiveClientNamespace + ActiveClientName cache the active client's k8s
	// namespace and display name at `client create` time, so the data
	// commands can bind to the active client's cluster (RFC-0001 §7.3) without
	// a backend round-trip — they run cluster-local and may be offline. Empty
	// when no client is active or for pre-v2 configs that predate the cache.
	ActiveClientNamespace string `json:"active_client_namespace,omitempty"`
	ActiveClientName      string `json:"active_client_name,omitempty"`
}

// Config is the on-disk CLI state: env-scoped profiles plus the current env.
type Config struct {
	Version    int                 `json:"version"`
	CurrentEnv string              `json:"current_env,omitempty"`
	Profiles   map[string]*Profile `json:"profiles,omitempty"`
}

// Profile returns env's profile, creating an empty one (stored in the map) if
// absent. The returned pointer is live: mutate it then Save() to persist.
func (c *Config) Profile(env string) *Profile {
	if c.Profiles == nil {
		c.Profiles = map[string]*Profile{}
	}
	p := c.Profiles[env]
	if p == nil {
		p = &Profile{}
		c.Profiles[env] = p
	}
	return p
}

// Current returns the profile for the current env. When no env is selected it
// returns a fresh empty profile (a read-only "not signed in" view) rather than
// nil, so callers can read fields without a guard.
func (c *Config) Current() *Profile {
	if c.CurrentEnv == "" {
		return &Profile{}
	}
	return c.Profile(c.CurrentEnv)
}

// SignedIn reports whether the current env has a stored token.
func (c *Config) SignedIn() bool {
	if c.CurrentEnv == "" {
		return false
	}
	p := c.Profiles[c.CurrentEnv]
	return p != nil && p.Token != ""
}

// Dir is the config directory: $TRACEBLOC_CONFIG_DIR if set (tests / ops
// override), else ~/.tracebloc.
func Dir() (string, error) {
	if d := os.Getenv("TRACEBLOC_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".tracebloc"), nil
}

// Path is the config file path (Dir()/config.json).
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the config, migrating a v1 file to v2 in memory. A missing file is
// NOT an error — it returns an empty v2 Config (a machine that's never run login).
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{Version: schemaVersion, Profiles: map[string]*Profile{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Detect the on-disk schema. v1 (cli#83) had no "version" key and a flat
	// {env,email,token,active_client_id}; it decodes here as version 0. Migrate
	// only a GENUINE v1 record: an old version AND no v2 `profiles` object — so a
	// v2-shaped file with a missing/wrong version is still parsed as v2 and never
	// has its profiles silently dropped by migrateV1.
	var probe struct {
		Version  int             `json:"version"`
		Profiles json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if probe.Version < schemaVersion && len(probe.Profiles) == 0 {
		return migrateV1(data, path)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if c.Profiles == nil {
		c.Profiles = map[string]*Profile{}
	}
	return &c, nil
}

// migrateV1 wraps a v1 flat record under profiles[env] (RFC-0001 Appendix C.8),
// with no data loss. The first Save rewrites the file as v2.
func migrateV1(data []byte, path string) (*Config, error) {
	var v1 struct {
		Env            string `json:"env"`
		Email          string `json:"email"`
		Token          string `json:"token"`
		ActiveClientID string `json:"active_client_id"`
	}
	if err := json.Unmarshal(data, &v1); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	c := &Config{Version: schemaVersion, Profiles: map[string]*Profile{}}
	// A signed-in v1 record carries over under its env; an empty / logged-out v1
	// file just becomes an empty v2 (nothing to migrate).
	if v1.Token != "" || v1.Email != "" || v1.ActiveClientID != "" {
		env := v1.Env
		if env == "" {
			env = defaultEnv
		}
		c.CurrentEnv = env
		c.Profiles[env] = &Profile{
			Email:          v1.Email,
			Token:          v1.Token,
			ActiveClientID: v1.ActiveClientID,
		}
	}
	return c, nil
}

// Save writes the config 0600 (creating the dir 0700), atomically: a temp file
// in the same dir then a rename, so a crash mid-write can't truncate a token.
func (c *Config) Save() error {
	c.Version = schemaVersion
	// Prune fully-empty profiles (e.g. the current env's profile after logout
	// clears it) so the file stays tidy; an absent profile reads as "not signed
	// in" for that env.
	for env, p := range c.Profiles {
		if p == nil || *p == (Profile{}) {
			delete(c.Profiles, env)
		}
	}

	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp config: %w", err)
	}
	path, _ := Path()
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}

// Clear removes the config file (full sign-out + reset, all envs). A missing
// file is not an error.
func Clear() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}
