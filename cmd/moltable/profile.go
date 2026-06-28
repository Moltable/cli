// `moltable profile` — list / use / remove named credential profiles.
//
// All three verbs operate on the same TOML config file the login flow
// writes (see config package + auth.go). The list output exposes
// {name, default, created}; we deliberately omit api_key so a JSON
// dump can be pasted in support tickets without leaking the secret.
//
// `use` and `remove` write under the config package's file lock; a
// concurrent `auth login` therefore never races with profile editing.

package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/alecthomas/kong"

	"github.com/moltable/cli/internal/config"
)

// profileSummary is the JSON shape `profile list --json` emits per
// profile. Defined here (not in config package) so the wire contract
// of the CLI doesn't drift if config.Profile gains internal fields.
//
// Email + OrgID are populated by `auth login` from /v1/me; profiles
// authed before that flow existed render the fields empty (omitempty
// in JSON; "—" in the human table).
type profileSummary struct {
	Name    string    `json:"name"`
	Default bool      `json:"default"`
	Created time.Time `json:"created"`
	Email   string    `json:"email,omitempty"`
	OrgID   string    `json:"org_id,omitempty"`
}

func (c *ProfileListCmd) Run(kctx *kong.Context, root *CLI) error {
	cfg, err := loadConfig(root.Config)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &config.Config{}
	}

	names := cfg.ListProfiles()
	out := make([]profileSummary, 0, len(names))
	for _, name := range names {
		p := cfg.Profiles[name]
		out = append(out, profileSummary{
			Name:    name,
			Default: name == cfg.DefaultProfile,
			Created: p.Created,
			Email:   p.Email,
			OrgID:   p.OrgID,
		})
	}

	if c.JSON {
		raw, err := json.Marshal(out)
		if err != nil {
			return err
		}
		fmt.Fprintln(kctx.Stdout, string(raw))
		return nil
	}

	if len(out) == 0 {
		fmt.Fprintln(kctx.Stdout, "No profiles configured. Run `moltable auth login` to create one.")
		return nil
	}

	tw := tabwriter.NewWriter(kctx.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDEFAULT\tEMAIL\tORG\tCREATED")
	for _, p := range out {
		def := ""
		if p.Default {
			def = "*"
		}
		email := p.Email
		if email == "" {
			email = "—"
		}
		org := p.OrgID
		if org == "" {
			org = "—"
		}
		created := ""
		if !p.Created.IsZero() {
			created = p.Created.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Name, def, email, org, created)
	}
	return tw.Flush()
}

func (c *ProfileUseCmd) Run(kctx *kong.Context, root *CLI) error {
	cfg, err := loadConfig(root.Config)
	if err != nil {
		return err
	}
	if cfg == nil || len(cfg.Profiles) == 0 {
		return &profileNotFoundError{Name: c.Name}
	}
	if _, ok := cfg.Profiles[c.Name]; !ok {
		return &profileNotFoundError{Name: c.Name}
	}
	cfg.DefaultProfile = c.Name
	if err := saveConfig(root.Config, cfg); err != nil {
		return err
	}
	fmt.Fprintf(kctx.Stdout, "Default profile set to %q.\n", c.Name)
	return nil
}

// profileRemoveDefaultError is returned when the user tries to remove
// the current default while other profiles still exist. The hint
// pushes them to `profile use` to pick a new default first.
type profileRemoveDefaultError struct {
	Name       string
	Candidates []string
}

func (e *profileRemoveDefaultError) Error() string { return e.UserMessage() }
func (e *profileRemoveDefaultError) UserMessage() string {
	return fmt.Sprintf("Cannot remove default profile %q while other profiles exist.", e.Name)
}
func (e *profileRemoveDefaultError) Hint() string {
	if len(e.Candidates) > 0 {
		return fmt.Sprintf("Switch default first: `moltable profile use %s`.", e.Candidates[0])
	}
	return "Switch default first: `moltable profile use <other>`."
}

func (c *ProfileRemoveCmd) Run(kctx *kong.Context, root *CLI) error {
	cfg, err := loadConfig(root.Config)
	if err != nil {
		return err
	}
	if cfg == nil || len(cfg.Profiles) == 0 {
		return &profileNotFoundError{Name: c.Name}
	}
	if _, ok := cfg.Profiles[c.Name]; !ok {
		return &profileNotFoundError{Name: c.Name}
	}

	// Guard: removing the default while other profiles exist would
	// leave the config in a "no default" state silently. Force the
	// user to pick a new default first.
	if cfg.DefaultProfile == c.Name && len(cfg.Profiles) > 1 {
		others := make([]string, 0, len(cfg.Profiles)-1)
		for _, name := range cfg.ListProfiles() {
			if name != c.Name {
				others = append(others, name)
			}
		}
		return &profileRemoveDefaultError{Name: c.Name, Candidates: others}
	}

	cfg.RemoveProfile(c.Name)
	if err := saveConfig(root.Config, cfg); err != nil {
		return err
	}

	// Same warning shape as auth logout: local removal does NOT revoke
	// the key on the server.
	fmt.Fprintf(kctx.Stderr,
		"Profile %q removed locally. Your API key is still active. Revoke it at https://app.moltable.io/settings/api-keys if this machine is compromised.\n",
		c.Name,
	)
	return nil
}
