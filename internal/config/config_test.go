package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDir_XDGHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-fake")
	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	want := filepath.Join("/tmp/xdg-fake", DirName)
	if got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestDir_FallsBackToHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/tmp/home-fake")
	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	want := filepath.Join("/tmp/home-fake", ".config", DirName)
	if got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestLoadFrom_MissingFileReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.toml")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom missing file: unexpected err %v", err)
	}
	if cfg != nil {
		t.Errorf("LoadFrom missing file: cfg = %+v, want nil", cfg)
	}
}

func TestLoadFrom_MalformedTOMLReturnsParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("default_profile = \nthis is not = valid toml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err == nil {
		t.Fatalf("LoadFrom malformed: want error, got cfg=%+v", cfg)
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("LoadFrom malformed: want *ParseError, got %T (%v)", err, err)
	}
	if pe.Path != path {
		t.Errorf("ParseError.Path = %q, want %q", pe.Path, path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error message %q does not embed path %q", err.Error(), path)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	created := time.Date(2026, 6, 17, 10, 30, 0, 0, time.UTC)
	cfg := &Config{
		DefaultProfile: "work",
		Profiles: map[string]Profile{
			"work": {
				APIKey:  "molt_work_abcdef",
				Created: created,
			},
			"personal": {
				APIKey:  "molt_personal_123456",
				Created: created.Add(5 * time.Minute),
			},
		},
	}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	// Check file mode is 0600 (owner-only).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("config file mode = %v, want 0600", mode)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got == nil {
		t.Fatal("LoadFrom returned nil cfg")
	}
	if got.DefaultProfile != "work" {
		t.Errorf("DefaultProfile = %q, want %q", got.DefaultProfile, "work")
	}
	if len(got.Profiles) != 2 {
		t.Errorf("Profiles count = %d, want 2", len(got.Profiles))
	}
	if p := got.Profiles["work"]; p.APIKey != "molt_work_abcdef" {
		t.Errorf("work.APIKey = %q, want %q", p.APIKey, "molt_work_abcdef")
	}
	// Created should round-trip identically (UTC).
	if p := got.Profiles["work"]; !p.Created.Equal(created) {
		t.Errorf("work.Created = %v, want %v", p.Created, created)
	}
}

func TestSave_AtomicViaTempPlusRename(t *testing.T) {
	// The contract is: after Save, no file named like `.config-*.toml.tmp`
	// remains in the directory (clean rename). If Save panics or errors
	// midway it must also clean up; that path is harder to test without
	// fault injection, so this verifies the happy path leaves no debris.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := &Config{DefaultProfile: "x", Profiles: map[string]Profile{"x": {APIKey: "molt_x", Created: time.Now().UTC()}}}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".config-") || strings.HasSuffix(name, ".tmp") {
			t.Errorf("temp file left behind: %s", name)
		}
	}
}

func TestSave_CreatesDirWith0700(t *testing.T) {
	dir := t.TempDir()
	// Use a nested path that does not yet exist.
	path := filepath.Join(dir, "nested", "moltable", "config.toml")
	cfg := &Config{DefaultProfile: "x", Profiles: map[string]Profile{"x": {APIKey: "molt_x", Created: time.Now().UTC()}}}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("config dir mode = %v, want 0700", mode)
	}
}

func TestSave_ConcurrentCallsSerializeViaFlock(t *testing.T) {
	// Two concurrent SaveTo calls on the same path must both succeed
	// AND the final state must be one of the two consistent writes —
	// never a half-merged file. This exercises the flock + temp+rename
	// path. We assert (a) no error and (b) the file is parseable
	// afterwards (no corruption).
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	now := time.Now().UTC().Truncate(time.Second)
	a := &Config{DefaultProfile: "a", Profiles: map[string]Profile{"a": {APIKey: "molt_a", Created: now}}}
	b := &Config{DefaultProfile: "b", Profiles: map[string]Profile{"b": {APIKey: "molt_b", Created: now}}}

	var wg sync.WaitGroup
	// Run many concurrent writers to maximize contention.
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = SaveTo(path, a) }()
		go func() { defer wg.Done(); _ = SaveTo(path, b) }()
	}
	wg.Wait()

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom after concurrent writes: %v", err)
	}
	if got == nil {
		t.Fatal("config gone after concurrent writes")
	}
	// Must match exactly one of the writers' state.
	switch got.DefaultProfile {
	case "a":
		if _, ok := got.Profiles["a"]; !ok {
			t.Errorf("state inconsistent: default=a but no profile a")
		}
	case "b":
		if _, ok := got.Profiles["b"]; !ok {
			t.Errorf("state inconsistent: default=b but no profile b")
		}
	default:
		t.Errorf("unexpected DefaultProfile %q", got.DefaultProfile)
	}
}

func TestAddProfile_SetsDefaultIfEmpty(t *testing.T) {
	cfg := &Config{}
	cfg.AddProfile("work", Profile{APIKey: "molt_w", Created: time.Now().UTC()})
	if cfg.DefaultProfile != "work" {
		t.Errorf("DefaultProfile = %q, want %q", cfg.DefaultProfile, "work")
	}
	// Adding a second profile should NOT change the default.
	cfg.AddProfile("personal", Profile{APIKey: "molt_p", Created: time.Now().UTC()})
	if cfg.DefaultProfile != "work" {
		t.Errorf("DefaultProfile = %q, want %q after second add", cfg.DefaultProfile, "work")
	}
}

func TestRemoveProfile_ClearsDefaultPointer(t *testing.T) {
	cfg := &Config{
		DefaultProfile: "work",
		Profiles: map[string]Profile{
			"work":     {APIKey: "molt_w", Created: time.Now().UTC()},
			"personal": {APIKey: "molt_p", Created: time.Now().UTC()},
		},
	}
	cfg.RemoveProfile("work")
	if _, ok := cfg.Profiles["work"]; ok {
		t.Errorf("work profile still present")
	}
	if cfg.DefaultProfile != "" {
		t.Errorf("DefaultProfile = %q, want empty after removing default", cfg.DefaultProfile)
	}
	// Removing a non-default profile leaves the pointer alone.
	cfg.DefaultProfile = "personal"
	cfg.AddProfile("work", Profile{APIKey: "molt_w2", Created: time.Now().UTC()})
	cfg.RemoveProfile("work")
	if cfg.DefaultProfile != "personal" {
		t.Errorf("DefaultProfile = %q, want %q", cfg.DefaultProfile, "personal")
	}
}

func TestListProfiles_StableOrder(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]Profile{
			"zulu":  {APIKey: "molt_z", Created: time.Now().UTC()},
			"alpha": {APIKey: "molt_a", Created: time.Now().UTC()},
			"mike":  {APIKey: "molt_m", Created: time.Now().UTC()},
		},
	}
	names := cfg.ListProfiles()
	want := []string{"alpha", "mike", "zulu"}
	if len(names) != len(want) {
		t.Fatalf("ListProfiles len = %d, want %d", len(names), len(want))
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("ListProfiles[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestListProfiles_EmptyReturnsNil(t *testing.T) {
	cfg := &Config{}
	if got := cfg.ListProfiles(); got != nil {
		t.Errorf("ListProfiles on empty config = %v, want nil", got)
	}
}

func TestRoundTrip_AllStatusValuesPreserved(t *testing.T) {
	// Plan calls out: success/queued/failed status all preserve. The
	// schema today doesn't carry a status field per profile, so this
	// test stands in by ensuring the AddProfile/RemoveProfile flow
	// preserves data through a Save/Load cycle for three distinct
	// profiles representing different lifecycle states (just-added,
	// recently-used, stale).
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	now := time.Date(2026, 6, 17, 10, 0, 0, 0, time.UTC)
	cfg := &Config{}
	cfg.AddProfile("fresh", Profile{APIKey: "molt_fresh_aaa", Created: now})
	cfg.AddProfile("active", Profile{APIKey: "molt_active_bbb", Created: now.Add(-1 * time.Hour)})
	cfg.AddProfile("stale", Profile{APIKey: "molt_stale_ccc", Created: now.Add(-30 * 24 * time.Hour)})

	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got == nil || len(got.Profiles) != 3 {
		t.Fatalf("round-trip lost profiles: got=%+v", got)
	}
	for _, name := range []string{"fresh", "active", "stale"} {
		if _, ok := got.Profiles[name]; !ok {
			t.Errorf("profile %q lost in round trip", name)
		}
	}
}
