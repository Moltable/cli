---
name: long-tail-fallback
description: Fall back to direct REST API calls (with curl) for moltable operations the CLI doesn't cover — views, connections, imports, sample sets, checkpoints, webhooks, API key management, and folder reorganization.
when_to_use: When the user asks for a moltable operation and the marquee CLI commands (workbook, table, column, row, run, stop, auth, profile, config, version, upgrade, skills) don't cover it. Triggers include "create a saved view", "set up a Salesforce connection", "configure a webhook ingest", "rotate my API key", "move this table to a different folder", "create a sample set", "set a checkpoint", or any moltable operation that doesn't map to a marquee verb.
---

# Long-Tail Fallback (CLI → REST)

The moltable CLI v1 covers the high-traffic flows. For everything else — views, connections, imports, sample sets, checkpoints, webhook ingest, API key management, folder reorganization — fall back to the REST API directly via `curl`.

## The critical setup

You need the API key. **Do not assume `MOLTABLE_API_KEY` is set in the environment** — on most machines, the user logged in via `moltable auth login` (browser handoff), and the key is stored in `~/.config/moltable/config.toml`, not exported as an env var.

**Always run this first:**

```bash
moltable config get api-key
```

That prints the active profile's full `molt_…` key to stdout. Capture it into a shell variable:

```bash
MOLT_KEY=$(moltable config get api-key)
```

(If the user has multiple profiles, pass `--profile <name>` to scope the fetch: `moltable config get api-key --profile work`.)

If `config get api-key` errors with "no profile configured", stop and run the `auth-and-profiles` skill — the user needs to log in first.

## Standard curl shape

```bash
curl -sS \
  -H "Authorization: Bearer $MOLT_KEY" \
  -H "Content-Type: application/json" \
  https://api.moltable.io/v1/<endpoint>
```

Add `-X POST -d '{"...":"..."}'` for writes. Add `| jq` to format JSON output. Add `-w '\nHTTP %{http_code}\n'` to surface the status code on the next line.

For local dev against a non-prod server, swap `https://api.moltable.io` for `http://localhost:8080` (or whatever the `moltable config show` output reports under `api_url`).

## Endpoint categories not covered by the CLI

The full surface lives in the OpenAPI spec — published alongside the
moltable deployment the CLI is targeting. Ask your admin or check
`https://<your-moltable-host>/openapi.yaml` (path varies by deploy).
Once you have the spec, fetch and grep it when you need exact
request/response shapes. Highlights:

### Views (saved row filters + column visibility)

- `GET /v1/tables/{table_id}/views` — list saved views on a table.
- `POST /v1/tables/{table_id}/views` — create a saved view (body: `{name, filters, hidden_columns, sort}`).
- `PATCH /v1/views/{view_id}` — rename / update.
- `DELETE /v1/views/{view_id}` — delete.
- Views are how the user persists "show me only failed rows" or "show me prospects in Q3 pipeline".

### Connections (external integrations)

- `GET /v1/connections` — list configured integrations (Salesforce, HubSpot, Attio, Clay, etc.).
- `POST /v1/connections` — create. Body shape varies per provider; reference OpenAPI.
- `DELETE /v1/connections/{id}` — disconnect.
- After creating a connection, reference it from a column via `source: "integration"`.

### Imports (bulk row uploads)

- `POST /v1/imports` — create an import job (returns `import_id`, `upload_url`).
- `POST /v1/imports/{import_id}/upload` — multipart-upload the CSV/JSON payload.
- `GET /v1/imports/{import_id}` — poll status.
- The CLI's `row import` uses this under the hood — fall back to direct calls only for unusual file sizes or async patterns.

### Sample sets (deterministic sub-selections)

- `GET /v1/tables/{table_id}/sample-sets` — list.
- `POST /v1/tables/{table_id}/sample-sets` — create a frozen sample (e.g. 20 random rows for prompt tuning).
- Use these when you want a stable evaluation set across prompt iterations.

### Checkpoints (point-in-time snapshots)

- `POST /v1/tables/{table_id}/checkpoints` — snapshot current state.
- `POST /v1/tables/{table_id}/checkpoints/{id}/restore` — roll back.
- Recommended before destructive prompt edits.

### Webhook ingest

- `POST /v1/tables/{table_id}/webhooks` — provision an inbound webhook (returns a `url` + `secret`).
- External systems POST rows to that URL; moltable validates the HMAC and creates rows.
- `GET /v1/tables/{table_id}/webhooks` and `DELETE /v1/webhooks/{id}` round out CRUD.

### API key management

- `GET /v1/api-keys` — list keys on the active org.
- `POST /v1/api-keys` — create. Body: `{name}`. Response includes the raw `molt_…` only once — capture it.
- `DELETE /v1/api-keys/{id}` — revoke. **This is how the user actually rotates a key** — the CLI's `auth logout` is local-only.

### Folder reorganization

- `PATCH /v1/workbooks/{id}` — body `{folder_id}` moves between folders.
- `POST /v1/folders` / `PATCH /v1/folders/{id}` / `DELETE /v1/folders/{id}` — manage folder tree.

## Worked example: rotate the active API key

```bash
MOLT_KEY=$(moltable config get api-key)

# Mint a new key
NEW_KEY=$(curl -sS \
  -H "Authorization: Bearer $MOLT_KEY" \
  -H "Content-Type: application/json" \
  -X POST https://api.moltable.io/v1/api-keys \
  -d '{"name":"CLI rotated 2026-06-17"}' \
  | jq -r '.api_key')

# Look up the current key's ID (only metadata, never the raw key)
OLD_ID=$(curl -sS \
  -H "Authorization: Bearer $MOLT_KEY" \
  https://api.moltable.io/v1/api-keys \
  | jq -r --arg p "${MOLT_KEY:0:8}" '.[] | select(.prefix == $p) | .id')

# Revoke the old one
curl -sS \
  -H "Authorization: Bearer $MOLT_KEY" \
  -X DELETE "https://api.moltable.io/v1/api-keys/$OLD_ID"

# Save the new key as a profile
# (No --api-key-stdin shortcut today — append to ~/.config/moltable/config.toml directly,
# then switch with `moltable profile use rotated`. The config schema is:
#
#   default_profile = "rotated"
#   [profiles.rotated]
#   api_key = "molt_…"
#   created = 2026-01-01T00:00:00Z
#
# Or simpler: re-run `moltable auth login --profile rotated` to start a fresh
# browser-handoff flow that mints AND writes a new key for that profile.)
```

## Worked example: create a saved view

```bash
MOLT_KEY=$(moltable config get api-key)

curl -sS \
  -H "Authorization: Bearer $MOLT_KEY" \
  -H "Content-Type: application/json" \
  -X POST https://api.moltable.io/v1/tables/tb_X/views \
  -d '{
    "name": "Failed cells only",
    "filters": [{"column":"col_2","op":"eq","value":"failed"}],
    "hidden_columns": [],
    "sort": [{"column":"row_created","direction":"desc"}]
  }' \
  | jq
```

## Tips for agents

- **Always** start with `moltable config get api-key` — never `$MOLTABLE_API_KEY` (won't be set on browser-auth machines).
- Quote the env var when interpolating: `-H "Authorization: Bearer $MOLT_KEY"`. Bash word-splitting will silently break the request otherwise.
- Echo + jq the response on first contact with a new endpoint so you can see the actual response shape — the OpenAPI spec is the source of truth but is large.
- If a marquee CLI command *does* exist for the operation, prefer it — the CLI handles auth, retries, JSON contract, deprecation warnings, and TTY detection that you'd otherwise have to redo.
- For state-mutating operations, capture the response body before doing anything else (`var=$(curl ...)`) — the API only returns once.
