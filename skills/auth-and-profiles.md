---
name: auth-and-profiles
description: Set up and switch between moltable credentials — browser-handoff login, multi-profile management, and verifying which identity is active.
when_to_use: When `moltable auth check` errors with "no auth configured" (exit code 2), when the user wants to add a second workspace or organization (work + personal), when commands fail with 401, or when the user asks "which moltable account am I using" / "log me in" / "switch profiles".
---

# Auth and Profiles

moltable uses **org-scoped API keys** (prefix `molt_`). One key per `(user, org)` pair. Each profile in the TOML config holds exactly one key.

## Quick triage

Run `moltable auth check --json` first. The outcome tells you which path to take:

| `auth check` result | Meaning | Next action |
| --- | --- | --- |
| Exit 0, prints `{profile, user, org_id, ...}` | Already logged in. | If the user wants a *different* profile, see "Add another profile". Otherwise nothing to do. |
| Exit 2, "no auth configured" | No profile yet. | Run "First-time login" below. |
| Exit non-zero, "401 unauthorized" | Key was revoked or workspace deleted. | Run `moltable auth logout` then "First-time login". |

## First-time login

`moltable auth login` opens a browser handoff. The server returns a short URL with a one-time code; the user signs in via Clerk, approves the CLI, and the key flows back to the CLI which writes it to `~/.config/moltable/config.toml`.

```
moltable auth login
```

Expected output:

```
Open https://app.moltable.io/cli-auth?code=AB7-FX2-K9P in your browser.
(waiting...)
Logged in as alice@example.com (org_X). Profile "personal" added.
```

The profile name defaults to `personal` on the first login. The first profile also becomes the default — subsequent commands without `--profile` use it.

If the browser doesn't open automatically (SSH session, headless, sandbox), print the URL and ask the user to open it manually on their workstation. The poll loop will keep going on the CLI side.

### Failure modes

| Symptom | Fix |
| --- | --- |
| `init` returns 503 | Network blip — the CLI retries 3x. If still failing, ask the user about VPN / firewall. |
| Poll times out after 5 minutes | User didn't approve in time. Re-run `moltable auth login`. |
| Poll returns 410 | The handoff code expired. Re-run `moltable auth login`. |
| Browser opened but no callback | User approved but the page didn't POST back — refresh `app.moltable.io/cli-auth?code=…` once. |

## Add another profile (work + personal)

Use `--profile <name>` on `auth login` to name the new profile. The first profile stays the default; the new one is added alongside.

```
moltable auth login --profile work
```

Then use it per-call with the global flag:

```
moltable --profile work table list --json
```

Or switch the default permanently:

```
moltable profile use work
```

## Inspect / list / remove profiles

```
moltable profile list --json
# [{"name":"personal","default":true,"created":"2026-06-17T10:00:00Z"},
#  {"name":"work","default":false,"created":"2026-06-17T11:30:00Z"}]

moltable profile use <name>     # set default
moltable profile remove <name>  # drop a profile
```

`profile remove` errors if you target the current default while other profiles exist — switch first with `profile use`, then remove.

## Logout (local-only)

```
moltable auth logout                # removes the default profile
moltable auth logout --profile work # removes a named profile
```

**Important to communicate to the user:** `logout` removes the profile from the local TOML file. **It does not revoke the API key on the server.** If the machine is compromised, the user must additionally revoke the key in the web UI at `https://app.moltable.io/settings/api-keys`. The CLI prints this reminder to stderr; surface it.

## Resolver order (precedence)

When multiple sources set a key, this is the order (highest wins):

1. `--api-key molt_…` flag (per-call override)
2. `MOLTABLE_API_KEY` env var (CI-friendly; see `AE4`)
3. `--profile <name>` flag (per-call profile pick)
4. `MOLTABLE_PROFILE` env var
5. `default_profile` in `config.toml`

`auth check --json` shows which source won via the `source` field.

## Local development (`--dev`)

When the user is running the moltable backend locally (e.g. `pnpm exec turbo dev` against a fresh checkout), the CLI's `--dev` global flag retargets every call at `https://localhost:8080` and skips TLS verification for the self-signed devcerts the local API serves. The user-agent also gets a `+dev` suffix so backend access logs distinguish the traffic at a glance.

```
# Per-call flag
moltable --dev auth login --profile dev
moltable --dev workbook list --json

# Or set the env var once per terminal
export MOLTABLE_DEV=1
moltable auth login --profile dev
```

`HandoffNotSupportedError` ("the moltable server doesn't support CLI browser-handoff auth") is the typed error the CLI raises when it hits a server without the `/v1/cli/handoff/*` endpoints — that hint points at `--dev` because today only the local dev API exposes them. Once handoff ships to production, that hint becomes a hint about server staleness rather than dev-vs-prod.

**Safety gate:** `--dev` skips TLS verification ONLY when the resolved host is loopback (`localhost` / `127.0.0.1` / `::1`). Combining `MOLTABLE_DEV=1` with `MOLTABLE_API_BASE=https://something-else.example` keeps full TLS verification — the flag won't silently MITM a remote host. If the user has both set and points at a remote host, the dev hint in their environment is ignored for TLS purposes (but the user-agent suffix still applies, so support can spot the inconsistency in logs).

For automation / non-interactive contexts, `--dev` works the same way. Just remember to also provide a key (env var or profile) — `moltable --dev workbook list` without a key still resolves the auth chain and exits 2 if nothing matches.

## CI-friendly auth

For CI, prefer the env var. No login dance, no TOML config:

```yaml
env:
  MOLTABLE_API_KEY: ${{ secrets.MOLTABLE_API_KEY }}
run: |
  moltable row import --table tb_X --csv leads.csv --json
```

`auth check` against an env var works the same way — it'll report `"source":"env"`.

## What `config.toml` looks like

Lives at `$XDG_CONFIG_HOME/moltable/config.toml` (defaults to `~/.config/moltable/config.toml`):

```toml
default_profile = "personal"

[profiles.personal]
api_key = "molt_…"
created = 2026-06-17T10:00:00Z

[profiles.work]
api_key = "molt_…"
created = 2026-06-17T11:30:00Z
```

The CLI writes this file atomically (temp + rename) under a file lock, so it's safe to `auth login` from two terminals at once.

## Tips for agents

- Always run `moltable auth check --json` before any other command in a fresh session. Use the `org_id` and `profile` fields to ground the user on which workspace they're operating in.
- If the user says "log me into work too", they want a *second* profile — pass `--profile work`, don't overwrite `personal`.
- Don't `cat` the TOML file to read the API key — use `moltable config get api-key` instead (see the `long-tail-fallback` skill).
