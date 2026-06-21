---
name: build-enrichment-table
description: Scaffold a moltable enrichment table end-to-end — workbook, table, input + Moltygent columns, row import, run with watch, and CSV export.
when_to_use: When the user wants to build a moltable that enriches a list of entities (companies, restaurants, leads, schools, repos, anything) using moltable's column sources. Triggers include "build me an enrichment table", "enrich this CSV", "research these companies with moltable", or pasting a CSV and asking moltable to fill in derived fields.
---

# Build an Enrichment Table

End-to-end recipe for turning "a list of entities + things to look up about each one" into a fully populated moltable. Use this whenever the user wants enrichment, not just storage.

## When NOT to use this skill

- Pure storage with no derived columns — skip the Moltygent step.
- Pulling existing data from a connection / integration — use `column add --source integration` (out of scope here; fall back to `long-tail-fallback`).
- One-off cell re-runs on an existing table — use `run-and-watch-jobs`.

## Prerequisites

Before any of the steps below, verify auth is set up. If `moltable auth check --json` errors with exit code 2, stop and run the `auth-and-profiles` skill first.

```
moltable auth check --json
```

Expected output (success):

```json
{"profile":"personal","user":"alice@example.com","org_id":"org_X","key_prefix":"molt_abc"}
```

If exit code is non-zero, do not proceed — fix auth first.

## The flow

Six steps. Each step's output feeds the next. Do not skip steps.

### 1. Create a workbook (the container)

```
moltable workbook create "<workbook name>" --json
```

Save the returned `id` (format: `wb_…`). Suggested name: `"<Entity type> Research"` or `"<Project name>"`.

### 2. Create a table inside the workbook

```
moltable table create --workbook <wb_id> --name "<table name>" --json
```

Save the returned `id` (format: `tb_…`). The name should describe the row set, e.g. `"Chicago Restaurants"` or `"YC W25 batch"`.

### 3. Add columns — input first, then enrichment

**Input columns** hold the user-supplied data. One per CSV header you'll import. `column add` always requires a `--config-*` source for the source_config payload; for `input` columns the config is just `{}`:

```
moltable column add --table <tb_id> --name "Name" --source input --config-arg '{}' --json
moltable column add --table <tb_id> --name "City" --source input --config-arg '{}' --json
```

**Moltygent columns** do the enrichment. One per derived field. Pipe the source_config JSON on stdin:

```
moltable column add \
  --table <tb_id> \
  --name "Cuisine" \
  --source moltygent \
  --config-stdin --json <<'JSON'
{
  "use_case": "web_research",
  "prompt": "What cuisine does {{Name}} in {{City}} serve? Answer with one or two words (e.g. 'Italian', 'Modern American').",
  "model": "claude-sonnet-4-5",
  "tools": ["web_search"],
  "output_schema": {
    "type": "string",
    "description": "Cuisine type"
  }
}
JSON
```

Field reference for `source_config` (Moltygent):

- `use_case` — `web_research` (allows web tools), `pure_llm` (no tools), `vision` (image input).
- `prompt` — Jinja-style template. Reference other columns with `{{Column Name}}`.
- `model` — Pick from the catalog (`claude-sonnet-4-5`, `gpt-4o`, etc.). Omit to use the workspace default.
- `tools` — Array. Common: `web_search`, `code_execution`.
- `output_schema` — JSON Schema fragment describing the answer shape. Use `{"type":"string"}` for simple text, `{"type":"object","properties":{...}}` for structured.

Repeat for each derived field. Order matters only when one column references another via `{{...}}` — moltable resolves dependencies, but cycles error out.

### 4. Import rows from the user's CSV

```
moltable row import --table <tb_id> --csv <path/to/data.csv> --json
```

Expected output: `{"imported":N,"skipped":0}`. If `skipped > 0`, the CSV header didn't match the input column names — re-read the CSV header, confirm names match input columns exactly (case-sensitive), then retry.

CSV header must match the input column **names**, not Moltygent column names. Moltygent columns are computed.

### 5. Run the enrichment with watch

```
moltable run table <tb_id> --watch --json
```

Streams JSON-lines events to stdout, one per line. The CLI re-emits the server's SSE event names verbatim — you'll see `cell:update`, `column:progress`, and `job:update`. Key events:

- `{"event":"job:update","job_id":"job_…","status":"queued|running","done_cells":...,"total_cells":...}` — progress. Save `job_id` from the first one if you need to reference it later.
- `{"event":"column:progress","column_id":"col_…","done":42,"total":100}` — per-column progress.
- `{"event":"job:update","job_id":"job_…","status":"success|failed|cancelled","done_cells":N,"failed_cells":M}` — terminal. Exit 0 on `success`; non-zero on `failed`/`cancelled`.

If the user asked for CI-friendly (no streaming), use `--wait --timeout 1h` instead of `--watch`. Same exit semantics.

If `failed_cells > 0`, that's normal for enrichment (rate limits, missing data, model refusals). Surface the count to the user; offer to re-run failed cells with `moltable run cell` after diagnosing.

### 6. Export results

```
moltable table export <tb_id> --format csv -o <path/to/results.csv>
```

Or `--format json -o results.json` for structured output. Omitting `-o` writes to stdout (good for piping).

## Putting it together (AE1: Chicago restaurants)

```
$ moltable auth check --json
{"profile":"personal","user_id":"user_X","email":"you@example.com","org_id":"org_X","key_prefix":"molt_…"}

$ moltable workbook create "Restaurant Research" --json
{"id":"wb_X","name":"Restaurant Research"}

$ moltable table create --workbook wb_X --name "Chicago Restaurants" --json
{"id":"tb_Y","name":"Chicago Restaurants","workbook_id":"wb_X"}

$ moltable column add --table tb_Y --name "Name" --source input --config-arg '{}' --json
$ moltable column add --table tb_Y --name "Cuisine" --source moltygent --config-stdin --json <<'JSON'
{"use_case":"web_research","prompt":"What cuisine does {{Name}} serve?","output_schema":{"type":"string"}}
JSON

$ moltable row import --table tb_Y --csv chicago-restaurants.csv --json
{"imported":100,"skipped":0}

$ moltable run table tb_Y --watch --json
{"event":"job:update","job_id":"job_K","status":"queued","done_cells":0,"total_cells":100}
{"event":"column:progress","column_id":"col_2","done":42,"total":100}
{"event":"column:progress","column_id":"col_2","done":100,"total":100}
{"event":"job:update","job_id":"job_K","status":"success","done_cells":98,"failed_cells":2}

$ moltable table export tb_Y --format csv -o results.csv
```

Final report to user: "Done. 98 succeeded, 2 failed. Results in results.csv."

## Error recovery

| Symptom | Fix |
| --- | --- |
| `auth check` exits 2 | Run `auth-and-profiles` skill — user needs to log in. |
| `workbook create` 401 | Same — key revoked or expired. |
| `column add` "source must be one of: ..." | Typo in `--source`. Valid: `input`, `formula`, `http`, `js`, `ai`, `webhook`, `send_to_table`, `integration`, `moltygent`. |
| `column add` "Invalid JSON in stdin" | The heredoc body is malformed. Print the JSON, ask user to confirm shape. |
| `row import` reports `skipped > 0` | Re-check CSV header matches input column names exactly. |
| `run table` exits non-zero with `job:error` | Surface error message; offer `moltable run watch <job-id>` to inspect, or `moltable stop <tb_id>` to halt. |
| Run hangs with no progress | Use `run-and-watch-jobs` skill — covers debugging stuck jobs. |

## Tips for agents

- Always pass `--json` on every step. Parsing the JSON is more reliable than parsing the human-readable summaries.
- Save IDs as you go. Reference them by variable in your shell-out plan instead of re-listing.
- Don't ask the user to confirm between every step — the user already approved the goal. Confirm at the *end* with the export path + success/failure counts.
- If the user describes the entities in prose ("companies in fintech"), turn that into a CSV first (often via `build-tam` or by asking) before reaching for moltable.
