// `moltable run watch` + top-level `moltable watch` — SSE consumer.
//
// Both verbs share the same body: resolve the job → look up its table
// ID → open the table's SSE stream → filter events to that job_id (or
// to cell/column events that fall under that job's scope) → emit one
// JSON object per line on stdout (--json) or a stderr progress bar
// (TTY mode).
//
// Endpoint reference (verified against router.go):
//
//   GET /v1/jobs/{jobId}              — job lookup (org-scoped, no table needed)
//   GET /v1/tables/{tableId}/stream   — SSE event stream
//
// Filtering rules (agent-friendly outputs):
//
//   • job:update events → emit only when data.job_id == target job
//   • cell:update / column:progress → also emit; the table-scoped
//     stream may carry events from other jobs running on the same
//     table, but in practice run table/cell creates a single dominant
//     job. We err on the side of "show more" so agents see cell
//     progress without an extra subscription.
//
// Library: github.com/r3labs/sse/v2 — handles the Last-Event-ID
// reconnect dance and exponential backoff for us.

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	sse "github.com/r3labs/sse/v2"
	"gopkg.in/cenkalti/backoff.v1"

	"github.com/moltable/cli/internal/auth"
	molterrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/httpc"
	"github.com/moltable/cli/internal/output"
)

// --- moltable run watch -------------------------------------------

func (c *RunWatchCmd) Run(kctx *kong.Context, root *CLI) error {
	tableID, err := lookupJobTable(root, c.JobID)
	if err != nil {
		return err
	}
	return streamAndRender(kctx, root, tableID, c.JobID, c.Timeout, c.JSON, c.JQ)
}

// --- moltable watch (top-level alias) -----------------------------

func (c *WatchCmd) Run(kctx *kong.Context, root *CLI) error {
	tableID, err := lookupJobTable(root, c.JobID)
	if err != nil {
		return err
	}
	return streamAndRender(kctx, root, tableID, c.JobID, c.Timeout, c.JSON, c.JQ)
}

// lookupJobTable resolves a job ID to its table ID via GET /v1/jobs/{id}.
// Used by `watch` / `run watch` to figure out which SSE stream to open.
func lookupJobTable(root *CLI, jobID string) (string, error) {
	client, err := newAPIClient(root)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodGet,
		Path:   "/v1/jobs/" + jobID,
	})
	if err != nil {
		return "", err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "job", jobID); err != nil {
		return "", err
	}
	var job jobSummary
	if err := json.Unmarshal(resp.Body, &job); err != nil {
		return "", fmt.Errorf("watch: decode job: %w", err)
	}
	if job.TableID == "" {
		return "", &molterrors.NotFoundError{Kind: "job", ID: jobID}
	}
	return job.TableID, nil
}

// streamAndRender opens the table's SSE stream, filters events to the
// specified jobID, and either emits JSON-lines (jsonOut=true OR
// non-TTY) or renders a TTY progress bar in stderr. Returns when:
//
//   - A job:update for jobID arrives with a terminal status; OR
//   - The supplied timeout elapses (error); OR
//   - The context is canceled (e.g. ctrl-C).
//
// Exported within the package so run.go's --watch path reuses it.
func streamAndRender(
	kctx *kong.Context,
	root *CLI,
	tableID, jobID string,
	timeout time.Duration,
	jsonOut bool,
	jqExpr string,
) error {
	cfg, err := loadConfig(root.Config)
	if err != nil {
		return err
	}
	in := auth.FromEnvironment(root.APIKey, cfg)
	if root.Profile != "" && in.FlagAPIKey == "" && in.EnvAPIKey == "" {
		in.EnvProfile = root.Profile
	}
	apiKey, _, rerr := auth.Resolve(in)
	if rerr != nil {
		return rerr
	}
	apiBase := resolveAPIBase(root.Dev)

	if timeout <= 0 {
		timeout = time.Hour
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Build SSE client. We attach the Bearer auth header so the API's
	// requireAuth middleware accepts the connection. r3labs handles the
	// Last-Event-ID reconnect dance internally — it stamps the value
	// from the server's `id:` field on every reconnect.
	streamURL := strings.TrimRight(apiBase, "/") + "/v1/tables/" + tableID + "/stream"
	sseClient := sse.NewClient(streamURL)
	ua := buildUserAgent(root.Dev)
	if root.Dev {
		// r3labs/sse owns its own *http.Client, separate from httpc's
		// transport. Mirror the InsecureSkipVerify here so SSE works
		// against the same self-signed devcerts the rest of the CLI
		// has already accepted under --dev — BUT only if the resolved
		// host is loopback. Otherwise a MOLTABLE_API_BASE pointing at a
		// remote host (set in a parent shell, devcontainer image, dotfile)
		// combined with --dev would silently MITM the SSE stream of cells
		// and column events.
		if httpc.IsLoopbackURL(apiBase) {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- gated on --dev / MOLTABLE_DEV AND loopback host check.
			sseClient.Connection.Transport = tr
		}
	}
	sseClient.Headers = map[string]string{
		"Authorization": "Bearer " + apiKey,
		"User-Agent":    ua,
	}
	// We want ctx (the user's --timeout) to be the ONLY stop signal for
	// reconnects. The default ExponentialBackOff caps elapsed time at
	// 15 minutes (DefaultMaxElapsedTime), which means a long --timeout
	// like 1h would silently terminate the SSE loop at minute 15 and
	// — because a closed events channel used to fall through to
	// `return nil` — exit with success even though the job hadn't
	// finished. Setting MaxElapsedTime=0 disables that cap in
	// cenkalti/backoff.v1 (see exponential.go: "It never stops if
	// MaxElapsedTime == 0").
	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = 0
	sseClient.ReconnectStrategy = bo

	// r3labs's SubscribeChanRawWithContext blocks until the initial
	// connect succeeds (returns nil) or fails (returns err). After it
	// returns nil, the library's internal goroutine keeps feeding events
	// into `events` until ctx is canceled — so we call it synchronously
	// to capture the initial-connect error, then enter the read loop.
	events := make(chan *sse.Event, 64)
	if err := sseClient.SubscribeChanRawWithContext(ctx, events); err != nil {
		return fmt.Errorf("watch: SSE subscription failed: %w", err)
	}
	defer sseClient.Unsubscribe(events)

	// Decide rendering mode. Non-TTY collapses to JSON-lines so script
	// pipelines see structured events even without --json. TTY mode
	// without --json shows a progress bar in stderr.
	emitJSON := jsonOut || !isTTY(kctx.Stderr)

	// Progress-bar state. We sum done/total across all known columns and
	// also track the latest job:update for the target job, whichever
	// reports a higher absolute total wins (job:update is authoritative
	// once it arrives).
	state := &watchState{}

	defer func() {
		// Clean up the rendered progress line so the terminal cursor
		// returns to a sane column before the summary fires.
		if !emitJSON {
			fmt.Fprint(kctx.Stderr, "\n")
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// Distinguish "user wanted a deadline" (timeout) from
			// "context canceled" (ctrl-C). DeadlineExceeded gets a
			// typed error so CI exit codes are meaningful.
			if ctx.Err() == context.DeadlineExceeded {
				return &molterrors.GenericError{
					Msg:      fmt.Sprintf("Timed out after %s watching job %s.", timeout, jobID),
					HintText: "Re-run with a longer --timeout, or poll via `moltable run table <id> --wait`.",
				}
			}
			return ctx.Err()

		case ev, ok := <-events:
			if !ok {
				// The events channel only closes when r3labs/sse gives
				// up on reconnects — i.e. its ReconnectStrategy returned
				// backoff.Stop. With MaxElapsedTime=0 above this should
				// not happen before ctx is done, but if it ever does we
				// must NOT silently return nil (the original bug): that
				// would make a mid-watch connection loss look like the
				// job completed successfully.
				return sseChannelClosedError(ctx)
			}
			eventType := string(ev.Event)
			if eventType == "" || eventType == "connected" {
				continue
			}
			// rows:stale + import:* etc. aren't useful here — skip them
			// to reduce noise in JSON-lines output. cell:update and
			// column:progress flow through (see filtering rules above).
			if !isWatchedEvent(eventType) {
				continue
			}

			// Decode the data payload as a generic map so we can both
			// filter and re-emit with the synthetic "event" field.
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				// Bad payload — skip rather than abort.
				continue
			}

			// Filter job:update to ONLY the target job. cell:update and
			// column:progress flow through (no job_id field) so callers
			// see progress that is plausibly attributable to the job.
			if eventType == "job:update" {
				if jid, _ := data["job_id"].(string); jid != "" && jid != jobID {
					continue
				}
			}

			// Update the shared state for the progress bar.
			updateWatchState(state, eventType, data)

			if emitJSON {
				// One JSON object per line, with the synthetic "event"
				// field at the front so awk/jq filters can branch on it.
				out := map[string]any{"event": eventType}
				for k, v := range data {
					out[k] = v
				}
				if err := output.Print(kctx.Stdout, out, jqExpr); err != nil {
					return err
				}
			} else {
				// TTY: rewrite the progress bar on stderr. Stdout stays
				// silent until the terminal event.
				fmt.Fprint(kctx.Stderr, formatProgressBar(state.done, state.total, jobID))
			}

			// Terminal detection: a job:update we passed the filter for
			// with status in {success, failed, cancelled} ends the loop.
			if eventType == "job:update" {
				if status, _ := data["status"].(string); terminalJobStatus(status) {
					if !emitJSON {
						fmt.Fprint(kctx.Stderr, "\n")
						fmt.Fprintf(kctx.Stderr, "Job %s %s (%d/%d cells).\n",
							jobID, status, state.done, state.total)
					}
					if status != "success" {
						return &molterrors.GenericError{
							Msg:      fmt.Sprintf("Job %s ended with status %s.", jobID, status),
							HintText: "Inspect failed cells with `moltable run watch " + jobID + " --json`.",
						}
					}
					return nil
				}
			}
		}
	}
}

// watchState tracks the running totals the TTY progress bar paints.
// Per-column counters arrive via column:progress; the job:update event
// is authoritative for the run-wide total once it fires.
type watchState struct {
	done       int
	total      int
	jobUpdated bool
}

// isWatchedEvent reports whether the named SSE event type is one the
// CLI surfaces. Anything outside this list (rows:stale, import:*, ...)
// is hidden because it doesn't help a single job's caller — they'd add
// JSON-lines noise an agent has to filter past.
func isWatchedEvent(name string) bool {
	switch name {
	case "cell:update", "column:progress", "job:update":
		return true
	}
	return false
}

// updateWatchState mutates state in response to one event payload.
// The job:update path "wins" once it arrives because the server emits
// it with the authoritative table-wide totals.
func updateWatchState(state *watchState, eventType string, data map[string]any) {
	switch eventType {
	case "job:update":
		done, ok1 := toInt(data["done_cells"])
		total, ok2 := toInt(data["total_cells"])
		if ok1 && ok2 {
			state.done = done
			state.total = total
			state.jobUpdated = true
		}
	case "column:progress":
		// Don't fight job:update once it's seeded the totals — the
		// per-column counters are partial and would regress the bar.
		if state.jobUpdated {
			return
		}
		// We treat column:progress as best-effort: the table-stream hub
		// emits one event per column per change, so the absolute total
		// requires summing across columns we don't track. For the
		// "first event" case (before job:update arrives) we paint a
		// rough bar from the success+failed counters of the most recent
		// column. This is intentionally imprecise — it just shows
		// motion until job:update lands the real numbers.
		success, _ := toInt(data["success"])
		failed, _ := toInt(data["failed"])
		queued, _ := toInt(data["queued"])
		running, _ := toInt(data["running"])
		colDone := success + failed
		colTotal := colDone + queued + running
		if colTotal > state.total {
			state.total = colTotal
		}
		if colDone > state.done {
			state.done = colDone
		}
	}
}

// sseChannelClosedError decides what to return when the SSE events
// channel closes. If ctx is already done, the ctx error path "wins"
// (the user hit --timeout or ctrl-C — that's a normal terminal
// condition handled by the ctx.Done branch elsewhere; we just echo
// ctx.Err() for callers that route here directly). If ctx is NOT
// done, the channel closing means r3labs/sse exhausted its reconnect
// strategy before the watch could finish — surface that as a typed
// error so the user can distinguish "watch finished" from "we gave up
// reconnecting".
func sseChannelClosedError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("watch: SSE connection lost; reconnects exhausted before timeout")
}

// toInt extracts an int from JSON-decoded any (which arrives as
// float64 from encoding/json's default codec).
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}
