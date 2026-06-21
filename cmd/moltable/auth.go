// `moltable auth` — login / logout / check.
//
// Login drives the browser handoff dance (init + browser open + poll)
// and writes the resulting `molt_` key into the TOML profile config.
// Logout removes a profile locally; it does NOT call DELETE on the
// server-side API key — the user is told to revoke it in the web UI
// if their machine is compromised.
// Check fetches `/v1/me` using the resolved key and prints either a
// human-readable summary or a JSON object (--json).
//
// Verb structs live next to main.go and bodies live here so Kong's
// dispatch stays uniform. Each Run() borrows the root CLI pointer
// for the global flags (--api-key, --profile, --config).
//
// Tests live in auth_test.go and use the same in-process run() harness
// as skills_test.go — no subprocess spawning.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	"github.com/moltable/cli/internal/auth"
	"github.com/moltable/cli/internal/config"
	clierrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/handoff"
	"github.com/moltable/cli/internal/httpc"
)

// defaultAPIBase is the production moltable API root. Overridden by
// MOLTABLE_API_BASE for dev / smoke-testing, or by --dev for the
// localhost shortcut.
const defaultAPIBase = "https://api.moltable.io"

// devAPIBase is what --dev (or MOLTABLE_DEV=1) defaults to when no
// explicit MOLTABLE_API_BASE is set. Matches the port `apps/api`
// listens on when run via `pnpm exec turbo dev`.
const devAPIBase = "https://localhost:8080"

// envAPIBase names the env var that overrides defaultAPIBase. Lifted
// to a constant so tests can t.Setenv it without a string typo.
const envAPIBase = "MOLTABLE_API_BASE"

// resolveAPIBase returns the API base URL the handoff client + /v1/me
// call should target. Precedence:
//
//  1. MOLTABLE_API_BASE (always wins — lets devs target a non-default
//     local port or a staging environment).
//  2. devAPIBase when --dev / MOLTABLE_DEV is set.
//  3. defaultAPIBase (production).
func resolveAPIBase(dev bool) string {
	if v := strings.TrimSpace(os.Getenv(envAPIBase)); v != "" {
		return v
	}
	if dev {
		return devAPIBase
	}
	return defaultAPIBase
}

// --- auth login --------------------------------------------------

// authLoginRunner is the seam tests inject through to bypass the real
// handoff dance. Production wiring uses runHandoffLogin (below);
// auth_test.go swaps in a closure that returns canned LoginResults.
//
// label is the computed device fingerprint to send in the handoff
// init body — see computeClientLabel for the precedence rules. Empty
// string means "don't send" and the server falls back to a date-only
// key name.
var authLoginRunner func(ctx context.Context, apiBase string, dev bool, label string, stderr *os.File) (*handoff.LoginResult, error) = runHandoffLogin

// runHandoffLogin is the real production runner — wraps handoff.New +
// Login with the API base and stderr writer the cmd layer supplied.
// When dev is true the underlying HTTP transport skips TLS verify so
// the local API's self-signed devcerts don't break the handoff dance.
func runHandoffLogin(ctx context.Context, apiBase string, dev bool, label string, stderr *os.File) (*handoff.LoginResult, error) {
	// The HTTP client's APIKey is irrelevant for handoff calls (they're
	// unauthenticated) — we just need a non-empty value because httpc.New
	// rejects empty keys. Use a sentinel so any accidental Authorization
	// header on an init/poll request is conspicuously wrong in logs.
	hc, err := httpc.NewWithOptions(apiBase, "molt_handoff_unused", buildUserAgent(dev), httpc.Options{
		InsecureSkipTLSVerify: dev,
	})
	if err != nil {
		return nil, err
	}
	cl := handoff.New(hc, apiBase)
	cl.Stderr = stderr
	cl.ClientLabel = label
	return cl.Login(ctx)
}

// defaultLoginProfile is the profile name auth login writes to when
// the global `--profile` flag is unset.
const defaultLoginProfile = "default"

func (c *AuthLoginCmd) Run(kctx *kong.Context, root *CLI) error {
	apiBase := resolveAPIBase(root.Dev)
	profileName := strings.TrimSpace(root.Profile)
	if profileName == "" {
		profileName = defaultLoginProfile
	}

	stderr, _ := kctx.Stderr.(*os.File)
	if stderr == nil {
		// Tests sometimes route through a *bytes.Buffer; the handoff
		// client only needs an io.Writer-shaped value. os.Stderr is the
		// safe production default.
		stderr = os.Stderr
	}

	// Compute the device label sent to the server. Either the user's
	// explicit --label override or "<hostname> · <UTC date>"; honors
	// MOLTABLE_NO_HOSTNAME for the privacy opt-out path.
	label := computeClientLabel(c.Label, time.Now())

	res, err := authLoginRunner(context.Background(), apiBase, root.Dev, label, stderr)
	if err != nil {
		return err
	}

	// Persist the key to a profile, under the file lock the config
	// package already arranged for us.
	cfg, err := loadConfig(root.Config)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.AddProfile(profileName, config.Profile{
		APIKey:  res.APIKey,
		Created: time.Now().UTC(),
	})
	if err := saveConfig(root.Config, cfg); err != nil {
		return err
	}

	// Fetch /v1/me with the brand-new key so the success message can
	// confirm "Logged in as <email> (<org>)". Errors from /v1/me are
	// non-fatal: the key is saved, the user can still use the CLI.
	email, orgID := fetchMeBestEffort(apiBase, res.APIKey, root.Dev)

	who := email
	if who == "" {
		who = "<unknown>"
	}
	suffix := ""
	if orgID != "" {
		suffix = fmt.Sprintf(" (%s)", orgID)
	}
	fmt.Fprintf(kctx.Stdout, "Logged in as %s%s. Profile %q added.\n", who, suffix, profileName)
	return nil
}

// meResult is what fetchMe returns on success. Fields mirror the
// /v1/me JSON envelope so callers don't need a second struct.
type meResult struct {
	UserID string `json:"user_id"`
	OrgID  string `json:"org_id"`
	Email  string `json:"email"`
}

// fetchMe calls GET /v1/me with the resolved key. Returns the parsed
// identity payload AND a status code so callers can branch:
//
//   - HTTP 200          → res != nil, status=200, err=nil
//   - HTTP 401          → res=nil, status=401, err=*clierrors.AuthError{Reason:"revoked"}
//   - other HTTP / net  → res=nil, status=<code or 0>, err=transport/decode error
//
// `dev` flips the underlying transport to insecure TLS so /v1/me works
// against the local API's self-signed devcerts.
func fetchMe(apiBase, apiKey string, dev bool) (*meResult, int, error) {
	hc, err := httpc.NewWithOptions(apiBase, apiKey, buildUserAgent(dev), httpc.Options{
		InsecureSkipTLSVerify: dev,
	})
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := hc.Do(ctx, httpc.Request{Method: http.MethodGet, Path: "/v1/me"})
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, http.StatusUnauthorized, &clierrors.AuthError{Reason: "revoked"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("/v1/me: HTTP %d", resp.StatusCode)
	}
	var out meResult
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, resp.StatusCode, err
	}
	return &out, resp.StatusCode, nil
}

// fetchMeBestEffort wraps fetchMe for the `auth login` success message,
// where any /v1/me failure is non-fatal — the key is saved either way
// and the user can still use the CLI. Returns empty strings on any error.
func fetchMeBestEffort(apiBase, apiKey string, dev bool) (email, orgID string) {
	res, _, err := fetchMe(apiBase, apiKey, dev)
	if err != nil || res == nil {
		return "", ""
	}
	return res.Email, res.OrgID
}

// --- auth logout -------------------------------------------------

func (c *AuthLogoutCmd) Run(kctx *kong.Context, root *CLI) error {
	cfg, err := loadConfig(root.Config)
	if err != nil {
		return err
	}
	if cfg == nil || len(cfg.Profiles) == 0 {
		fmt.Fprintln(kctx.Stderr, "No profiles configured; nothing to remove.")
		return nil
	}

	// Resolve target. Global --profile wins; else the current default.
	target := strings.TrimSpace(root.Profile)
	if target == "" {
		target = cfg.DefaultProfile
	}
	if target == "" {
		fmt.Fprintln(kctx.Stderr, "No default profile set. Pass --profile <name>.")
		return nil
	}
	if _, ok := cfg.Profiles[target]; !ok {
		return &profileNotFoundError{Name: target}
	}

	cfg.RemoveProfile(target)
	if err := saveConfig(root.Config, cfg); err != nil {
		return err
	}

	// Always warn that local removal does NOT revoke the key.
	fmt.Fprintf(kctx.Stderr,
		"Profile %q removed locally. Your API key is still active. Revoke it at https://app.moltable.io/settings/api-keys if this machine is compromised.\n",
		target,
	)
	return nil
}

// --- auth check --------------------------------------------------

func (c *AuthCheckCmd) Run(kctx *kong.Context, root *CLI) error {
	cfg, err := loadConfig(root.Config)
	if err != nil {
		return err
	}
	in := auth.FromEnvironment(root.APIKey, cfg)
	if root.Profile != "" {
		if in.FlagAPIKey == "" && in.EnvAPIKey == "" {
			in.EnvProfile = root.Profile
		}
	}
	key, src, rerr := auth.Resolve(in)
	if rerr != nil {
		return rerr
	}

	// Try to identify which profile the key came from, when it came
	// from a profile-shaped source. This is best-effort — the source
	// already tells the user where to look.
	profileName := ""
	switch src {
	case auth.SourceEnvProfile:
		profileName = os.Getenv(auth.EnvProfile)
		if root.Profile != "" {
			profileName = root.Profile
		}
	case auth.SourceConfig:
		if cfg != nil {
			profileName = cfg.DefaultProfile
		}
	}

	apiBase := resolveAPIBase(root.Dev)
	// Unlike `auth login` (which is best-effort on /v1/me), `auth check`
	// is the explicit "is my key still valid?" probe — a 401 here must
	// fail loudly so scripts that gate on `auth check && ...` don't
	// silently proceed with a revoked key.
	res, _, err := fetchMe(apiBase, key, root.Dev)
	if err != nil {
		return err
	}

	if c.JSON {
		out := map[string]any{
			"profile":    profileName,
			"user_id":    res.UserID,
			"email":      res.Email,
			"org_id":     res.OrgID,
			"key_prefix": keyPrefix(key),
			"source":     string(src),
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return err
		}
		fmt.Fprintln(kctx.Stdout, string(raw))
		return nil
	}

	who := res.Email
	if who == "" {
		who = "<unknown>"
	}
	orgPart := ""
	if res.OrgID != "" {
		orgPart = fmt.Sprintf(" (%s)", res.OrgID)
	}
	profilePart := ""
	if profileName != "" {
		profilePart = fmt.Sprintf(" Profile %q.", profileName)
	}
	fmt.Fprintf(kctx.Stdout, "Logged in as %s%s.%s Source: %s. Key: %s\n",
		who, orgPart, profilePart, src, keyPrefix(key))
	return nil
}

// keyPrefix returns the prefix `molt_xxx…` form of an API key for
// non-secret display. Matches main.go's mask() but is centralized
// here so auth check and login share the exact same redaction logic.
func keyPrefix(key string) string {
	if len(key) <= 9 {
		return "molt_***"
	}
	return key[:8] + "…"
}

// --- shared helpers ----------------------------------------------

// saveConfig writes cfg to the override path if --config was passed,
// else to the XDG-resolved default. Honors the file lock in the config
// package.
func saveConfig(override string, cfg *config.Config) error {
	if override != "" {
		return config.SaveTo(override, cfg)
	}
	return config.Save(cfg)
}

// profileNotFoundError is the typed error returned when a name doesn't
// match any entry in the config. It satisfies the Hinter interface so
// the central error printer renders the hint on a follow-up line.
type profileNotFoundError struct {
	Name string
}

func (e *profileNotFoundError) Error() string { return e.UserMessage() }
func (e *profileNotFoundError) UserMessage() string {
	return fmt.Sprintf("Profile %q not found.", e.Name)
}
func (e *profileNotFoundError) Hint() string {
	return "Run `moltable profile list` to see available profiles."
}
