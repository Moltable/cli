package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moltable/cli/internal/config"
	"github.com/moltable/cli/internal/handoff"
)

// fakeLoginRunner returns an authLoginRunner closure that yields a
// canned LoginResult, recording the api base it was asked to call
// against so tests can assert on env-var threading. The new `label`
// arg matches the production runner's signature; tests that care
// about the label can extend this closure.
func fakeLoginRunner(key string, calls *int) func(context.Context, string, bool, string, *os.File) (*handoff.LoginResult, error) {
	return func(ctx context.Context, apiBase string, dev bool, label string, stderr *os.File) (*handoff.LoginResult, error) {
		if calls != nil {
			*calls++
		}
		return &handoff.LoginResult{APIKey: key, KeyID: "key_test"}, nil
	}
}

// withLoginRunner swaps the package-level authLoginRunner for the
// duration of t and restores on cleanup. Mirrors how config_test.go
// uses t.Setenv to scope env mutations.
func withLoginRunner(t *testing.T, runner func(context.Context, string, bool, string, *os.File) (*handoff.LoginResult, error)) {
	t.Helper()
	prev := authLoginRunner
	authLoginRunner = runner
	t.Cleanup(func() { authLoginRunner = prev })
}

// withAPIServer stands up an httptest server that always returns a
// canned /v1/me payload. Sets MOLTABLE_API_BASE so the cmd layer's
// resolveAPIBase points at it.
func withAPIServer(t *testing.T, email, orgID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"user_id": "user_x",
				"org_id":  orgID,
				"email":   email,
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Setenv(envAPIBase, srv.URL)
	t.Cleanup(srv.Close)
	return srv
}

// --- auth login happy path ----------------------------------------

func TestAuthLogin_HappyPath_WritesProfileAndSetsDefault(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	_ = withAPIServer(t, "alice@example.com", "org_42")
	withLoginRunner(t, fakeLoginRunner("molt_alice_key", nil))

	code, stdout, _ := runCLI(t, "--config", cfgPath, "auth", "login")
	if code != 0 {
		t.Fatalf("exit = %d; stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "alice@example.com") {
		t.Errorf("stdout missing email: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "org_42") {
		t.Errorf("stdout missing org: %q", stdout.String())
	}

	// Inspect the config file.
	cfg, err := config.LoadFrom(cfgPath)
	if err != nil || cfg == nil {
		t.Fatalf("LoadFrom: cfg=%v err=%v", cfg, err)
	}
	p := cfg.Profiles["default"]
	if p.APIKey != "molt_alice_key" {
		t.Errorf("default profile key = %q", p.APIKey)
	}
	// Email + OrgID must be persisted from /v1/me alongside the key so
	// `profile list` can surface them without re-authing every profile.
	if p.Email != "alice@example.com" {
		t.Errorf("default profile email = %q; want alice@example.com", p.Email)
	}
	if p.OrgID != "org_42" {
		t.Errorf("default profile org_id = %q; want org_42", p.OrgID)
	}
	if cfg.DefaultProfile != "default" {
		t.Errorf("DefaultProfile = %q; want 'default' on first login", cfg.DefaultProfile)
	}
}

// TestAuthLogin_MeFailure_StillSavesKeyWithoutEmailOrOrg pins the
// fallback: when /v1/me errors (network blip, transient 5xx), the API
// key still saves so the CLI is usable; the profile just lands without
// email/org_id and `profile list` renders them as "—" until next auth.
func TestAuthLogin_MeFailure_StillSavesKeyWithoutEmailOrOrg(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	// Stub server returns 500 on /v1/me so fetchMeBestEffort fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv(envAPIBase, srv.URL)
	withLoginRunner(t, fakeLoginRunner("molt_keyonly", nil))

	code, _, _ := runCLI(t, "--config", cfgPath, "auth", "login")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	cfg, err := config.LoadFrom(cfgPath)
	if err != nil || cfg == nil {
		t.Fatalf("LoadFrom: cfg=%v err=%v", cfg, err)
	}
	p := cfg.Profiles["default"]
	if p.APIKey != "molt_keyonly" {
		t.Errorf("key not saved: %q", p.APIKey)
	}
	if p.Email != "" || p.OrgID != "" {
		t.Errorf("email/org_id leaked on /v1/me failure: %q / %q", p.Email, p.OrgID)
	}
}

// --- auth login adds work profile without changing default --------

func TestAuthLogin_AddsSecondProfileWithoutChangingDefault(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	// Seed the config with a "personal" default already.
	seed := &config.Config{
		DefaultProfile: "personal",
		Profiles: map[string]config.Profile{
			"personal": {APIKey: "molt_personal", Created: time.Now().UTC()},
		},
	}
	if err := config.SaveTo(cfgPath, seed); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	_ = withAPIServer(t, "bob@example.com", "org_7")
	withLoginRunner(t, fakeLoginRunner("molt_bob_work", nil))

	code, _, _ := runCLI(t, "--config", cfgPath, "--profile", "work", "auth", "login")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := cfg.Profiles["work"].APIKey; got != "molt_bob_work" {
		t.Errorf("work profile key = %q", got)
	}
	if cfg.DefaultProfile != "personal" {
		t.Errorf("DefaultProfile = %q; second login must not flip the default", cfg.DefaultProfile)
	}
	if _, ok := cfg.Profiles["personal"]; !ok {
		t.Error("personal profile was removed")
	}
}

// --- auth login timeout from handoff runner -----------------------

func TestAuthLogin_PollTimeoutReportsCleanError(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	_ = withAPIServer(t, "", "")

	// Runner returns the same typed error the handoff package would.
	withLoginRunner(t, func(ctx context.Context, _ string, _ bool, _ string, _ *os.File) (*handoff.LoginResult, error) {
		return nil, &timeoutFixtureErr{}
	})

	code, _, stderr := runCLI(t, "--config", cfgPath, "auth", "login")
	if code == 0 {
		t.Fatal("exit = 0; want non-zero on timeout")
	}
	if !strings.Contains(stderr.String(), "timed out") && !strings.Contains(stderr.String(), "Login timed out") {
		t.Errorf("stderr missing timeout message: %q", stderr.String())
	}
}

// --- auth login poll 410 → expired error --------------------------

func TestAuthLogin_PollExpiredReportsCleanError(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	_ = withAPIServer(t, "", "")

	withLoginRunner(t, func(ctx context.Context, _ string, _ bool, _ string, _ *os.File) (*handoff.LoginResult, error) {
		return nil, &expiredFixtureErr{}
	})

	code, _, stderr := runCLI(t, "--config", cfgPath, "auth", "login")
	if code == 0 {
		t.Fatal("exit = 0; want non-zero on expired")
	}
	if !strings.Contains(stderr.String(), "expired") {
		t.Errorf("stderr missing 'expired': %q", stderr.String())
	}
}

// --- auth logout --------------------------------------------------

func TestAuthLogout_RemovesProfileAndWarns(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seed := &config.Config{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work":     {APIKey: "molt_work_key", Created: time.Now().UTC()},
			"personal": {APIKey: "molt_personal_key", Created: time.Now().UTC()},
		},
	}
	if err := config.SaveTo(cfgPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	code, _, stderr := runCLI(t, "--config", cfgPath, "--profile", "work", "auth", "logout")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "still active") {
		t.Errorf("logout missing security warning: %q", stderr.String())
	}

	cfg, _ := config.LoadFrom(cfgPath)
	if _, ok := cfg.Profiles["work"]; ok {
		t.Error("work profile not removed")
	}
	if cfg.DefaultProfile == "work" {
		t.Errorf("DefaultProfile still 'work'; want unset since we removed the default")
	}
}

func TestAuthLogout_NoProfileNoOp(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	code, _, stderr := runCLI(t, "--config", cfgPath, "auth", "logout")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "nothing to remove") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// --- auth check ---------------------------------------------------

func TestAuthCheck_NoAuthExits2(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	// Unset all MOLTABLE_* envs so resolver definitely gets nothing.
	t.Setenv("MOLTABLE_API_KEY", "")
	t.Setenv("MOLTABLE_PROFILE", "")

	code, _, stderr := runCLI(t, "--config", cfgPath, "auth", "check")
	if code != 2 {
		t.Fatalf("exit = %d; want 2 for no-auth", code)
	}
	if !strings.Contains(stderr.String(), "credentials") {
		t.Errorf("stderr missing no-auth message: %q", stderr.String())
	}
}

func TestAuthCheck_JSONEmitsExpectedFields(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seed := &config.Config{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work": {APIKey: "molt_work_123456789", Created: time.Now().UTC()},
		},
	}
	if err := config.SaveTo(cfgPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = withAPIServer(t, "carol@example.com", "org_X")
	t.Setenv("MOLTABLE_API_KEY", "")
	t.Setenv("MOLTABLE_PROFILE", "")

	code, stdout, _ := runCLI(t, "--config", cfgPath, "auth", "check", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal JSON: %v; body=%q", err, stdout.String())
	}
	for _, k := range []string{"profile", "user_id", "email", "org_id", "key_prefix", "source"} {
		if _, ok := out[k]; !ok {
			t.Errorf("missing field %q in JSON: %q", k, stdout.String())
		}
	}
	if out["email"] != "carol@example.com" {
		t.Errorf("email = %v", out["email"])
	}
	if out["user_id"] != "user_x" {
		t.Errorf("user_id = %v", out["user_id"])
	}
	if out["org_id"] != "org_X" {
		t.Errorf("org_id = %v", out["org_id"])
	}
	if out["profile"] != "work" {
		t.Errorf("profile = %v", out["profile"])
	}
	if s, _ := out["key_prefix"].(string); !strings.HasPrefix(s, "molt_") {
		t.Errorf("key_prefix = %v", out["key_prefix"])
	}
}

func TestAuthCheck_ProfileFlagOverridesEnv(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seed := &config.Config{
		DefaultProfile: "personal",
		Profiles: map[string]config.Profile{
			"work":     {APIKey: "molt_work_key", Created: time.Now().UTC()},
			"personal": {APIKey: "molt_personal_key", Created: time.Now().UTC()},
		},
	}
	if err := config.SaveTo(cfgPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = withAPIServer(t, "dan@example.com", "org_Z")
	t.Setenv("MOLTABLE_API_KEY", "")
	t.Setenv("MOLTABLE_PROFILE", "personal")

	// Global --profile work should override MOLTABLE_PROFILE=personal.
	code, stdout, _ := runCLI(t, "--config", cfgPath, "--profile", "work", "auth", "check", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["profile"] != "work" {
		t.Errorf("profile = %v; want 'work' (flag should override env)", out["profile"])
	}
}

// withAPIServerReturning401 stands up an httptest server that returns 401
// for /v1/me — used to verify `auth check` fails loudly on a revoked key
// instead of silently reporting success with empty fields.
func withAPIServerReturning401(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"invalid api key"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Setenv(envAPIBase, srv.URL)
	t.Cleanup(srv.Close)
	return srv
}

// TestAuthCheck_FailsLoudOn401 — historically `auth check` swallowed 401
// from /v1/me and returned exit 0 with empty fields, so scripts doing
// `moltable auth check && deploy` would silently proceed under a revoked
// key. Lock in the post-fix behavior: any 401 surfaces as exit-2 AuthError.
func TestAuthCheck_FailsLoudOn401(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seed := &config.Config{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work": {APIKey: "molt_revoked_key", Created: time.Now().UTC()},
		},
	}
	if err := config.SaveTo(cfgPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = withAPIServerReturning401(t)
	t.Setenv("MOLTABLE_API_KEY", "")
	t.Setenv("MOLTABLE_PROFILE", "")

	code, stdout, stderr := runCLI(t, "--config", cfgPath, "auth", "check")
	if code != 2 {
		t.Fatalf("exit = %d; want 2 (AuthError); stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "Authentication failed") {
		t.Errorf("stderr should contain AuthError message; got: %q", stderr.String())
	}
}

// --- resolveAPIBase -----------------------------------------------

func TestResolveAPIBase_DefaultsToProd(t *testing.T) {
	t.Setenv(envAPIBase, "")
	if got := resolveAPIBase(false); got != defaultAPIBase {
		t.Fatalf("resolveAPIBase(false) = %q; want %q", got, defaultAPIBase)
	}
}

func TestResolveAPIBase_DevFlagPicksLocalhost(t *testing.T) {
	t.Setenv(envAPIBase, "")
	if got := resolveAPIBase(true); got != devAPIBase {
		t.Fatalf("resolveAPIBase(true) = %q; want %q (dev fallback)", got, devAPIBase)
	}
}

func TestResolveAPIBase_EnvVarBeatsDevFlag(t *testing.T) {
	t.Setenv(envAPIBase, "https://staging.example.com")
	if got := resolveAPIBase(true); got != "https://staging.example.com" {
		t.Fatalf("resolveAPIBase(true) = %q; want env-var override to win over --dev", got)
	}
}

// --- error fixtures ------------------------------------------------

// timeoutFixtureErr mimics *molterrors.LoginTimeoutError without
// importing it (kept lightweight here — we assert on the rendered
// stderr, not on type identity, since the renderer in main.go is what
// the user sees).
type timeoutFixtureErr struct{}

func (*timeoutFixtureErr) Error() string       { return "Login timed out before approval." }
func (*timeoutFixtureErr) UserMessage() string { return "Login timed out before approval." }
func (*timeoutFixtureErr) Hint() string        { return "Run `moltable auth login` again." }

type expiredFixtureErr struct{}

func (*expiredFixtureErr) Error() string       { return "This login attempt expired." }
func (*expiredFixtureErr) UserMessage() string { return "This login attempt expired." }
func (*expiredFixtureErr) Hint() string        { return "Run `moltable auth login` again." }
