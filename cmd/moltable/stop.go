// `moltable stop` — POST /v1/tables/{id}/stop.
//
// Halts every active job on the table. The server responds with a
// `{"status":"stopped"}` envelope (no count today — the API hides the
// number of jobs that were terminated). The human render echoes the
// table ID and points at `run watch` for follow-up; --json passes the
// server response through unchanged so future server work that adds a
// `stopped: N` field flows out untouched.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/alecthomas/kong"

	"github.com/moltable/cli/internal/httpc"
	"github.com/moltable/cli/internal/output"
)

func (c *StopCmd) Run(kctx *kong.Context, root *CLI) error {
	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodPost,
		Path:   "/v1/tables/" + c.ID + "/stop",
	})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "table", c.ID); err != nil {
		return err
	}

	if c.JSON {
		var raw any
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return fmt.Errorf("stop: decode response: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}
	fmt.Fprintf(kctx.Stdout, "Stopped active jobs on table %s.\n", c.ID)
	return nil
}
