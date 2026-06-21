package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moltable/cli/internal/config"
)

// fixtureConfig returns a config with two profiles: "work" (default)
// and "personal". Used by most precedence tests.
func fixtureConfig() *config.Config {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, time.UTC)
	return &config.Config{
		DefaultProfile: "work",
		Profiles: map[string]config.Profile{
			"work":     {APIKey: "molt_work_key", Created: now},
			"personal": {APIKey: "molt_personal_key", Created: now},
		},
	}
}

func TestResolve_PrecedenceTable(t *testing.T) {
	cfg := fixtureConfig()

	tests := []struct {
		name    string
		in      Inputs
		wantKey string
		wantSrc Source
	}{
		{
			name: "flag beats env_key + env_profile + config",
			in: Inputs{
				FlagAPIKey: "molt_flag_xxx",
				EnvAPIKey:  "molt_env_yyy",
				EnvProfile: "personal",
				Config:     cfg,
			},
			wantKey: "molt_flag_xxx",
			wantSrc: SourceFlag,
		},
		{
			name: "env_key beats env_profile + config",
			in: Inputs{
				FlagAPIKey: "",
				EnvAPIKey:  "molt_env_yyy",
				EnvProfile: "personal",
				Config:     cfg,
			},
			wantKey: "molt_env_yyy",
			wantSrc: SourceEnvKey,
		},
		{
			name: "env_profile beats default_profile in config",
			in: Inputs{
				EnvProfile: "personal",
				Config:     cfg,
			},
			wantKey: "molt_personal_key",
			wantSrc: SourceEnvProfile,
		},
		{
			name: "default_profile from config when nothing else set",
			in: Inputs{
				Config: cfg,
			},
			wantKey: "molt_work_key",
			wantSrc: SourceConfig,
		},
		{
			name: "whitespace in flag is trimmed",
			in: Inputs{
				FlagAPIKey: "  molt_flag_xxx  ",
				Config:     cfg,
			},
			wantKey: "molt_flag_xxx",
			wantSrc: SourceFlag,
		},
		{
			name: "empty flag does not short-circuit to ErrNoAuth",
			in: Inputs{
				FlagAPIKey: "",
				EnvAPIKey:  "molt_env",
				Config:     cfg,
			},
			wantKey: "molt_env",
			wantSrc: SourceEnvKey,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotKey, gotSrc, err := Resolve(tc.in)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if gotKey != tc.wantKey {
				t.Errorf("key = %q, want %q", gotKey, tc.wantKey)
			}
			if gotSrc != tc.wantSrc {
				t.Errorf("source = %q, want %q", gotSrc, tc.wantSrc)
			}
		})
	}
}

func TestResolve_NoAuth_AnywhereReturnsErrNoAuth(t *testing.T) {
	_, _, err := Resolve(Inputs{})
	if err == nil {
		t.Fatal("Resolve with empty Inputs: want ErrNoAuth, got nil")
	}
	if !IsNoAuth(err) {
		t.Fatalf("Resolve: want ErrNoAuth, got %T (%v)", err, err)
	}
	if !errors.Is(err, ErrNoAuth) {
		t.Errorf("errors.Is(err, ErrNoAuth) = false, want true")
	}
}

func TestResolve_NoAuth_HintIsLoginInvitation(t *testing.T) {
	_, _, err := Resolve(Inputs{})
	if err == nil {
		t.Fatal("Resolve: want ErrNoAuth, got nil")
	}
	h, ok := err.(Hinter)
	if !ok {
		t.Fatalf("err does not satisfy Hinter: %T", err)
	}
	hint := h.Hint()
	if !strings.Contains(hint, "moltable auth login") {
		t.Errorf("hint = %q, want substring %q", hint, "moltable auth login")
	}
}

func TestResolve_NoAuth_ConfigWithNoDefaultProfile(t *testing.T) {
	// Config exists but has no default_profile and no env hints.
	cfg := &config.Config{Profiles: map[string]config.Profile{}}
	_, _, err := Resolve(Inputs{Config: cfg})
	if !IsNoAuth(err) {
		t.Errorf("want ErrNoAuth, got %T (%v)", err, err)
	}
}

func TestResolve_InvalidKeyPrefix_FlagRejectedBeforeNetwork(t *testing.T) {
	_, src, err := Resolve(Inputs{FlagAPIKey: "sk_not_a_moltable_key"})
	if err == nil {
		t.Fatal("Resolve: want InvalidKeyError, got nil")
	}
	var ie *InvalidKeyError
	if !errors.As(err, &ie) {
		t.Fatalf("err type = %T, want *InvalidKeyError", err)
	}
	if ie.Source != SourceFlag {
		t.Errorf("InvalidKeyError.Source = %q, want %q", ie.Source, SourceFlag)
	}
	if src != SourceFlag {
		t.Errorf("returned source = %q, want %q (so caller can attribute the bad value)", src, SourceFlag)
	}
}

func TestResolve_InvalidKeyPrefix_EnvKey(t *testing.T) {
	_, _, err := Resolve(Inputs{EnvAPIKey: "abc123"})
	var ie *InvalidKeyError
	if !errors.As(err, &ie) {
		t.Fatalf("want *InvalidKeyError, got %T (%v)", err, err)
	}
	if ie.Source != SourceEnvKey {
		t.Errorf("Source = %q, want %q", ie.Source, SourceEnvKey)
	}
}

func TestResolve_InvalidKeyPrefix_ProfileKey(t *testing.T) {
	cfg := &config.Config{
		DefaultProfile: "bad",
		Profiles: map[string]config.Profile{
			"bad": {APIKey: "garbage_key", Created: time.Now().UTC()},
		},
	}
	_, _, err := Resolve(Inputs{Config: cfg})
	var ie *InvalidKeyError
	if !errors.As(err, &ie) {
		t.Fatalf("want *InvalidKeyError, got %T (%v)", err, err)
	}
	if ie.Source != SourceConfig {
		t.Errorf("Source = %q, want %q", ie.Source, SourceConfig)
	}
}

func TestResolve_InvalidKeyPrefix_HintsAreContextAware(t *testing.T) {
	cases := []struct {
		name string
		in   Inputs
		want string
	}{
		{"flag", Inputs{FlagAPIKey: "abc"}, "--api-key"},
		{"env_key", Inputs{EnvAPIKey: "abc"}, "MOLTABLE_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Resolve(tc.in)
			h, ok := err.(Hinter)
			if !ok {
				t.Fatalf("err is not Hinter: %T", err)
			}
			if !strings.Contains(h.Hint(), tc.want) {
				t.Errorf("hint = %q, want substring %q", h.Hint(), tc.want)
			}
		})
	}
}

func TestResolve_MissingProfileViaEnvProfile(t *testing.T) {
	cfg := fixtureConfig()
	_, _, err := Resolve(Inputs{EnvProfile: "does-not-exist", Config: cfg})
	var mpe *MissingProfileError
	if !errors.As(err, &mpe) {
		t.Fatalf("want *MissingProfileError, got %T (%v)", err, err)
	}
	if mpe.Name != "does-not-exist" {
		t.Errorf("Name = %q, want %q", mpe.Name, "does-not-exist")
	}
	if mpe.Source != SourceEnvProfile {
		t.Errorf("Source = %q, want %q", mpe.Source, SourceEnvProfile)
	}
	// Hint should be actionable.
	if h, ok := err.(Hinter); ok {
		hint := h.Hint()
		if !strings.Contains(hint, "does-not-exist") {
			t.Errorf("hint = %q, want it to mention the missing profile name", hint)
		}
	}
}

func TestResolve_MissingProfileViaEnvProfile_NilConfig(t *testing.T) {
	_, _, err := Resolve(Inputs{EnvProfile: "x", Config: nil})
	var mpe *MissingProfileError
	if !errors.As(err, &mpe) {
		t.Fatalf("want *MissingProfileError, got %T (%v)", err, err)
	}
	if mpe.Source != SourceEnvProfile {
		t.Errorf("Source = %q, want %q", mpe.Source, SourceEnvProfile)
	}
}

func TestResolve_MissingProfileViaConfigDefault(t *testing.T) {
	// default_profile points at a name that isn't in Profiles.
	cfg := &config.Config{DefaultProfile: "ghost", Profiles: map[string]config.Profile{}}
	_, _, err := Resolve(Inputs{Config: cfg})
	var mpe *MissingProfileError
	if !errors.As(err, &mpe) {
		t.Fatalf("want *MissingProfileError, got %T (%v)", err, err)
	}
	if mpe.Source != SourceConfig {
		t.Errorf("Source = %q, want %q", mpe.Source, SourceConfig)
	}
}

func TestSource_StringIsHumanReadable(t *testing.T) {
	cases := map[Source]string{
		SourceFlag:       "--api-key flag",
		SourceEnvKey:     "MOLTABLE_API_KEY env",
		SourceEnvProfile: "MOLTABLE_PROFILE env",
		SourceConfig:     "config default_profile",
	}
	for src, want := range cases {
		if got := src.String(); got != want {
			t.Errorf("Source(%q).String() = %q, want %q", src, got, want)
		}
	}
}
