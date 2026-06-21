// `moltable table` — create / list / get / export.
//
// All four verbs share newAPIClient + mapStatusError from workbook.go.
// Endpoint reference (verified against router.go):
//
//   POST   /v1/workbooks                    workbook create
//   GET    /v1/workbooks                    workbook list
//   POST   /v1/tables                       table create  (workbook_id in BODY)
//   GET    /v1/tables                       table list (org-wide)
//   GET    /v1/workbooks/s/{shortId}/tables table list  (--workbook filter)
//   GET    /v1/tables/{id}                  table get
//   GET    /v1/tables/{id}/export.csv       table export --format csv
//
// `table export --format json` has no dedicated server endpoint today;
// we synthesize it by GET-ing the table (which already returns rich
// metadata + counts) and emitting that JSON. This is a pragmatic
// choice for v1: the JSON exporter is rarely the primary path,
// agents that want structured data pipe `table get --json` instead,
// and forcing the API to grow an /export.json endpoint just for the
// CLI would be premature. A future row-list endpoint would let this
// verb include rows in the JSON shape; today it stops at the table
// metadata.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/alecthomas/kong"

	molterrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/httpc"
	"github.com/moltable/cli/internal/output"
)

// tableSummary is the human-render shape. The API actually returns
// more fields (mock_enabled, max_concurrency, ...) but the human table
// only needs id/name/workbook_id/created.
type tableSummary struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	WorkbookID string    `json:"workbook_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// --- table create ------------------------------------------------

func (c *TableCreateCmd) Run(kctx *kong.Context, root *CLI) error {
	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]string{
		"name":        c.Name,
		"workbook_id": c.Workbook,
	})
	if err != nil {
		return fmt.Errorf("table create: marshal body: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodPost,
		Path:   "/v1/tables",
		Body:   body,
	})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "table", c.Name); err != nil {
		return err
	}

	var t tableSummary
	if err := json.Unmarshal(resp.Body, &t); err != nil {
		return fmt.Errorf("table create: decode response: %w", err)
	}

	if c.JSON {
		var raw any
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return fmt.Errorf("table create: decode response for --json: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}
	fmt.Fprintf(kctx.Stdout, "Created table %q (%s) in workbook %s.\n", t.Name, t.ID, t.WorkbookID)
	return nil
}

// --- table list --------------------------------------------------

func (c *TableListCmd) Run(kctx *kong.Context, root *CLI) error {
	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	path := "/v1/tables"
	if c.Workbook != "" {
		// The per-workbook listing uses the short-ID-keyed path; the
		// API accepts both UUID and short-id at /s/{shortId} thanks to
		// resolveID. Using `/s/` mirrors what the web app calls and
		// keeps the CLI consistent with what's documented in router.go.
		path = "/v1/workbooks/s/" + c.Workbook + "/tables"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{Method: http.MethodGet, Path: path})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "workbook", c.Workbook); err != nil {
		return err
	}

	if c.JSON {
		var raw any
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return fmt.Errorf("table list: decode response: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}

	var entries []tableSummary
	if err := json.Unmarshal(resp.Body, &entries); err != nil {
		return fmt.Errorf("table list: decode response: %w", err)
	}
	if len(entries) == 0 {
		fmt.Fprintln(kctx.Stdout, "No tables.")
		return nil
	}

	tw := tabwriter.NewWriter(kctx.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tWORKBOOK\tCREATED")
	for _, e := range entries {
		created := ""
		if !e.CreatedAt.IsZero() {
			created = e.CreatedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.ID, e.Name, e.WorkbookID, created)
	}
	return tw.Flush()
}

// --- table get ---------------------------------------------------

// tableDetail mirrors the API's per-table response. We embed an
// untyped `Columns` slice so the human view can show its length even
// if the API grows the column shape later — we never project beyond
// what the human render shows. The full JSON passes through unchanged.
type tableDetail struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	WorkbookID string         `json:"workbook_id"`
	CreatedAt  time.Time      `json:"created_at"`
	RowCount   int            `json:"row_count"`
	ColumnCount int           `json:"column_count"`
	Columns    []any          `json:"columns"`
}

func (c *TableGetCmd) Run(kctx *kong.Context, root *CLI) error {
	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodGet,
		Path:   "/v1/tables/" + c.ID,
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
			return fmt.Errorf("table get: decode response: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}

	var t tableDetail
	if err := json.Unmarshal(resp.Body, &t); err != nil {
		return fmt.Errorf("table get: decode response: %w", err)
	}
	created := ""
	if !t.CreatedAt.IsZero() {
		created = t.CreatedAt.UTC().Format(time.RFC3339)
	}
	// Use ColumnCount when the server populates it; fall back to the
	// embedded slice length so the human view stays informative if the
	// server omits the count field.
	colCount := t.ColumnCount
	if colCount == 0 {
		colCount = len(t.Columns)
	}
	fmt.Fprintf(kctx.Stdout, "Table %s\n", t.ID)
	fmt.Fprintf(kctx.Stdout, "  Name:     %s\n", t.Name)
	fmt.Fprintf(kctx.Stdout, "  Workbook: %s\n", t.WorkbookID)
	fmt.Fprintf(kctx.Stdout, "  Rows:     %d\n", t.RowCount)
	fmt.Fprintf(kctx.Stdout, "  Columns:  %d\n", colCount)
	if created != "" {
		fmt.Fprintf(kctx.Stdout, "  Created:  %s\n", created)
	}
	return nil
}

// --- table export ------------------------------------------------

// exportSummary is the --json shape emitted by `table export`. It is
// intentionally machine-friendly: { id, format, bytes_written, path }
// so an orchestrating agent can confirm the file landed where it asked.
type exportSummary struct {
	ID           string `json:"id"`
	Format       string `json:"format"`
	BytesWritten int    `json:"bytes_written"`
	Path         string `json:"path,omitempty"`
}

func (c *TableExportCmd) Run(kctx *kong.Context, root *CLI) error {
	switch c.Format {
	case "csv":
		return c.runCSV(kctx, root)
	case "json":
		return c.runJSON(kctx, root)
	default:
		// Kong's enum:"csv,json" already rejects anything else, but the
		// defensive default keeps this exhaustive against future drift.
		return &molterrors.InvalidInputError{
			Field:  "--format",
			Detail: "must be csv or json",
		}
	}
}

func (c *TableExportCmd) runCSV(kctx *kong.Context, root *CLI) error {
	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// The export.csv endpoint returns CSV, not JSON. Override Accept so
	// the server picks the right writer.
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodGet,
		Path:   "/v1/tables/" + c.ID + "/export.csv",
		Headers: http.Header{
			"Accept": []string{"text/csv"},
		},
	})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "table", c.ID); err != nil {
		return err
	}

	bytesWritten, err := writeExportBody(kctx.Stdout, c.Out, resp.Body)
	if err != nil {
		return err
	}
	return c.emitExportSummary(kctx, bytesWritten)
}

func (c *TableExportCmd) runJSON(kctx *kong.Context, root *CLI) error {
	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodGet,
		Path:   "/v1/tables/" + c.ID,
	})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "table", c.ID); err != nil {
		return err
	}

	bytesWritten, err := writeExportBody(kctx.Stdout, c.Out, resp.Body)
	if err != nil {
		return err
	}
	return c.emitExportSummary(kctx, bytesWritten)
}

// writeExportBody fans the bytes either to disk (when outPath is set)
// or to stdout (otherwise). Returns the byte count for the summary.
func writeExportBody(stdout io.Writer, outPath string, body []byte) (int, error) {
	if outPath == "" {
		n, err := stdout.Write(body)
		if err != nil {
			return n, fmt.Errorf("export: write stdout: %w", err)
		}
		return n, nil
	}
	if err := os.WriteFile(outPath, body, 0o644); err != nil {
		return 0, fmt.Errorf("export: write %s: %w", outPath, err)
	}
	return len(body), nil
}

func (c *TableExportCmd) emitExportSummary(kctx *kong.Context, bytesWritten int) error {
	// In --json mode we emit a structured summary instead of the human
	// line. When no file is set, suppress the summary entirely because
	// the body itself already went to stdout — appending a JSON object
	// would corrupt the payload.
	if c.JSON {
		if c.Out == "" {
			return nil
		}
		sum := exportSummary{
			ID:           c.ID,
			Format:       c.Format,
			BytesWritten: bytesWritten,
			Path:         c.Out,
		}
		return output.Print(kctx.Stdout, sum, c.JQ)
	}
	// Human summary on stderr only when we wrote to a file — without
	// `-o`, the body already went to stdout and the user can see it.
	if c.Out != "" {
		fmt.Fprintf(kctx.Stderr, "Exported %s (%d bytes) to %s\n", c.ID, bytesWritten, c.Out)
	}
	return nil
}
