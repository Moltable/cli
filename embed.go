// Package cli holds package-level resources for the moltable CLI —
// today just the embedded Claude Code skills bundle that ships in the
// binary.
//
// The skills are authored as plain markdown under ./skills/ so they
// can be reviewed and edited like any other documentation. At build
// time `go:embed` snapshots them into the binary so the
// `moltable skills install` command (see ./cmd/moltable/skills.go)
// can write them out to the user's Claude Code plugins directory
// without any external assets.
//
// We deliberately expose the embedded bundle through Files() rather
// than the raw embed.FS so the install command doesn't accidentally
// pull in unrelated assets if more files land alongside the skills
// later (e.g. README.md inside skills/).
package cli

import (
	"embed"
	"io/fs"
	"strings"
)

// skillFiles holds the bundled skill markdown documents. The embed
// directive is intentionally restrictive (only `*.md` directly under
// `skills/`) so non-markdown helpers can live alongside without being
// shipped to users.
//
//go:embed skills/*.md
var skillFiles embed.FS

// pluginManifest is the Claude Code plugin descriptor that namespaces
// the bundled skills under the `moltable:` prefix
// (e.g. `/moltable:auth-and-profiles`). Without this file in the
// installed plugin directory, Claude Code's plugin loader either
// skips the directory or loads the skills unnamespaced.
//
//go:embed .claude-plugin/plugin.json
var pluginManifest []byte

// Files returns the embedded skill markdown files as a map of bare
// filename (e.g. "build-enrichment-table.md") -> raw markdown bytes.
// The returned map is a fresh copy on every call so callers are free
// to mutate it; the underlying embed.FS is read-only either way.
//
// On a successful build the map always contains the four skills the
// `moltable skills install` command writes to disk. If the embed
// directive is misconfigured (e.g. wrong glob) Files() returns an
// empty map — the install command treats that as a fatal bug and
// exits non-zero rather than silently writing nothing.
// Manifest returns the embedded Claude Code plugin manifest bytes.
// The skills install command writes these to
// <plugin-root>/.claude-plugin/plugin.json so Claude Code's loader
// can register the bundle under the `moltable:` prefix.
func Manifest() []byte {
	// Defensive copy so callers can't mutate the backing slice and
	// affect future installs in the same process. Cheap (~500 bytes).
	out := make([]byte, len(pluginManifest))
	copy(out, pluginManifest)
	return out
}

func Files() map[string][]byte {
	out := map[string][]byte{}
	entries, err := fs.ReadDir(skillFiles, "skills")
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := fs.ReadFile(skillFiles, "skills/"+name)
		if err != nil {
			// A read error inside an embed.FS only happens if the
			// glob matched something that isn't readable, which is a
			// build-time bug. Skip it; the caller will notice the
			// missing file via the count assertion in tests.
			continue
		}
		out[name] = data
	}
	return out
}
