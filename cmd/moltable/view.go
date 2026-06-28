// `moltable view` — list / get / search.
//
// Views are saved table viewing states (filter + sort + visible columns +
// pinning). Each table has exactly one is_default=true view; callers can
// add more for common filter presets. The CLI exposes:
//
//   GET    /v1/tables/{tableId}/views                      view list
//   GET    /v1/tables/{tableId}/views/{viewId}             view get
//   GET    /v1/tables/{tableId}/views/{viewId}/search      view search
//
// `view search` is the agent-native counterpart to the web's Cmd-F-style
// in-view search bar. It's a substring search across cells inside the
// view's filtered row set, capped at 5000 matched cells server-side. The
// response shape (match_count, truncated, row_set_truncated, matches[])
// is designed for agents to act on: each match carries row_id +
// matching_column_ids so the caller can read just the matched cells
// rather than paginating the full row.
//
// Per-org+table rate limit (5 req/s, burst 10) — bursting from a single
// CLI invocation lands in one bucket; parallel invocations across many
// tables fan out across buckets.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alecthomas/kong"

	molterrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/httpc"
	"github.com/moltable/cli/internal/output"
)

// viewSummary is the subset of domain.View the human render needs.
// The full server payload (filters, sort, visible_columns, column_widths,
// column_pinning) passes through unchanged in --json mode.
type viewSummary struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	IsDefault bool      `json:"is_default"`
	Position  int       `json:"position"`
	CreatedAt time.Time `json:"created_at"`
}

// --- view list ---------------------------------------------------

func (c *ViewListCmd) Run(kctx *kong.Context, root *CLI) error {
	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodGet,
		Path:   "/v1/tables/" + c.Table + "/views",
	})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "table", c.Table); err != nil {
		return err
	}

	if c.JSON {
		var raw any
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return fmt.Errorf("view list: decode response: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}

	var entries []viewSummary
	if err := json.Unmarshal(resp.Body, &entries); err != nil {
		return fmt.Errorf("view list: decode response: %w", err)
	}
	if len(entries) == 0 {
		fmt.Fprintln(kctx.Stdout, "No views.")
		return nil
	}

	tw := tabwriter.NewWriter(kctx.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tDEFAULT\tPOS\tCREATED")
	for _, e := range entries {
		def := ""
		if e.IsDefault {
			def = "yes"
		}
		created := ""
		if !e.CreatedAt.IsZero() {
			created = e.CreatedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", e.ID, e.Name, def, e.Position, created)
	}
	return tw.Flush()
}

// --- view get ----------------------------------------------------

func (c *ViewGetCmd) Run(kctx *kong.Context, root *CLI) error {
	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodGet,
		Path:   "/v1/tables/" + c.Table + "/views/" + c.ID,
	})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "view", c.ID); err != nil {
		return err
	}

	if c.JSON {
		var raw any
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return fmt.Errorf("view get: decode response: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}

	var v viewSummary
	if err := json.Unmarshal(resp.Body, &v); err != nil {
		return fmt.Errorf("view get: decode response: %w", err)
	}
	def := "no"
	if v.IsDefault {
		def = "yes"
	}
	created := ""
	if !v.CreatedAt.IsZero() {
		created = v.CreatedAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(kctx.Stdout, "View %s\n", v.ID)
	fmt.Fprintf(kctx.Stdout, "  Name:     %s\n", v.Name)
	fmt.Fprintf(kctx.Stdout, "  Table:    %s\n", c.Table)
	fmt.Fprintf(kctx.Stdout, "  Default:  %s\n", def)
	fmt.Fprintf(kctx.Stdout, "  Position: %d\n", v.Position)
	if created != "" {
		fmt.Fprintf(kctx.Stdout, "  Created:  %s\n", created)
	}
	return nil
}

// --- view search -------------------------------------------------

// viewSearchRowMatch mirrors the API's ViewSearchRowMatch JSON shape.
type viewSearchRowMatch struct {
	RowID             string   `json:"row_id"`
	Position          int      `json:"position"`
	MatchingColumnIDs []string `json:"matching_column_ids"`
}

// viewSearchResult mirrors the API's ViewSearchResult JSON shape.
// Both truncation signals are surfaced in the human render so the
// caller can tell the two limits apart without --json.
type viewSearchResult struct {
	Query           string               `json:"query"`
	ViewID          string               `json:"view_id"`
	MatchCount      int                  `json:"match_count"`
	Truncated       bool                 `json:"truncated"`
	RowSetTruncated bool                 `json:"row_set_truncated"`
	Matches         []viewSearchRowMatch `json:"matches"`
}

// humanMatchRowsCap bounds the human-render row count so a 5000-match
// response doesn't dump 5000 lines into a terminal. --json mode always
// returns every row (agent's responsibility to paginate / filter).
const humanMatchRowsCap = 50

func (c *ViewSearchCmd) Run(kctx *kong.Context, root *CLI) error {
	q := strings.TrimSpace(c.Query)
	if q == "" {
		return &molterrors.InvalidInputError{
			Field:  "query",
			Detail: "search query is empty",
		}
	}

	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	params := url.Values{}
	params.Set("q", q)
	if c.Limit > 0 {
		params.Set("limit", strconv.Itoa(c.Limit))
	}

	// The server bounds its own work at 10s via statement_timeout +
	// handler context. 30s here is a generous network/serialization
	// budget — enough for slow connections without masking server hangs.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodGet,
		Path:   "/v1/tables/" + c.Table + "/views/" + c.View + "/search?" + params.Encode(),
	})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "view", c.View); err != nil {
		return err
	}

	if c.JSON {
		var raw any
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return fmt.Errorf("view search: decode response: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}

	var result viewSearchResult
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return fmt.Errorf("view search: decode response: %w", err)
	}

	if len(result.Matches) == 0 {
		// Distinguish "no matches" from "row set was truncated and the
		// rows we DID search had no matches" — the latter is a strong
		// "narrow your filter" hint, not a real zero-result.
		if result.RowSetTruncated {
			fmt.Fprintf(kctx.Stdout,
				"No matches in the first ~5000 rows of the view. "+
					"The view's row set exceeded the per-search cap; narrow the filter "+
					"or split the view to search the remaining rows.\n")
			return nil
		}
		fmt.Fprintln(kctx.Stdout, "No matches.")
		return nil
	}

	// Summary line — match_count is CELL count (one per row×column
	// matched cell), len(Matches) is row count. Calling out both keeps
	// the agent from misreading the number.
	suffix := ""
	if result.Truncated {
		suffix = "+ (cell-match cap reached)"
	}
	fmt.Fprintf(kctx.Stdout, "%d matched cells across %d rows%s.\n",
		result.MatchCount, len(result.Matches), suffix)
	if result.RowSetTruncated {
		fmt.Fprintf(kctx.Stdout,
			"Row set truncated: searched only the first ~5000 rows of the view. "+
				"Narrow the filter to search more.\n")
	}

	tw := tabwriter.NewWriter(kctx.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROW_ID\tPOS\tMATCHING_COLUMNS")
	shown := result.Matches
	if len(shown) > humanMatchRowsCap {
		shown = shown[:humanMatchRowsCap]
	}
	for _, m := range shown {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", m.RowID, m.Position, strings.Join(m.MatchingColumnIDs, ","))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(result.Matches) > len(shown) {
		fmt.Fprintf(kctx.Stdout, "... and %d more rows. Use --json for the full list.\n",
			len(result.Matches)-len(shown))
	}
	return nil
}
