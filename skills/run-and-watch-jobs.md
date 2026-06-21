---
name: run-and-watch-jobs
description: Kick off, monitor, and stop moltable enrichment runs — table-level, cell-level, or reattaching to an in-flight job by id.
when_to_use: When the user wants to (re-)execute moltable enrichment on an existing table, watch progress of a running job, stop a stuck or expensive run, or debug a job that finished with errors. Triggers include "run my table", "re-run failed cells", "watch this job", "stop the running enrichment", "show me progress on tb_X".
---

# Run and Watch Jobs

moltable executes columns asynchronously on the server. The CLI kicks off jobs and (optionally) consumes the server-sent-events (SSE) stream to surface progress.

## Three execution modes

Pick based on what the user wants:

1. **Whole table** — `moltable run table <tb_id>` — runs every Moltygent/derived column on every row.
2. **Single cell** — `moltable run cell --table <tb_id> --row <row_id> --column <col_id>` — re-runs one cell. Use for retrying failures.
3. **Reattach** — `moltable run watch <job-id>` — pure watcher, doesn't kick off anything. Use when a job was started elsewhere (different terminal, web UI, CI).

## Watching: three flavors

The `--watch` and `--wait` flags control how the CLI surfaces progress.

| Flag | Stdout behavior | Exit | Good for |
| --- | --- | --- | --- |
| (neither) | Prints `job_id` immediately. | 0 | Fire-and-forget. |
| `--watch` | TTY: progress bar on stderr, summary at end. Pipe / `--json`: JSON-lines on stdout. | 0 when a `job:update` with `status=success` arrives; non-zero on `failed`/`cancelled`. | Interactive use; agents (with `--json`). |
| `--wait --timeout 1h` | Silent until terminal. Then prints final summary. | 0 on success, non-zero on failure or timeout. | CI scripts (no streaming). |

Default for agent-driven use: `--watch --json`.

## Kick off a table run

```
moltable run table <tb_id> --watch --json
```

Event stream (one JSON object per line). The CLI re-emits the server's SSE event types verbatim — the kinds you'll see are `cell:update`, `column:progress`, and `job:update`:

```json
{"event":"job:update","job_id":"job_K","status":"queued","done_cells":0,"total_cells":100}
{"event":"column:progress","column_id":"col_2","done":42,"total":100}
{"event":"column:progress","column_id":"col_2","done":78,"total":100}
{"event":"cell:update","row_id":"row_3","column_id":"col_2","status":"success"}
{"event":"job:update","job_id":"job_K","status":"success","done_cells":98,"failed_cells":2}
```

The stream ends after the first `job:update` whose `status` is terminal (`success`, `failed`, or `cancelled`). Exit code 0 on `success` (even with `failed_cells > 0` — partial failure is normal; surface the count). Non-zero on `failed`/`cancelled`.

If the SSE connection drops mid-stream, the CLI reconnects with `Last-Event-ID` automatically. You should not need to handle reconnection.

## Kick off a single cell

```
moltable run cell \
  --table <tb_id> \
  --row <row_id> \
  --column <col_id> \
  --watch --json
```

Same event shape as table runs, but limited to one cell. Use this for retry-after-failure: read the table, find rows where `status=failed`, re-run each.

## Reattach to a running job

If a run was started earlier (e.g. on the web UI), watch it from the CLI:

```
moltable run watch <job_id> --json
```

This doesn't kick off anything new; it filters the table's SSE stream to events matching `job_id`. Exits when a `job:update` with terminal `status` (`success`/`failed`/`cancelled`) arrives.

## Stop a running job

```
moltable stop <tb_id>
```

Output: `{"status":"stopped"}` (the server does not yet return a count of cancelled cells; if/when a `stopped` field is added it will flow through untouched). Idempotent — running it again returns the same shape.

Use this when:

- The user changed their mind about an expensive run.
- Cells keep erroring and the user wants to abort.
- A run looks stuck (no `column:progress` events for several minutes).

## Debugging stuck or failing jobs

Symptoms and what to do:

| Symptom | Diagnosis | Action |
| --- | --- | --- |
| No `column:progress` for 60+ seconds | Server queue backed up, or a column's tool call is hanging. | Wait one more cycle. If still stuck, `moltable stop <tb_id>` and re-run; consider switching `model` or reducing `tools` in the column's source_config. |
| `job:error` event | Server-side fatal error. | Read the `message` field — common causes: missing BYOK credentials, invalid column reference, model rate limit. Surface to user. |
| `failed` count high (>10%) | Bad prompt, unreliable source data, or rate-limited model. | Use `moltable table get <tb_id> --json` to see per-cell error messages; consider `run cell` retries for transient failures, or edit the column prompt for systemic ones. |
| `--watch` exits 0 but no terminal `job:update` (status=success/failed/cancelled) seen | Stream ended early — likely a CLI bug or proxy timeout. | Reattach with `moltable run watch <job_id>` to confirm final state. |

## Output to JSON-only sink (CI)

Agents and CI scripts should always pass `--json`. This guarantees:

- Stdout is JSON-lines (one event per line, no banners).
- Stderr is reserved for warnings (deprecation, update nudge) — safely ignorable in pipelines.
- TTY progress bars / spinners / colors are suppressed.
- Exit code reflects job outcome (0 for a terminal `job:update` with `status=success`, non-zero for `failed`/`cancelled` or `--timeout` exceeded).

Combine with `--wait --timeout` for the bulletproof shape:

```bash
moltable run table tb_X --wait --timeout 1h --json
```

This prints nothing until the job finishes, then prints one terminal JSON object and exits.

## Putting it together (AE3: terminal-only run)

```
$ moltable run table tb_X --watch
[█████████████████··········] 78/100 columns done · 4:32 elapsed · 1:12 remaining
✓ Done. 98 succeeded, 2 failed (4 min 32 s).
$ echo $?
0
```

Or with `--json`:

```
$ moltable run table tb_X --watch --json
{"event":"job:update","job_id":"job_K","status":"queued","done_cells":0,"total_cells":100}
{"event":"column:progress","column_id":"col_2","done":42,"total":100}
{"event":"job:update","job_id":"job_K","status":"success","done_cells":98,"failed_cells":2}
```

## Tips for agents

- Default to `--watch --json` so you can react to per-event progress.
- Save the `job_id` from the first `job:update` — you'll need it for `run watch` if the connection drops, or for `moltable stop` (though `stop` takes the table id, not the job id).
- If a user kicks off a long run and walks away, store the `job_id` so they can resume watching later with `moltable run watch <job_id>`.
- For batch retries after a partial-failure run, iterate the failed cells with `moltable run cell` in parallel — moltable handles concurrency server-side.
