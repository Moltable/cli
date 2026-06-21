// `moltable skills` — install or remove the bundled Claude Code
// skills into the user's local plugins directory.
//
// Agents discover the CLI by reading the small bundle of skill
// markdown files we ship. The files themselves live in ./skills/
// and are baked into the binary via `go:embed` (see embed.go). This
// file implements the user-facing verbs that move them onto disk.
//
// Default install target: `~/.claude/plugins/moltable/skills/`. The
// `--target` flag overrides for tests and unusual setups. We refuse
// to write through a symlink at the target path to avoid silently
// rewriting whatever the symlink points at — the user can pass
// `--target` to an explicit real path if they really want that.
//
// Writes are atomic per-file: each skill is written to a `.tmp`
// sibling and renamed into place. On a mid-batch failure, all `.tmp`
// files we wrote in this run are cleaned up so the directory is
// either fully updated or left as it was before the run started
// (modulo files we successfully renamed earlier in the batch — see
// note on partial atomicity in installSkills below).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/alecthomas/kong"

	cli "github.com/moltable/cli"
)

// SkillsCmd is the `moltable skills` noun. It groups the two verbs
// the user can run today: install and uninstall.
type SkillsCmd struct {
	Install   SkillsInstallCmd   `cmd:"" help:"Install the moltable Claude Code skills to your local plugins directory."`
	Uninstall SkillsUninstallCmd `cmd:"" help:"Remove the moltable skills directory."`
}

// SkillsInstallCmd writes the embedded skills bundle to disk.
type SkillsInstallCmd struct {
	Target string `name:"target" help:"Override install target. Defaults to ~/.claude/plugins/moltable/skills/." type:"path"`
}

// Run installs the embedded skills to the resolved target directory.
//
// Reporting policy: when stderr is a TTY we print a one-line success
// summary so the human running the command sees something. When
// stderr is redirected (CI, agent harness), we stay silent on
// success — exit code 0 carries the signal — so the command composes
// cleanly with scripts.
func (c *SkillsInstallCmd) Run(kctx *kong.Context) error {
	target, err := resolveSkillsTarget(c.Target)
	if err != nil {
		return err
	}

	files := cli.Files()
	if len(files) == 0 {
		// Embed glob mis-wired at build time. This should never
		// happen in a release build but is worth catching loudly.
		return fmt.Errorf("skills install: no embedded skills found; this is a build bug")
	}

	if err := guardNotSymlink(target); err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("skills install: create %s: %w", target, err)
	}

	// Write the plugin manifest at <plugin-root>/.claude-plugin/plugin.json
	// before the skill files. Without this, Claude Code's plugin loader
	// either skips the directory or loads the skills unnamespaced — the
	// `/moltable:<skill>` invocation prefix depends on the manifest's
	// `name` field. The plugin root is the parent of the skills target;
	// `--target` overrides keep the same parent-of relationship.
	if err := installManifest(target); err != nil {
		return err
	}

	written, err := installSkills(target, files)
	if err != nil {
		return err
	}

	if isTTY(kctx.Stderr) {
		fmt.Fprintf(kctx.Stderr, "Installed %d skills to %s.\n", written, target)
	}
	return nil
}

// SkillsUninstallCmd removes the skills directory the install
// command wrote.
type SkillsUninstallCmd struct {
	Target string `name:"target" help:"Override uninstall target. Defaults to ~/.claude/plugins/moltable/skills/." type:"path"`
	Yes    bool   `name:"yes" short:"y" help:"Skip the confirmation prompt and delete the directory."`
}

// Run removes the resolved target directory.
//
// We require an explicit `--yes` because removing the directory is
// non-trivially destructive — a user who customized a skill in place
// would lose those edits otherwise. Without `--yes` we error with
// the exact command to re-run.
func (c *SkillsUninstallCmd) Run(kctx *kong.Context) error {
	target, err := resolveSkillsTarget(c.Target)
	if err != nil {
		return err
	}
	if !c.Yes {
		return fmt.Errorf("skills uninstall: refusing to delete %s without --yes (re-run with `moltable skills uninstall --yes`)", target)
	}
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		// Nothing to remove. Treat as success so re-running
		// uninstall is idempotent.
		if isTTY(kctx.Stderr) {
			fmt.Fprintf(kctx.Stderr, "No skills directory at %s; nothing to do.\n", target)
		}
		return nil
	}
	if err := guardNotSymlink(target); err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("skills uninstall: remove %s: %w", target, err)
	}
	// Best-effort: also remove the sibling .claude-plugin/ manifest
	// dir that installManifest wrote. Same parent-relationship rule
	// as install. Don't fail uninstall on this — pre-manifest installs
	// won't have it; user-customized layouts may not either.
	manifestDir := filepath.Join(filepath.Dir(target), ".claude-plugin")
	if info, err := os.Lstat(manifestDir); err == nil && info.Mode()&os.ModeSymlink == 0 {
		_ = os.RemoveAll(manifestDir)
	}
	if isTTY(kctx.Stderr) {
		fmt.Fprintf(kctx.Stderr, "Removed %s.\n", target)
	}
	return nil
}

// isTTY reports whether `w` is a *os.File backed by a character
// device (i.e. an interactive terminal). Pipes, files, and io.Writer
// implementations other than *os.File return false. Stdlib-only so
// we don't take an isatty dependency for one call site.
func isTTY(w interface{}) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// resolveSkillsTarget returns the absolute target directory the
// install/uninstall verbs should act on. An explicit override wins;
// otherwise we synthesize the default
// `~/.claude/plugins/moltable/skills/` path via os.UserHomeDir().
func resolveSkillsTarget(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("skills: cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "plugins", "moltable", "skills"), nil
}

// guardNotSymlink refuses to operate on a symlinked target directory.
// Writing through a symlink would silently mutate whatever the link
// points at; the user can opt into that by passing the real path
// directly via --target.
func guardNotSymlink(target string) error {
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("skills: stat %s: %w", target, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("skills: %s is a symbolic link; refusing to write through it. Pass --target to specify a real directory.", target)
	}
	return nil
}

// installManifest writes the embedded Claude Code plugin manifest
// to <parent-of-skillsTarget>/.claude-plugin/plugin.json so Claude
// Code's plugin loader registers the bundle under the `moltable:`
// invocation prefix. Same atomic write-rename discipline as
// installSkills; idempotent across re-runs.
func installManifest(skillsTarget string) error {
	pluginRoot := filepath.Dir(skillsTarget)
	manifestDir := filepath.Join(pluginRoot, ".claude-plugin")
	manifestPath := filepath.Join(manifestDir, "plugin.json")

	// Refuse to write through a symlinked manifest dir for the same
	// reason guardNotSymlink protects the skills target — don't
	// silently mutate whatever the link points at.
	if err := guardNotSymlink(manifestDir); err != nil {
		return err
	}
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		return fmt.Errorf("skills install: create %s: %w", manifestDir, err)
	}

	body := cli.Manifest()
	if len(body) == 0 {
		return fmt.Errorf("skills install: embedded plugin manifest is empty; this is a build bug")
	}

	tmpPath := manifestPath + ".tmp"
	if err := writeAtomic(tmpPath, body); err != nil {
		return fmt.Errorf("skills install: write manifest: %w", err)
	}
	if err := os.Rename(tmpPath, manifestPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("skills install: install manifest: %w", err)
	}
	return nil
}

// installSkills writes every file in `files` into `target` atomically.
//
// For each entry we write to `<target>/<name>.tmp`, then `os.Rename`
// into place. If any single file fails, we clean up the `.tmp` files
// we created during *this* run and return the error. Files already
// renamed earlier in the batch stay — full transactional rollback
// would require copying the prior content aside, which isn't worth
// the complexity for a four-file bundle that's stamped from the
// binary on every install.
func installSkills(target string, files map[string][]byte) (int, error) {
	// Sort for deterministic ordering so test output (and stderr
	// logs, if we add them later) are stable across runs.
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var tmpPaths []string
	cleanup := func() {
		for _, p := range tmpPaths {
			_ = os.Remove(p)
		}
	}

	written := 0
	for _, name := range names {
		body := files[name]
		finalPath := filepath.Join(target, name)
		tmpPath := finalPath + ".tmp"
		tmpPaths = append(tmpPaths, tmpPath)
		if err := writeAtomic(tmpPath, body); err != nil {
			cleanup()
			return 0, fmt.Errorf("skills install: write %s: %w", name, err)
		}
		if err := os.Rename(tmpPath, finalPath); err != nil {
			cleanup()
			return 0, fmt.Errorf("skills install: install %s: %w", name, err)
		}
		// Successfully renamed — drop from the cleanup list so we
		// don't try to remove the now-renamed file on a later
		// failure.
		tmpPaths = tmpPaths[:len(tmpPaths)-1]
		written++
	}
	return written, nil
}

// writeAtomic writes `data` to `path` via create-write-sync-close.
// The caller is responsible for the subsequent rename.
func writeAtomic(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}
