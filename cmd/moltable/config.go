// `moltable config show|get|set` — read and tweak the TOML config
// file the auth resolver consults.
//
// Why three verbs:
//
//   - `show`     dumps the full config (TOML or --json), with API keys
//                truncated to a `molt_xxxxxxxx` prefix so the output is
//                safe to paste into bug reports.
//   - `get <k>`  prints ONE value, suitable for shell substitution. The
//                long-tail fallback skill relies on `moltable config get
//                api-key` printing the resolved API key in plaintext —
//                this is intentional; the skill needs the secret to make
//                a downstream call.
//   - `set <k> <v>` writes a value to the config and saves. Today the
//                only supported key is `default-profile`, which is a
//                shorthand for `moltable profile use <name>`. We could
//                extend this later but explicit shape today beats
//                surprise behavior.
//
// Keys (intentionally NOT the raw TOML field names — these are the
// user-facing knobs):
//
//   - api-key          → resolved active key (via auth.Resolve)
//   - profile          → name of the profile that resolved
//   - default-profile  → config.DefaultProfile
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/moltable/cli/internal/auth"
	"github.com/moltable/cli/internal/config"
	clierrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/output"
)

// runConfigShow implements `moltable config show`. The ConfigShowCmd
// struct in main.go delegates here so the wiring + the body stay in
// separate files.
func runConfigShow(kctx *kong.Context, root *CLI, jsonOut bool, jqExpr string) error {
	cfg, err := loadConfig(root.Config)
	if err != nil {
		return err
	}
	if cfg == nil {
		// No file on disk yet → fresh install. Emit a friendly
		// "nothing configured" line so the user knows where to look,
		// rather than `{}` which reads as "broken".
		if jsonOut {
			return output.Print(kctx.Stdout, map[string]any{
				"default_profile": "",
				"profiles":        map[string]any{},
			}, jqExpr)
		}
		path, _ := config.Path()
		fmt.Fprintf(kctx.Stdout, "no profiles configured (config: %s)\n", path)
		return nil
	}

	// Build a sanitized view: API keys are truncated to the first 8
	// chars (the `molt_xxx` prefix is informational only — the body
	// after is the actual secret).
	type sanitizedProfile struct {
		APIKey  string `json:"api_key"`
		Created string `json:"created,omitempty"`
	}
	type sanitizedConfig struct {
		DefaultProfile string                      `json:"default_profile"`
		Profiles       map[string]sanitizedProfile `json:"profiles"`
	}
	out := sanitizedConfig{
		DefaultProfile: cfg.DefaultProfile,
		Profiles:       map[string]sanitizedProfile{},
	}
	for name, p := range cfg.Profiles {
		out.Profiles[name] = sanitizedProfile{
			APIKey:  truncateAPIKey(p.APIKey),
			Created: p.Created.UTC().Format("2006-01-02T15:04:05Z"),
		}
	}

	if jsonOut {
		return output.Print(kctx.Stdout, out, jqExpr)
	}

	// Human form: stable order so output is grep-friendly.
	fmt.Fprintf(kctx.Stdout, "default_profile = %q\n", out.DefaultProfile)
	names := make([]string, 0, len(out.Profiles))
	for n := range out.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := out.Profiles[n]
		fmt.Fprintf(kctx.Stdout, "\n[profiles.%s]\napi_key = %q\ncreated = %s\n", n, p.APIKey, p.Created)
	}
	return nil
}

// runConfigGet implements `moltable config get <key>`. The api-key
// special case returns the FULL plaintext key — agents need it for
// downstream HTTP calls.
func runConfigGet(kctx *kong.Context, root *CLI, key string) error {
	cfg, err := loadConfig(root.Config)
	if err != nil {
		return err
	}
	switch key {
	case "api-key":
		in := auth.FromEnvironment(root.APIKey, cfg)
		if root.Profile != "" && in.FlagAPIKey == "" && in.EnvAPIKey == "" {
			in.EnvProfile = root.Profile
		}
		resolved, _, rerr := auth.Resolve(in)
		if rerr != nil {
			return rerr
		}
		fmt.Fprintln(kctx.Stdout, resolved)
		return nil
	case "profile":
		// Best-effort: which profile would resolve TODAY.
		if cfg == nil {
			return &clierrors.InvalidInputError{
				Field:  "config get",
				Detail: "no config file on disk; run `moltable auth login` first",
			}
		}
		in := auth.FromEnvironment(root.APIKey, cfg)
		if root.Profile != "" && in.FlagAPIKey == "" && in.EnvAPIKey == "" {
			in.EnvProfile = root.Profile
		}
		_, src, rerr := auth.Resolve(in)
		if rerr != nil {
			return rerr
		}
		// Resolve only reports a Source string; for "profile" we want
		// the profile name. Pull it from the inputs directly.
		switch src {
		case auth.SourceEnvProfile:
			fmt.Fprintln(kctx.Stdout, in.EnvProfile)
		case auth.SourceConfig:
			fmt.Fprintln(kctx.Stdout, cfg.DefaultProfile)
		default:
			fmt.Fprintln(kctx.Stdout, "")
		}
		return nil
	case "default-profile":
		if cfg == nil {
			fmt.Fprintln(kctx.Stdout, "")
			return nil
		}
		fmt.Fprintln(kctx.Stdout, cfg.DefaultProfile)
		return nil
	default:
		return &clierrors.InvalidInputError{
			Field:  "config get",
			Detail: fmt.Sprintf("unknown key %q; valid keys: api-key, profile, default-profile", key),
		}
	}
}

// runConfigSet implements `moltable config set <key> <value>`. Today
// only `default-profile` is writeable; any other key returns an
// InvalidInputError directing the user at the right command (e.g.
// `moltable auth login` for adding an api key).
func runConfigSet(kctx *kong.Context, root *CLI, key, value string) error {
	cfg, err := loadConfig(root.Config)
	if err != nil {
		return err
	}
	if cfg == nil {
		// `set` on an empty config: create one so the operation does
		// what the user meant (not a confusing "no profiles" error).
		cfg = &config.Config{}
	}
	switch key {
	case "default-profile":
		if _, ok := cfg.Profiles[value]; !ok {
			return &clierrors.NotFoundError{Kind: "profile", ID: value}
		}
		cfg.DefaultProfile = value
		path, err := resolveConfigPath(root.Config)
		if err != nil {
			return err
		}
		if err := config.SaveTo(path, cfg); err != nil {
			return err
		}
		fmt.Fprintf(kctx.Stdout, "default_profile = %q\n", value)
		return nil
	default:
		return &clierrors.InvalidInputError{
			Field:  "config set",
			Detail: fmt.Sprintf("unsupported key %q; today only `default-profile` is writeable. Use `moltable auth login` to add API keys.", key),
		}
	}
}

// truncateAPIKey returns a redacted preview safe for stdout/log
// surfaces. Anything <= 8 chars is replaced wholesale so we never
// leak the actual key body even for unusually short keys.
func truncateAPIKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "molt_***"
	}
	return k[:8] + "xxxxxxxx"
}

// resolveConfigPath picks the override-or-default path for SaveTo.
func resolveConfigPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return config.Path()
}

// stdinIsTTY is a stand-in for future interactive prompts. Kept here
// because runConfigSet may want to prompt for confirmation on
// destructive ops in the future. The helper survives empty `os` import.
func stdinIsTTY() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// keysList is used by completion + did-you-mean. Single source of
// truth for "what keys does config understand".
var keysList = []string{"api-key", "profile", "default-profile"}

// suggestKey returns a typo suggestion using the shared DidYouMean
// helper from internal/errors.
func suggestKey(typed string) string {
	return clierrors.DidYouMean(strings.ToLower(typed), keysList)
}

// Compile-time check: keep the helpers referenced so future refactors
// don't drop them by mistake. Cheap and avoids "unused" linter noise.
var _ = stdinIsTTY
var _ = suggestKey
