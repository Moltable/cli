// `moltable workbook` — create / list.
//
// Workbooks are the top-level container for tables. The CLI exposes
// two verbs here: `create <name>` (POSTs to /v1/workbooks) and
// `list` (GETs /v1/workbooks). Both honor --json for agent output;
// human output goes through a small tabwriter so `id`, `name`, and
// `created` columns line up like `gh repo list`.
//
// API contract notes:
//
//   - POST /v1/workbooks accepts {name, description, folder_id, ...}.
//     We only send {name}; everything else is server-defaulted.
//   - GET /v1/workbooks returns []WorkbookWithTableCount — each entry
//     embeds the domain.Workbook fields (id, name, created_at, ...)
//     plus a `table_count`. We render id/name/created in TTY mode and
//     pass the array through untouched in --json mode.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"text/tabwriter"
	"time"

	"github.com/alecthomas/kong"

	"github.com/moltable/cli/internal/auth"
	molterrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/httpc"
	"github.com/moltable/cli/internal/output"
)

// workbookSummary is the shape we render in TTY tables. We only depend
// on the subset of fields the human view actually shows — anything else
// the API returns flows through --json unchanged.
type workbookSummary struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// --- workbook create ---------------------------------------------

func (c *WorkbookCreateCmd) Run(kctx *kong.Context, root *CLI) error {
	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]string{"name": c.Name})
	if err != nil {
		return fmt.Errorf("workbook create: marshal body: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodPost,
		Path:   "/v1/workbooks",
		Body:   body,
	})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "workbook", c.Name); err != nil {
		return err
	}

	// Decode just enough to render the success line. Pass the raw bytes
	// through in --json so callers see whatever the server returned.
	var wb workbookSummary
	if err := json.Unmarshal(resp.Body, &wb); err != nil {
		return fmt.Errorf("workbook create: decode response: %w", err)
	}

	if c.JSON {
		// Pass the server's raw JSON object straight through. We round-
		// trip via a decode-then-Print so --jq works.
		var raw any
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return fmt.Errorf("workbook create: decode response for --json: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}
	fmt.Fprintf(kctx.Stdout, "Created workbook %q (%s).\n", wb.Name, wb.ID)
	return nil
}

// --- workbook list ------------------------------------------------

func (c *WorkbookListCmd) Run(kctx *kong.Context, root *CLI) error {
	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodGet,
		Path:   "/v1/workbooks",
	})
	if err != nil {
		return err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "workbook", ""); err != nil {
		return err
	}

	if c.JSON {
		// Server returns a JSON array; round-trip through interface{} so
		// gojq filters work and so we don't depend on a particular Go
		// shape for the entries (the server may add fields over time).
		var raw any
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return fmt.Errorf("workbook list: decode response: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}

	var entries []workbookSummary
	if err := json.Unmarshal(resp.Body, &entries); err != nil {
		return fmt.Errorf("workbook list: decode response: %w", err)
	}
	if len(entries) == 0 {
		fmt.Fprintln(kctx.Stdout, "No workbooks. Run `moltable workbook create <name>` to start.")
		return nil
	}

	tw := tabwriter.NewWriter(kctx.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tCREATED")
	for _, e := range entries {
		created := ""
		if !e.CreatedAt.IsZero() {
			created = e.CreatedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.ID, e.Name, created)
	}
	return tw.Flush()
}

// --- shared API-client helper ------------------------------------

// newAPIClient resolves the active credential via the tri-layer
// auth chain (flag → env → config profile) and constructs an
// httpc.Client pointed at the right API base. Returns either the
// wired client or a typed error suitable for the central renderer
// in run(). Shared across every command that issues authenticated
// HTTP — table.go, row.go, column.go all call this.
func newAPIClient(root *CLI) (*httpc.Client, error) {
	cfg, err := loadConfig(root.Config)
	if err != nil {
		return nil, err
	}
	in := auth.FromEnvironment(root.APIKey, cfg)
	// Global --profile beats MOLTABLE_PROFILE env when no flag/key was
	// supplied (matches the precedence auth_check follows).
	if root.Profile != "" && in.FlagAPIKey == "" && in.EnvAPIKey == "" {
		in.EnvProfile = root.Profile
	}
	key, _, rerr := auth.Resolve(in)
	if rerr != nil {
		return nil, rerr
	}
	apiBase := resolveAPIBase(root.Dev)
	return httpc.NewWithOptions(apiBase, key, buildUserAgent(root.Dev), httpc.Options{
		InsecureSkipTLSVerify: root.Dev,
	})
}

// mapStatusError turns the API's status code into a typed CLI error.
// The body is parsed only when we need a server-supplied message (the
// 4xx happy paths just return nil so the caller can decode normally).
//
// 401 maps to AuthError → exit code 2. 404 maps to NotFoundError
// with the supplied kind/id → exit code 3. 429 maps to RateLimitError.
// Other 4xx fall through to a generic GenericError so the user sees the
// server's own message rather than a synthetic one.
func mapStatusError(status int, body []byte, kind, id string) error {
	if status >= 200 && status < 300 {
		return nil
	}
	switch status {
	case http.StatusUnauthorized:
		return &molterrors.AuthError{Reason: "401"}
	case http.StatusNotFound:
		return &molterrors.NotFoundError{Kind: kind, ID: id}
	case http.StatusTooManyRequests:
		return &molterrors.RateLimitError{}
	}
	// Try to pull the server's error message; fall back to status text.
	var er struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := http.StatusText(status)
	if err := json.Unmarshal(body, &er); err == nil && er.Error.Message != "" {
		msg = er.Error.Message
	}
	return &molterrors.GenericError{Msg: fmt.Sprintf("%s (HTTP %d).", msg, status)}
}
