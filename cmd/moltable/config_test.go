package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moltable/cli/internal/config"
)

// writeTempConfig drops a fully-formed TOML config into tmpDir and
// returns its path. Used by the config tests to drive `moltable config`
// against a known-good shape without simulating browser handoff.
func writeTempConfig(t *testing.T, dir string, cfg *config.Config) string {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	if err := config.SaveTo(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return path
}

func TestConfigShow_EmptyConfigFriendly(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	// File does NOT exist on purpose.
	code, stdout, stderr := runCLI(t, "--config", cfgPath, "config", "show")
	if code != 0 {
		t.Fatalf("exit = %d; want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no profiles configured") {
		t.Fatalf("stdout = %q; want 'no profiles configured'", stdout.String())
	}
}

func TestConfigShow_JSONSanitizesAPIKeys(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	cfgPath := writeTempConfig(t, dir, &config.Config{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work": {APIKey: "molt_super_secret_dont_print_this_full_key"},
		},
	})
	code, stdout, stderr := runCLI(t, "--config", cfgPath, "config", "show", "--json")
	if code != 0 {
		t.Fatalf("exit = %d; want 0; stderr=%s", code, stderr.String())
	}

	var got struct {
		DefaultProfile string `json:"default_profile"`
		Profiles       map[string]struct {
			APIKey string `json:"api_key"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\nstdout=%s", err, stdout.String())
	}
	if got.DefaultProfile != "work" {
		t.Fatalf("default_profile = %q; want work", got.DefaultProfile)
	}
	work := got.Profiles["work"]
	if strings.Contains(work.APIKey, "super_secret") {
		t.Fatalf("api_key %q contains the secret body; must be sanitized", work.APIKey)
	}
	if !strings.HasPrefix(work.APIKey, "molt_") {
		t.Fatalf("api_key %q lost the molt_ prefix", work.APIKey)
	}
}

func TestConfigGet_APIKeyReturnsResolvedKey(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	cfgPath := writeTempConfig(t, dir, &config.Config{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work": {APIKey: "molt_abcdef123456"},
		},
	})
	code, stdout, stderr := runCLI(t, "--config", cfgPath, "config", "get", "api-key")
	if code != 0 {
		t.Fatalf("exit = %d; want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "molt_abcdef123456" {
		t.Fatalf("stdout = %q; want molt_abcdef123456", got)
	}
}

func TestConfigGet_APIKeyNoProfileErrors(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	// Stop env vars from leaking the real user's key into the test.
	t.Setenv("MOLTABLE_API_KEY", "")
	t.Setenv("MOLTABLE_PROFILE", "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	// File missing → resolve fails with ErrNoAuth.
	code, _, stderr := runCLI(t, "--config", cfgPath, "config", "get", "api-key")
	if code != 2 {
		t.Fatalf("exit = %d; want 2 (auth)", code)
	}
	if !strings.Contains(stderr.String(), "moltable auth login") {
		t.Fatalf("stderr = %q; want a 'moltable auth login' hint", stderr.String())
	}
}

func TestConfigGet_DefaultProfile(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	cfgPath := writeTempConfig(t, dir, &config.Config{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work": {APIKey: "molt_abc"},
		},
	})
	code, stdout, _ := runCLI(t, "--config", cfgPath, "config", "get", "default-profile")
	if code != 0 {
		t.Fatalf("exit = %d; want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "work" {
		t.Fatalf("stdout = %q; want work", got)
	}
}

func TestConfigSet_DefaultProfileUpdatesConfig(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	cfgPath := writeTempConfig(t, dir, &config.Config{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work":     {APIKey: "molt_abc"},
			"personal": {APIKey: "molt_def"},
		},
	})
	code, _, stderr := runCLI(t, "--config", cfgPath, "config", "set", "default-profile", "personal")
	if code != 0 {
		t.Fatalf("exit = %d; want 0; stderr=%s", code, stderr.String())
	}
	// Reload and assert the change persisted.
	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.DefaultProfile != "personal" {
		t.Fatalf("default_profile = %q; want personal", cfg.DefaultProfile)
	}
}

func TestConfigSet_UnknownProfileErrors(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	cfgPath := writeTempConfig(t, dir, &config.Config{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work": {APIKey: "molt_abc"},
		},
	})
	code, _, stderr := runCLI(t, "--config", cfgPath, "config", "set", "default-profile", "nonexistent")
	if code != 3 {
		t.Fatalf("exit = %d; want 3 (not found); stderr=%s", code, stderr.String())
	}
}

func TestConfigGet_UnknownKeyErrors(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	cfgPath := writeTempConfig(t, dir, &config.Config{
		Profiles: map[string]config.Profile{"work": {APIKey: "molt_abc"}},
	})
	code, _, stderr := runCLI(t, "--config", cfgPath, "config", "get", "garbage-key")
	if code != 1 {
		t.Fatalf("exit = %d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown key") {
		t.Fatalf("stderr = %q; want mention of unknown key", stderr.String())
	}
}

// TestConfigGet_APIKeyFlagOverride confirms --api-key wins over the
// configured profile. The long-tail fallback skill uses this path so a
// scripted caller can pass a key without writing it to disk.
func TestConfigGet_APIKeyFlagOverride(t *testing.T) {
	t.Setenv("MOLTABLE_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	cfgPath := writeTempConfig(t, dir, &config.Config{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work": {APIKey: "molt_from_config"},
		},
	})
	code, stdout, _ := runCLI(t, "--config", cfgPath, "--api-key", "molt_from_flag", "config", "get", "api-key")
	if code != 0 {
		t.Fatalf("exit = %d; want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "molt_from_flag" {
		t.Fatalf("stdout = %q; want molt_from_flag (flag should win)", got)
	}
}

// Ensure os import is referenced even when tests don't need it; some
// runners flag unreferenced imports.
var _ = os.Getenv
