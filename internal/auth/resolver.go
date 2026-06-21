// Package auth resolves the active moltable API key from the
// tri-layer (well, four-layer) precedence chain.
//
// Order of precedence (highest wins):
//
//  1. `--api-key` flag (explicit, request-scoped)
//  2. `MOLTABLE_API_KEY` environment variable (process-scoped)
//  3. `MOLTABLE_PROFILE=<name>` env, resolved against config.toml
//  4. `default_profile` from config.toml
//
// The resolved key is always validated for the `molt_` prefix BEFORE
// any network call, so a typo'd key fails fast with a useful error
// rather than a 401 round-trip.
//
// The shape mirrors the GitHub CLI's `pkg/auth` package: a single
// Resolve call returns the key, its source, and any typed error. The
// caller can surface the source in `moltable auth check` output (e.g.
// "Using key from --api-key flag" vs "Using key from profile 'work'").
package auth

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/moltable/cli/internal/config"
)

// Source describes which layer of the precedence chain produced the
// resolved API key.
type Source string

const (
	SourceFlag        Source = "flag"
	SourceEnvKey      Source = "env_key"
	SourceEnvProfile  Source = "env_profile"
	SourceConfig      Source = "config"
)

// String returns a human-readable label for the source, suitable for
// `moltable auth check` output.
func (s Source) String() string {
	switch s {
	case SourceFlag:
		return "--api-key flag"
	case SourceEnvKey:
		return "MOLTABLE_API_KEY env"
	case SourceEnvProfile:
		return "MOLTABLE_PROFILE env"
	case SourceConfig:
		return "config default_profile"
	default:
		return string(s)
	}
}

// EnvKey is the env var that supplies a raw API key directly.
const EnvKey = "MOLTABLE_API_KEY"

// EnvProfile is the env var that names which profile to use from the
// TOML config.
const EnvProfile = "MOLTABLE_PROFILE"

// KeyPrefix is the required prefix on every moltable API key. The
// resolver enforces this BEFORE returning, so callers can trust that
// the returned key at least syntactically looks like a moltable key.
const KeyPrefix = "molt_"

// ErrNoAuth is returned when no layer of the precedence chain yields
// a key. It satisfies the `Hinter` interface used by the CLI's
// error-rendering layer so the user sees a next-action hint, not just
// "no auth".
var ErrNoAuth = &noAuthError{}

type noAuthError struct{}

func (*noAuthError) Error() string {
	return "no moltable credentials configured"
}

// Hint implements the Hinter interface — the CLI's central error
// renderer prints this on a follow-up line so users always know the
// next move.
func (*noAuthError) Hint() string {
	return "Run `moltable auth login` to get started."
}

// InvalidKeyError is returned when a resolved key does not carry the
// `molt_` prefix. The wrapped Source tells the user where to look to
// fix the bad value.
type InvalidKeyError struct {
	Source Source
}

func (e *InvalidKeyError) Error() string {
	return fmt.Sprintf("auth: API key from %s is missing the %q prefix", e.Source, KeyPrefix)
}

// Hint returns a next-action suggestion tailored to where the bad
// key came from.
func (e *InvalidKeyError) Hint() string {
	switch e.Source {
	case SourceFlag:
		return "Check the value passed to --api-key. moltable keys begin with `molt_`."
	case SourceEnvKey:
		return "Check the value of MOLTABLE_API_KEY. moltable keys begin with `molt_`."
	case SourceEnvProfile:
		return "Inspect the profile named in MOLTABLE_PROFILE via `moltable profile list`."
	case SourceConfig:
		return "Run `moltable auth login` to refresh your default profile."
	default:
		return "Run `moltable auth login` to refresh credentials."
	}
}

// MissingProfileError is returned when MOLTABLE_PROFILE names a
// profile that isn't in the config, OR the default_profile points
// at a profile that has since been removed.
type MissingProfileError struct {
	Name   string
	Source Source
}

func (e *MissingProfileError) Error() string {
	return fmt.Sprintf("auth: profile %q (from %s) not found in config", e.Name, e.Source)
}

func (e *MissingProfileError) Hint() string {
	return fmt.Sprintf("Run `moltable profile list` to see available profiles, or `moltable auth login --profile %s` to create it.", e.Name)
}

// Inputs feeds Resolve the raw values it needs. Keeping env reads
// behind this struct makes tests trivial — they pass a closure-like
// snapshot instead of mutating process env.
type Inputs struct {
	// FlagAPIKey is the value of the `--api-key` flag, empty if unset.
	FlagAPIKey string
	// EnvAPIKey is the value of MOLTABLE_API_KEY at process start.
	EnvAPIKey string
	// EnvProfile is the value of MOLTABLE_PROFILE at process start.
	EnvProfile string
	// Config is the loaded TOML config, or nil if the file does not
	// exist. A nil Config combined with empty FlagAPIKey/EnvAPIKey/
	// EnvProfile triggers ErrNoAuth.
	Config *config.Config
}

// FromEnvironment populates Inputs from the live process env. Used by
// the real CLI; tests construct Inputs directly.
func FromEnvironment(flagAPIKey string, cfg *config.Config) Inputs {
	return Inputs{
		FlagAPIKey: flagAPIKey,
		EnvAPIKey:  os.Getenv(EnvKey),
		EnvProfile: os.Getenv(EnvProfile),
		Config:     cfg,
	}
}

// Resolve walks the precedence chain and returns the active key, the
// source that produced it, and any typed error.
//
// Errors:
//   - ErrNoAuth — no layer produced a key.
//   - *MissingProfileError — a profile was named but isn't in config.
//   - *InvalidKeyError — a key was found but lacks the `molt_` prefix.
func Resolve(in Inputs) (key string, src Source, err error) {
	// Layer 1: --api-key flag.
	if k := strings.TrimSpace(in.FlagAPIKey); k != "" {
		if !strings.HasPrefix(k, KeyPrefix) {
			return "", SourceFlag, &InvalidKeyError{Source: SourceFlag}
		}
		return k, SourceFlag, nil
	}

	// Layer 2: MOLTABLE_API_KEY env.
	if k := strings.TrimSpace(in.EnvAPIKey); k != "" {
		if !strings.HasPrefix(k, KeyPrefix) {
			return "", SourceEnvKey, &InvalidKeyError{Source: SourceEnvKey}
		}
		return k, SourceEnvKey, nil
	}

	// Layer 3: MOLTABLE_PROFILE env → profile from config.
	if name := strings.TrimSpace(in.EnvProfile); name != "" {
		if in.Config == nil {
			return "", SourceEnvProfile, &MissingProfileError{Name: name, Source: SourceEnvProfile}
		}
		p, ok := in.Config.Profiles[name]
		if !ok {
			return "", SourceEnvProfile, &MissingProfileError{Name: name, Source: SourceEnvProfile}
		}
		k := strings.TrimSpace(p.APIKey)
		if !strings.HasPrefix(k, KeyPrefix) {
			return "", SourceEnvProfile, &InvalidKeyError{Source: SourceEnvProfile}
		}
		return k, SourceEnvProfile, nil
	}

	// Layer 4: default_profile from config.
	if in.Config != nil && in.Config.DefaultProfile != "" {
		name := in.Config.DefaultProfile
		p, ok := in.Config.Profiles[name]
		if !ok {
			return "", SourceConfig, &MissingProfileError{Name: name, Source: SourceConfig}
		}
		k := strings.TrimSpace(p.APIKey)
		if !strings.HasPrefix(k, KeyPrefix) {
			return "", SourceConfig, &InvalidKeyError{Source: SourceConfig}
		}
		return k, SourceConfig, nil
	}

	return "", "", ErrNoAuth
}

// Hinter is satisfied by errors that can suggest a next action to the
// user. The CLI's central error printer detects this interface and
// renders the hint on a follow-up line.
type Hinter interface {
	Hint() string
}

// IsNoAuth reports whether err is (or wraps) ErrNoAuth.
func IsNoAuth(err error) bool {
	return errors.Is(err, ErrNoAuth)
}

// Is supports errors.Is for ErrNoAuth — there's only one ErrNoAuth
// value, so identity check is sufficient.
func (*noAuthError) Is(target error) bool {
	_, ok := target.(*noAuthError)
	return ok
}
