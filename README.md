# moltable CLI

Drive the moltable API from the shell. A single static Go binary
designed to be the most ergonomic surface a coding agent (Claude
Code, Cursor, Codex) can call when building enrichment tables.

```text
moltable <noun> <verb> [flags]
```

- ~22 commands, all `<noun> <verb>` shaped
- Browser-handoff `auth login` — no copy-pasting `molt_` keys
- Multi-profile config at `~/.config/moltable/config.toml`
- `--json` output + `--jq` filtering on every read command
- Bundled Claude Code skills installed automatically into
  `~/.claude/plugins/moltable/`
- Background self-update + API version handshake via
  `X-Moltable-Version`

---

## Install

### macOS / Linux

```sh
curl -fsSL https://get.moltable.io | sh
```

The installer detects your OS + arch, pulls the latest signed release
from GitHub, verifies the sha256 against the published
`checksums.txt`, and drops the binary into `/usr/local/bin` (or
`~/.local/bin` if `/usr/local/bin` is not writable — it warns about
PATH in that case). After the binary is in place it runs
`moltable skills install` so the bundled Claude Code skills are
written to `~/.claude/plugins/moltable/skills/`.

Useful environment overrides:

| Variable                | Effect                                                  |
| ----------------------- | ------------------------------------------------------- |
| `MOLTABLE_INSTALL_DIR`  | Force the install directory (no PATH fallback logic).   |
| `MOLTABLE_VERSION`      | Pin a specific release (e.g. `MOLTABLE_VERSION=0.1.0`). |

### Windows

The installer doesn't cover Windows natively (WSL users follow the
macOS/Linux flow). Native Windows users grab a build from the
[releases page](https://github.com/moltable/cli/releases) and
drop `moltable.exe` somewhere on `%PATH%`.

---

## Quick start

A 5-command tour, end to end:

```sh
# 1. Authenticate (opens a browser for the molt_ key handoff).
moltable auth login

# 2. Create a workbook and a table inside it.
moltable workbook create --name "Chicago restaurants" --json | jq -r .id
moltable table create --workbook wkb_xxx --name "Leads"

# 3. Import seed rows.
moltable row import --table tbl_xxx --file ./seed.csv

# 4. Add enrichment columns.
moltable column add --table tbl_xxx --type lookup --name "Domain" --prompt "Find the official domain."

# 5. Run + watch.
moltable run table --table tbl_xxx
moltable run watch --table tbl_xxx
```

Every command supports `--help`. Read commands accept `--json` and
`--jq '<expr>'` for piping into other tools.

---

## Auth + profiles

`moltable` uses **org-scoped API keys** (prefix `molt_`). One key per
`(user, org)` pair, stored per-profile in
`~/.config/moltable/config.toml`.

```sh
moltable auth login                # browser handoff for the default profile
moltable auth login --profile work # add a second workspace
moltable profile list              # show every profile + which is active
moltable profile use work          # switch the default
moltable auth check                # confirm which profile is active right now
```

Full triage flow lives in the bundled skill at
[`skills/auth-and-profiles.md`](./skills/auth-and-profiles.md) —
Claude Code loads this automatically once `moltable skills install`
has run.

---

## Skills (Claude Code bundle)

Every release ships with a small bundle of markdown skills that teach
Claude Code (and any other harness that consumes the same plugin
format) how to drive the CLI. They're embedded into the binary via
`go:embed` so the binary and the skills are always in lockstep —
there's no separate "skills version".

```sh
moltable skills install         # writes the bundle into ~/.claude/plugins/moltable/
moltable skills install --force # overwrite an existing install
```

Bundled skills today:

- `auth-and-profiles.md` — login flow, multi-profile setup, triage
- `build-enrichment-table.md` — workbook → table → columns → rows
- `run-and-watch-jobs.md` — `run table`, `run cell`, `run watch`, `stop`
- `long-tail-fallback.md` — what to do when the right verb doesn't
  exist (drop down to `moltable api`)

---

## Development

Build, test, and install from the repo root:

```sh
make build           # ./moltable
make test            # go test ./...
make test-race       # race detector
make vet             # go vet (also: make lint)
make install-local   # go install ./cmd/moltable
make release-dry-run # goreleaser release --snapshot --clean
```

Layout:

```text
./
├── cmd/moltable/       # Kong command tree (one file per noun)
├── internal/
│   ├── auth/           # tri-layer credential resolver (flag → env → config)
│   ├── config/         # TOML loader, XDG path resolution
│   ├── handoff/        # browser-handoff client (init / poll)
│   ├── httpc/          # HTTP client with retries + X-Moltable-Version handshake
│   ├── output/         # JSON / jq / TTY-aware formatting
│   ├── ui/             # lipgloss styles, progress bar, spinner, gh-style help
│   ├── updater/        # self-update (sha256 + atomic swap)
│   ├── version/        # build-time version + commit constants
│   └── errors/         # typed CLI errors + hints
├── skills/             # go:embed'd Claude Code skills (one md per skill)
├── scripts/            # release smoke tests
├── go.mod
├── Makefile
├── .goreleaser.yaml    # cross-platform release builds
└── README.md
```

The Go module path is
[`github.com/moltable/cli`](./go.mod). The CLI lives in its own
public repo (separate from the moltable server) so users can audit
`curl | sh` and the binary download path stays publicly readable.

---

## Releases

Releases are tag-triggered and run via GitHub Actions
([`.github/workflows/release.yml`](./.github/workflows/release.yml))
which invokes [goreleaser](https://goreleaser.com) against the root
[`.goreleaser.yaml`](./.goreleaser.yaml).

**Cutting a release:**

```sh
# 1. Make sure the tree is clean and internal/version/version.go's
#    BinaryVersion constant is bumped.
# 2. Tag and push:
git tag cli-v0.1.0
git push origin cli-v0.1.0
```

The tag must use the **`cli-v*` prefix** — anything else is ignored
by the release workflow. The workflow produces:

- `moltable_<version>_linux_amd64.tar.gz`
- `moltable_<version>_linux_arm64.tar.gz`
- `moltable_<version>_darwin_amd64.tar.gz`
- `moltable_<version>_darwin_arm64.tar.gz`
- `moltable_<version>_windows_amd64.zip`
- `checksums.txt` (sha256 per asset)

Dry-run the build locally without pushing a tag:

```sh
make release-dry-run
# Output appears in ./dist/
```

This invokes `goreleaser release --snapshot --clean`, so `goreleaser`
v2 must be on your `PATH` (`brew install goreleaser` or download
from the goreleaser releases page).

After a successful release, run the smoke test against the published
artifacts:

```sh
./scripts/test-install.sh
```

This boots a clean `ubuntu:latest` container, installs `curl` +
`tar`, runs the on-disk `install.sh`, and confirms `moltable version`
exits zero. Requires Docker on the host.

---

## Links

- Issues: <https://github.com/moltable/cli/issues>
- Releases: <https://github.com/moltable/cli/releases>
- API docs (server-side OpenAPI): published with your moltable
  deployment
