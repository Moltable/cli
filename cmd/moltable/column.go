// `moltable column` — add / list.
//
// API contract notes (verified against apps/api/internal/router/router.go
// + apps/api/internal/handler/column.go):
//
//   POST /v1/tables/{tableId}/columns       column add
//     body: {name, source_type, source_config?}  (col_type defaults
//     server-side; the executor also forces JSON/text where required)
//     accepts a single object OR an array; we always send a single
//     object so the response shape is a flat Column, not an array.
//
//   GET  /v1/tables/{tableId}/columns       column list
//     returns []Column with id/name/source_type/source_config/created_at/...
//
// The agent-friendly hook is `--config-stdin`. Moltygent column configs
// run dozens of lines of JSON (multiple connection IDs, tool selection,
// LLM tuning); shoving that onto a single shell line via
// `--config '{...}'` is fragile. The stdin pipe is the primary path
// for Claude Code skills, so we wire all three flag forms but make
// stdin the documented default.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alecthomas/kong"

	molterrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/httpc"
	"github.com/moltable/cli/internal/output"
)

// validSourceTypes mirrors domain.SourceType (apps/api/internal/domain/column.go).
// Kept here instead of importing the API's domain package because the
// CLI is a separate Go module that intentionally doesn't depend on the
// server packages — we duplicate the enum + bump it on the rare
// occasion the server grows a new source type.
var validSourceTypes = []string{
	"input", "formula", "http", "js", "ai", "webhook",
	"send_to_table", "integration", "moltygent",
}

// stdinReader is overridable in tests so the --config-stdin path can be
// driven from a string.Reader without writing to the process's real
// os.Stdin. Default is os.Stdin (the real CLI path).
var stdinReader io.Reader = os.Stdin

// columnSummary is the TTY-render shape. We keep it narrow on purpose;
// the --json path passes the server's raw response through unchanged so
// any new server-side fields (output_schema, run_condition, ...) reach
// agents without a CLI bump.
type columnSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SourceType string `json:"source_type"`
}

// --- column add ---------------------------------------------------

func (c *ColumnAddCmd) Run(kctx *kong.Context, root *CLI) error {
	if !isValidSourceType(c.Source) {
		return &molterrors.InvalidInputError{
			Field:  "--source",
			Detail: "Source must be one of: " + strings.Join(validSourceTypes, ", ") + ".",
		}
	}

	cfg, err := c.readSourceConfig()
	if err != nil {
		return err
	}

	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	// Build the request body. We marshal source_config as json.RawMessage
	// so the user's exact byte sequence travels to the server — the
	// validator on the server side runs against the same bytes the user
	// inspected, so any "but it worked on my machine" diffs come down to
	// transport, not re-encoding.
	body, err := json.Marshal(map[string]any{
		"name":          c.Name,
		"source_type":   c.Source,
		"source_config": json.RawMessage(cfg),
	})
	if err != nil {
		return fmt.Errorf("column add: marshal body: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodPost,
		Path:   "/v1/tables/" + c.Table + "/columns",
		Body:   body,
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
			return fmt.Errorf("column add: decode response: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}

	var col columnSummary
	if err := json.Unmarshal(resp.Body, &col); err != nil {
		return fmt.Errorf("column add: decode response: %w", err)
	}
	fmt.Fprintf(kctx.Stdout, "Created column %q (%s).\n", col.Name, col.ID)
	return nil
}

// readSourceConfig resolves the source_config bytes from whichever of
// --config-stdin / --config-file / --config the user supplied. Exactly
// one must be set; the body always validates as JSON before being sent
// so the user sees a parser error from the CLI, not a generic 400 from
// the API.
//
// Returns the raw JSON bytes (re-serialized so leading whitespace and
// trailing newlines from stdin/files don't pollute the wire payload).
func (c *ColumnAddCmd) readSourceConfig() ([]byte, error) {
	sources := 0
	if c.ConfigStdin {
		sources++
	}
	if c.ConfigFile != "" {
		sources++
	}
	if c.ConfigArg != "" {
		sources++
	}
	if sources == 0 {
		return nil, &molterrors.InvalidInputError{
			Field:  "source_config",
			Detail: "Provide source_config via one of --config-stdin, --config-file, or --config-arg.",
		}
	}
	if sources > 1 {
		return nil, &molterrors.InvalidInputError{
			Field:  "source_config",
			Detail: "Provide only one of --config-stdin, --config-file, --config-arg.",
		}
	}

	var raw []byte
	var origin string
	switch {
	case c.ConfigStdin:
		origin = "stdin"
		b, err := io.ReadAll(stdinReader)
		if err != nil {
			return nil, fmt.Errorf("column add: read stdin: %w", err)
		}
		raw = b
	case c.ConfigFile != "":
		origin = c.ConfigFile
		b, err := os.ReadFile(c.ConfigFile)
		if err != nil {
			return nil, fmt.Errorf("column add: read %s: %w", c.ConfigFile, err)
		}
		raw = b
	default:
		origin = "--config-arg"
		raw = []byte(c.ConfigArg)
	}

	// Validate + re-serialize so we send a canonical JSON object on the
	// wire. The round-trip also catches trailing junk after a valid JSON
	// document, which json.Unmarshal happily accepts but which the
	// server would reject ambiguously.
	var parsed any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&parsed); err != nil {
		return nil, &molterrors.InvalidInputError{
			Field:  "source_config",
			Detail: fmt.Sprintf("Invalid JSON in %s: %s", origin, err.Error()),
		}
	}
	// Drain whitespace/EOF after the first value; anything else is junk.
	if dec.More() {
		return nil, &molterrors.InvalidInputError{
			Field:  "source_config",
			Detail: fmt.Sprintf("Invalid JSON in %s: extra data after first JSON document", origin),
		}
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return nil, fmt.Errorf("column add: re-encode source_config: %w", err)
	}
	return out, nil
}

func isValidSourceType(s string) bool {
	for _, v := range validSourceTypes {
		if v == s {
			return true
		}
	}
	return false
}

// --- column list --------------------------------------------------

func (c *ColumnListCmd) Run(kctx *kong.Context, root *CLI) error {
	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodGet,
		Path:   "/v1/tables/" + c.Table + "/columns",
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
			return fmt.Errorf("column list: decode response: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}

	var entries []columnSummary
	if err := json.Unmarshal(resp.Body, &entries); err != nil {
		return fmt.Errorf("column list: decode response: %w", err)
	}
	if len(entries) == 0 {
		fmt.Fprintln(kctx.Stdout, "No columns. Run `moltable column add` to start.")
		return nil
	}

	tw := tabwriter.NewWriter(kctx.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSOURCE")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.ID, e.Name, e.SourceType)
	}
	return tw.Flush()
}
