// Package config persists the tracebloc CLI's user state — the backend
// environment, the user token from `tracebloc login`, and the active client
// for this machine — at ~/.tracebloc/config.json (mode 0600, it holds a
// token). RFC-0001 (backend#830).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Config is the on-disk CLI state. Every field is omitempty so a
// partially-configured file stays small and forward-compatible.
type Config struct {
	Env            string `json:"env,omitempty"`              // dev|stg|prod (mirrors CLIENT_ENV)
	Email          string `json:"email,omitempty"`            // who is signed in (display only)
	Token          string `json:"token,omitempty"`            // user token from device login
	ActiveClientID string `json:"active_client_id,omitempty"` // client this machine enrolls as
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

// Load reads the config. A missing file is NOT an error — it returns an empty
// Config (a fresh machine that has never run `login`).
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the config 0600 (creating the dir 0700), atomically: a temp file
// in the same dir then a rename, so a crash mid-write can't truncate the token.
func (c *Config) Save() error {
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

// Clear removes the config file (full sign-out + reset). A missing file is not
// an error.
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

// SignedIn reports whether a user token is stored.
func (c *Config) SignedIn() bool { return c.Token != "" }
