package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moltable/cli/internal/config"
)

// seedProfiles writes a config with the supplied profiles + default
// into the given path. Used by every profile-cmd test as setup.
func seedProfiles(t *testing.T, path string, def string, profiles map[string]config.Profile) {
	t.Helper()
	cfg := &config.Config{DefaultProfile: def, Profiles: profiles}
	if err := config.SaveTo(path, cfg); err != nil {
		t.Fatalf("seed profiles: %v", err)
	}
}

// --- profile list ------------------------------------------------

func TestProfileList_EmptyConfigPrintsHelpfulMessage(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	code, stdout, _ := runCLI(t, "--config", cfgPath, "profile", "list")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "No profiles configured") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestProfileList_EmptyConfigJSONIsEmptyArray(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	code, stdout, _ := runCLI(t, "--config", cfgPath, "profile", "list", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	body := strings.TrimSpace(stdout.String())
	if body != "[]" {
		t.Errorf("body = %q; want %q", body, "[]")
	}
}

func TestProfileList_JSONShape(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	now := time.Now().UTC().Truncate(time.Second)
	seedProfiles(t, cfgPath, "work", map[string]config.Profile{
		"work":     {APIKey: "molt_work_key", Created: now},
		"personal": {APIKey: "molt_personal_key", Created: now},
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "profile", "list", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	var arr []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &arr); err != nil {
		t.Fatalf("unmarshal: %v; body=%q", err, stdout.String())
	}
	if len(arr) != 2 {
		t.Fatalf("len(arr) = %d", len(arr))
	}

	// Lexicographic order means "personal" before "work".
	if arr[0]["name"] != "personal" || arr[1]["name"] != "work" {
		t.Errorf("order = %v %v; want personal,work", arr[0]["name"], arr[1]["name"])
	}
	if arr[1]["default"] != true {
		t.Errorf("work.default = %v; want true", arr[1]["default"])
	}
	if arr[0]["default"] != false {
		t.Errorf("personal.default = %v; want false", arr[0]["default"])
	}

	// Critically: no api_key field leaks into the JSON.
	for _, p := range arr {
		if _, leaked := p["api_key"]; leaked {
			t.Errorf("api_key leaked in JSON: %v", p)
		}
	}

	// Each entry must carry name/default/created keys.
	for _, p := range arr {
		for _, k := range []string{"name", "default", "created"} {
			if _, ok := p[k]; !ok {
				t.Errorf("missing key %q in entry %v", k, p)
			}
		}
	}
}

// TestProfileList_HumanShowsEmailAndOrg pins that the human table
// includes EMAIL + ORG columns and the values render when populated.
func TestProfileList_HumanShowsEmailAndOrg(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	now := time.Now().UTC().Truncate(time.Second)
	seedProfiles(t, cfgPath, "work", map[string]config.Profile{
		"work": {
			APIKey: "molt_work_key", Created: now,
			Email: "alice@example.com", OrgID: "org_42",
		},
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "profile", "list")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"EMAIL", "ORG", "alice@example.com", "org_42"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q: %q", want, out)
		}
	}
}

// TestProfileList_HumanFallsBackToDashWhenMissing pins the back-compat
// rendering for profiles authed before email/org_id persistence. The
// fields are empty in the TOML; the human table renders "—" so the
// user knows the profile predates the metadata (re-auth populates it).
func TestProfileList_HumanFallsBackToDashWhenMissing(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	now := time.Now().UTC().Truncate(time.Second)
	seedProfiles(t, cfgPath, "legacy", map[string]config.Profile{
		"legacy": {APIKey: "molt_legacy_key", Created: now}, // no Email / OrgID
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "profile", "list")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	// Two "—" cells in the legacy row (one per missing field).
	if got := strings.Count(stdout.String(), "—"); got < 2 {
		t.Errorf("expected at least 2 dash placeholders; got %d in %q", got, stdout.String())
	}
}

// TestProfileList_JSONOmitsEmptyEmailOrgViaOmitempty pins the JSON
// shape: when email/org_id are unset, the keys are omitted (no empty
// strings in the response). When they're set, they're present.
func TestProfileList_JSONOmitsEmptyEmailOrgViaOmitempty(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	now := time.Now().UTC().Truncate(time.Second)
	seedProfiles(t, cfgPath, "work", map[string]config.Profile{
		"work":   {APIKey: "k1", Created: now, Email: "a@b.c", OrgID: "org_z"},
		"legacy": {APIKey: "k2", Created: now}, // no email/org
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "profile", "list", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var arr []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &arr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byName := map[string]map[string]any{}
	for _, p := range arr {
		byName[p["name"].(string)] = p
	}
	if byName["work"]["email"] != "a@b.c" || byName["work"]["org_id"] != "org_z" {
		t.Errorf("work profile missing email/org_id: %v", byName["work"])
	}
	if _, ok := byName["legacy"]["email"]; ok {
		t.Errorf("legacy.email should be omitted via omitempty: %v", byName["legacy"])
	}
	if _, ok := byName["legacy"]["org_id"]; ok {
		t.Errorf("legacy.org_id should be omitted via omitempty: %v", byName["legacy"])
	}
}

// --- profile use -------------------------------------------------

func TestProfileUse_SwitchesDefault(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	now := time.Now().UTC()
	seedProfiles(t, cfgPath, "personal", map[string]config.Profile{
		"work":     {APIKey: "molt_work_key", Created: now},
		"personal": {APIKey: "molt_personal_key", Created: now},
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "profile", "use", "work")
	if code != 0 {
		t.Fatalf("exit = %d; stdout = %q", code, stdout.String())
	}

	cfg, _ := config.LoadFrom(cfgPath)
	if cfg.DefaultProfile != "work" {
		t.Errorf("DefaultProfile = %q", cfg.DefaultProfile)
	}
}

func TestProfileUse_MissingErrors(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	now := time.Now().UTC()
	seedProfiles(t, cfgPath, "work", map[string]config.Profile{
		"work": {APIKey: "molt_work_key", Created: now},
	})

	code, _, stderr := runCLI(t, "--config", cfgPath, "profile", "use", "missing")
	if code == 0 {
		t.Fatal("exit = 0; want non-zero on missing profile")
	}
	s := stderr.String()
	if !strings.Contains(s, `"missing" not found`) && !strings.Contains(s, "Profile \"missing\" not found") {
		t.Errorf("stderr missing 'not found' message: %q", s)
	}
	if !strings.Contains(s, "profile list") {
		t.Errorf("stderr missing 'profile list' hint: %q", s)
	}
}

// --- profile remove ----------------------------------------------

func TestProfileRemove_DefaultWithOthersErrors(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	now := time.Now().UTC()
	seedProfiles(t, cfgPath, "work", map[string]config.Profile{
		"work":     {APIKey: "molt_work", Created: now},
		"personal": {APIKey: "molt_personal", Created: now},
	})

	code, _, stderr := runCLI(t, "--config", cfgPath, "profile", "remove", "work")
	if code == 0 {
		t.Fatal("exit = 0; want non-zero — cannot remove default with siblings")
	}
	if !strings.Contains(stderr.String(), "Switch default first") {
		t.Errorf("stderr missing 'Switch default first' hint: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "moltable profile use") {
		t.Errorf("stderr missing 'moltable profile use' suggestion: %q", stderr.String())
	}

	// Verify config was NOT mutated.
	cfg, _ := config.LoadFrom(cfgPath)
	if _, ok := cfg.Profiles["work"]; !ok {
		t.Error("work was removed despite error")
	}
}

func TestProfileRemove_NonDefaultSucceeds(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	now := time.Now().UTC()
	seedProfiles(t, cfgPath, "work", map[string]config.Profile{
		"work":     {APIKey: "molt_work", Created: now},
		"personal": {APIKey: "molt_personal", Created: now},
	})

	code, _, stderr := runCLI(t, "--config", cfgPath, "profile", "remove", "personal")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "still active") {
		t.Errorf("stderr missing security warning: %q", stderr.String())
	}

	cfg, _ := config.LoadFrom(cfgPath)
	if _, ok := cfg.Profiles["personal"]; ok {
		t.Error("personal not removed")
	}
	if cfg.DefaultProfile != "work" {
		t.Errorf("DefaultProfile changed to %q", cfg.DefaultProfile)
	}
}

func TestProfileRemove_SoleProfileClearsDefault(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	now := time.Now().UTC()
	seedProfiles(t, cfgPath, "only", map[string]config.Profile{
		"only": {APIKey: "molt_only_key", Created: now},
	})

	code, _, _ := runCLI(t, "--config", cfgPath, "profile", "remove", "only")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	cfg, _ := config.LoadFrom(cfgPath)
	if _, ok := cfg.Profiles["only"]; ok {
		t.Error("sole profile not removed")
	}
	if cfg.DefaultProfile != "" {
		t.Errorf("DefaultProfile = %q; want empty after removing sole profile", cfg.DefaultProfile)
	}
}

func TestProfileRemove_MissingErrors(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	now := time.Now().UTC()
	seedProfiles(t, cfgPath, "work", map[string]config.Profile{
		"work": {APIKey: "molt_work", Created: now},
	})

	code, _, stderr := runCLI(t, "--config", cfgPath, "profile", "remove", "ghost")
	if code == 0 {
		t.Fatal("exit = 0; want non-zero on missing profile")
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr missing 'not found': %q", stderr.String())
	}
}
