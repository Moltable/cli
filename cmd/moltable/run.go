// `moltable run` — table / cell.
//
// The execution surface kicks off jobs over a table and (optionally)
// follows them to completion via SSE (--watch) or polling (--wait).
//
// Endpoint reference (verified against router.go):
//
//   POST /v1/tables/{id}/execute/table       — kicks off all enrichments
//   POST /v1/tables/{id}/execute/cell        — one (row, column) pair
//   GET  /v1/jobs/{id}                       — job state (org-scoped)
//   GET  /v1/tables/{id}/stream              — SSE event stream
//
// `--watch` flag semantics:
//
//   --watch + --json   → JSON-lines on stdout (one event per line)
//   --watch (TTY)      → progress bar in stderr; stdout silent
//   --watch (no TTY)   → JSON-lines on stdout (treats non-TTY like --json)
//
// `--wait` is the CI-friendly alternative: polls GET /v1/jobs/{id}
// every 5s until terminal. No stream consumption, no progress bar.
// Exits non-zero on timeout or terminal failure.
//
// SSE library: github.com/r3labs/sse/v2 — built-in Last-Event-ID
// resume + exponential reconnect; we wire only the Authorization
// header and let the library handle the framing.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/alecthomas/kong"

	molterrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/httpc"
	"github.com/moltable/cli/internal/output"
	"github.com/moltable/cli/internal/ui"
)

// jobSummary is the minimal job shape we depend on. The server returns
// more fields (table_id, scope, target_*, started_at, etc.); we render
// only what the human + watch paths need and pass the rest through in
// --json mode.
type jobSummary struct {
	ID         string `json:"id"`
	TableID    string `json:"table_id"`
	Status     string `json:"status"`
	TotalCells int    `json:"total_cells"`
	DoneCells  int    `json:"done_cells"`
	FailedCells int   `json:"failed_cells"`
}

// terminalJobStatus reports whether a job:update status value means the
// job has reached a final state and the watch/wait loop should exit.
// Mirrors apps/api/internal/domain/job.go: success, failed, cancelled.
func terminalJobStatus(s string) bool {
	switch s {
	case "success", "failed", "cancelled":
		return true
	}
	return false
}

// --- run table ----------------------------------------------------

func (c *RunTableCmd) Run(kctx *kong.Context, root *CLI) error {
	if c.Watch && c.Wait {
		return &molterrors.InvalidInputError{
			Field:  "--watch / --wait",
			Detail: "pick one — --watch streams SSE, --wait polls.",
		}
	}

	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodPost,
		Path:   "/v1/tables/" + c.ID + "/execute/table",
	})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "table", c.ID); err != nil {
		return err
	}

	var job jobSummary
	if err := json.Unmarshal(resp.Body, &job); err != nil {
		return fmt.Errorf("run table: decode response: %w", err)
	}

	return finishRun(kctx, root, client, &job, resp.Body, c.Watch, c.Wait, c.Timeout, c.JSON, c.JQ)
}

// --- run cell -----------------------------------------------------

func (c *RunCellCmd) Run(kctx *kong.Context, root *CLI) error {
	if c.Watch && c.Wait {
		return &molterrors.InvalidInputError{
			Field:  "--watch / --wait",
			Detail: "pick one — --watch streams SSE, --wait polls.",
		}
	}

	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]string{
		"row_id":    c.Row,
		"column_id": c.Column,
	})
	if err != nil {
		return fmt.Errorf("run cell: marshal body: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodPost,
		Path:   "/v1/tables/" + c.Table + "/execute/cell",
		Body:   body,
	})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "table", c.Table); err != nil {
		return err
	}

	var job jobSummary
	if err := json.Unmarshal(resp.Body, &job); err != nil {
		return fmt.Errorf("run cell: decode response: %w", err)
	}

	return finishRun(kctx, root, client, &job, resp.Body, c.Watch, c.Wait, c.Timeout, c.JSON, c.JQ)
}

// finishRun is the shared tail of `run table` and `run cell`: render
// the just-dispatched job in the verbosity the user requested, then —
// if --watch or --wait was set — follow it to completion.
//
// rawBody is the server's raw POST response (passed through unchanged
// when --json is set, so any fields beyond jobSummary survive).
func finishRun(
	kctx *kong.Context,
	root *CLI,
	client *httpc.Client,
	job *jobSummary,
	rawBody []byte,
	watch, wait bool,
	timeout time.Duration,
	jsonOut bool,
	jqExpr string,
) error {
	// Fire-and-forget: no follow.
	if !watch && !wait {
		if jsonOut {
			var raw any
			if err := json.Unmarshal(rawBody, &raw); err != nil {
				return fmt.Errorf("run: decode response for --json: %w", err)
			}
			return output.Print(kctx.Stdout, raw, jqExpr)
		}
		fmt.Fprintf(kctx.Stdout, "Started job %s on table %s.\n", job.ID, job.TableID)
		return nil
	}

	if watch {
		// In --watch mode we want stdout silent before the stream takes
		// over (no "Started job X" line in the way of the JSON-lines).
		// The stream consumer prints any human summary itself.
		return streamAndRender(kctx, root, job.TableID, job.ID, timeout, jsonOut, jqExpr)
	}

	// --wait: poll until terminal.
	return pollJobUntilTerminal(kctx, client, job.ID, timeout, jsonOut, jqExpr)
}

// pollJobUntilTerminal repeatedly GETs /v1/jobs/{id} until the job
// reaches success/failed/cancelled, the timeout elapses, or the context
// is canceled. Exits non-zero on timeout or terminal failure.
//
// Polling interval is fixed at 5s — frequent enough to feel responsive
// for jobs that finish in seconds, sparse enough not to hammer the
// API for long-running ones. Cap the per-request budget at 10s so a
// stuck API call can't blow past the user's --timeout silently.
func pollJobUntilTerminal(
	kctx *kong.Context,
	client *httpc.Client,
	jobID string,
	timeout time.Duration,
	jsonOut bool,
	jqExpr string,
) error {
	if timeout <= 0 {
		timeout = time.Hour
	}
	deadline := time.Now().Add(timeout)
	const pollInterval = 5 * time.Second

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := client.Do(ctx, httpc.Request{
			Method: http.MethodGet,
			Path:   "/v1/jobs/" + jobID,
		})
		cancel()
		if err != nil {
			return err
		}
		if err := mapStatusError(resp.StatusCode, resp.Body, "job", jobID); err != nil {
			return err
		}
		var job jobSummary
		if err := json.Unmarshal(resp.Body, &job); err != nil {
			return fmt.Errorf("run wait: decode job: %w", err)
		}

		if terminalJobStatus(job.Status) {
			if jsonOut {
				var raw any
				if err := json.Unmarshal(resp.Body, &raw); err != nil {
					return fmt.Errorf("run wait: decode job for --json: %w", err)
				}
				if err := output.Print(kctx.Stdout, raw, jqExpr); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(kctx.Stderr, "Job %s %s (%d/%d cells).\n",
					job.ID, job.Status, job.DoneCells, job.TotalCells)
			}
			if job.Status != "success" {
				return &molterrors.GenericError{
					Msg:      fmt.Sprintf("Job %s ended with status %s.", job.ID, job.Status),
					HintText: "Re-run with `moltable run table <id>` after addressing the failure, or inspect with `moltable run watch <job-id>`.",
				}
			}
			return nil
		}

		// Not terminal — sleep until next poll or until --timeout fires.
		now := time.Now()
		if now.After(deadline) {
			return &molterrors.GenericError{
				Msg:      fmt.Sprintf("Timed out after %s waiting for job %s (last status: %s).", timeout, jobID, job.Status),
				HintText: "Re-run with a longer --timeout, or follow live with `moltable run watch " + jobID + "`.",
			}
		}
		wait := pollInterval
		if remaining := deadline.Sub(now); remaining < wait {
			wait = remaining
		}
		time.Sleep(wait)
	}
}

// formatProgressBar returns the compact stderr line painted by the
// watch loop's TTY mode. Width is fixed (see ui.ProgressBarWidth);
// we deliberately don't grow to terminal width — keeps the look
// predictable across narrow + wide terminals.
//
// The actual rendering (colors gated by output.IsTTY) lives in the
// ui package; this wrapper exists so tests + the formatter call site
// keep their existing import surface.
func formatProgressBar(done, total int, jobID string) string {
	return ui.ProgressBar(done, total, jobID)
}
