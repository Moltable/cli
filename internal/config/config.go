// Package config handles reading and writing the moltable CLI's TOML
// configuration file (profiles + default profile pointer).
//
// Config location resolution honors XDG_CONFIG_HOME first, then falls
// back to $HOME/.config/moltable/. The file is always
// `<dir>/config.toml`. Writes are atomic: temp file + rename, with a
// process-cooperative advisory file lock on `<dir>/config.toml.lock`
// (via github.com/gofrs/flock) so concurrent `moltable auth login`
// calls from multiple terminals don't lose a profile.
//
// Schema:
//
//	default_profile = "work"
//
//	[profiles.work]
//	api_key = "molt_..."
//	created = 2026-06-17T10:00:00Z
//	email = "alice@example.com"   # optional, populated by auth login
//	org_id = "org_..."            # optional, populated by auth login
//
//	[profiles.personal]
//	api_key = "molt_..."
//	created = 2026-06-17T10:05:00Z
//
// `email` and `org_id` are written by `auth login` from the /v1/me
// response so `profile list` can show which profile maps to which
// org without re-authing. Profiles written before this was added
// (or by future flows that skip /v1/me) leave the fields empty;
// readers should fall back to an "unknown" placeholder.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	toml "github.com/pelletier/go-toml/v2"
)

// FileName is the bare TOML file name inside the config directory.
const FileName = "config.toml"

// LockFileName is the advisory lock companion file.
const LockFileName = "config.toml.lock"

// DirName is the directory under XDG_CONFIG_HOME / ~/.config that
// holds the moltable CLI config + state.
const DirName = "moltable"

// Profile is a single named credential entry.
//
// Email and OrgID are populated by `auth login` from the /v1/me response
// so `profile list` can show which profile belongs to which org without
// requiring a round-trip per profile. They are optional + omitempty for
// backward-compat: profiles written before these fields existed (or by
// flows that don't call /v1/me) simply leave them empty, and readers
// render "—" to signal "unknown, re-auth to populate."
type Profile struct {
	APIKey  string    `toml:"api_key"`
	Created time.Time `toml:"created"`
	Email   string    `toml:"email,omitempty"`
	OrgID   string    `toml:"org_id,omitempty"`
}

// Config is the on-disk shape of `config.toml`.
type Config struct {
	DefaultProfile string             `toml:"default_profile,omitempty"`
	Profiles       map[string]Profile `toml:"profiles,omitempty"`
}

// ParseError wraps a malformed-TOML failure with the file path so the
// user can find and fix the file directly.
type ParseError struct {
	Path string
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("config: failed to parse %s: %v", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// Dir returns the absolute path to the directory that should hold the
// moltable config file. It honors XDG_CONFIG_HOME, then falls back to
// $HOME/.config/moltable. It does NOT create the directory.
func Dir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, DirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", DirName), nil
}

// Path returns the absolute path to config.toml within the resolved
// config directory.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads the config file. If the file does not exist, Load
// returns (nil, nil) — callers handle "no profile configured"
// gracefully (e.g. via the auth resolver's ErrNoAuth).
//
// On malformed TOML, Load returns a *ParseError carrying the file
// path so the user can repair it directly.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return LoadFrom(path)
}

// LoadFrom is the testable form of Load that accepts an explicit path.
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, &ParseError{Path: path, Err: err}
	}
	return &cfg, nil
}

// Save atomically writes the config to disk under an advisory file
// lock. The write sequence is:
//
//  1. mkdir -p the config dir (0700)
//  2. flock the lock file (blocking, single-process holds it)
//  3. write to a sibling temp file (0600)
//  4. rename temp → config.toml (atomic on POSIX)
//  5. release flock
//
// The lock ensures two concurrent `moltable auth login` invocations
// don't both read pre-write state and clobber each other; the rename
// ensures a reader never sees a half-written file.
func Save(cfg *Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return SaveTo(path, cfg)
}

// SaveTo is the testable form of Save that accepts an explicit path.
// The lock file lives next to the config file at
// `<path>.lock` (i.e. config.toml.lock when path ends in config.toml).
func SaveTo(path string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config: refuse to write nil config")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", dir, err)
	}

	lockPath := path + ".lock"
	lk := flock.New(lockPath)
	if err := lk.Lock(); err != nil {
		return fmt.Errorf("config: acquire lock %s: %w", lockPath, err)
	}
	defer func() { _ = lk.Unlock() }()

	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: marshal toml: %w", err)
	}

	// Temp file in same dir so rename is atomic (same filesystem).
	tmp, err := os.CreateTemp(dir, ".config-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Ensure cleanup if we error before rename.
	cleanup := func() { _ = os.Remove(tmpPath) }

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("config: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("config: rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}

// AddProfile inserts or replaces a profile by name. If the config
// has no DefaultProfile, the new profile becomes the default.
// Returns a mutated copy; does not write to disk.
func (c *Config) AddProfile(name string, p Profile) {
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	c.Profiles[name] = p
	if c.DefaultProfile == "" {
		c.DefaultProfile = name
	}
}

// RemoveProfile deletes a profile by name. If the removed profile was
// the default, the default pointer is cleared (the caller can pick a
// new default if desired).
func (c *Config) RemoveProfile(name string) {
	delete(c.Profiles, name)
	if c.DefaultProfile == name {
		c.DefaultProfile = ""
	}
}

// ListProfiles returns the profile names in stable lexicographic order
// so `moltable profile list` output is deterministic.
func (c *Config) ListProfiles() []string {
	if len(c.Profiles) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	// Inline sort to avoid an extra import just for sort.Strings.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}
