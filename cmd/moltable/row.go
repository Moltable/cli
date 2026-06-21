// `moltable row` — create / import.
//
// API contract notes (verified against apps/api/internal/router/router.go
// + apps/api/internal/handler/row.go):
//
//   POST /v1/tables/{tableId}/rows   row create
//     body: {values: {colName-or-id: "string", ...}, count?: int}
//     server resolves column NAMES to IDs via columnLookup, so a CSV
//     header row with "Name", "Email" works without a pre-flight ID
//     fetch. Values are string-valued (the server casts to col_type).
//
// There is NO bulk-insert endpoint today. `row import` streams
// individual POST calls in order, accumulating an `imported` count and
// an `errors` slice. A future bulk endpoint would let us swap in a
// single call here. We don't pretend to support transactions either:
// a mid-import failure leaves the first N rows in the table.
//
// CSV format: header row matches table column NAMES (case-sensitive),
// or the user passes `--column-mapping table-col=csv-col` for each
// renaming. Header validation runs BEFORE any HTTP traffic so a typo
// fails fast with a list of missing/extra columns.

package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	molterrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/httpc"
	"github.com/moltable/cli/internal/output"
)

// rowCreateRequest is the wire shape POST /v1/tables/{id}/rows expects.
// Pulled from apps/api/internal/handler/row.go::Create. The server
// resolves keys via columnLookup (UUID or name).
type rowCreateRequest struct {
	Values map[string]string `json:"values"`
}

// rowSummary is the human-render shape for row create. The server
// returns the full Row domain object; we only render its ID.
type rowSummary struct {
	ID string `json:"id"`
}

// importReport is the --json summary emitted by `row import`. The
// `errors` slice carries one entry per failed row (capped at 50 in
// practice via the cap below) so an agent can decide whether to retry.
type importReport struct {
	Imported int           `json:"imported"`
	Skipped  int           `json:"skipped"`
	Errors   []importError `json:"errors"`
}

type importError struct {
	Row     int    `json:"row"`     // 1-indexed, header excluded
	Message string `json:"message"` // server-supplied where possible
}

// importErrorCap bounds the errors slice so a bad CSV doesn't blow up
// the report payload. Once exceeded, we keep counting `skipped` but
// stop appending to Errors. 50 rows of detail is plenty for triage.
const importErrorCap = 50

// --- row create --------------------------------------------------

func (c *RowCreateCmd) Run(kctx *kong.Context, root *CLI) error {
	// Validate --data parses BEFORE we touch the network so an obvious
	// typo doesn't burn an auth round-trip + a 400 server log.
	var values map[string]string
	if err := json.Unmarshal([]byte(c.Data), &values); err != nil {
		return &molterrors.InvalidInputError{
			Field:  "--data",
			Detail: fmt.Sprintf("Invalid JSON: %s. Expected a flat object of {column: value} strings.", err.Error()),
		}
	}

	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	body, err := json.Marshal(rowCreateRequest{Values: values})
	if err != nil {
		return fmt.Errorf("row create: marshal body: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodPost,
		Path:   "/v1/tables/" + c.Table + "/rows",
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
			return fmt.Errorf("row create: decode response: %w", err)
		}
		return output.Print(kctx.Stdout, raw, c.JQ)
	}

	var row rowSummary
	if err := json.Unmarshal(resp.Body, &row); err != nil {
		return fmt.Errorf("row create: decode response: %w", err)
	}
	fmt.Fprintf(kctx.Stdout, "Created row %s.\n", row.ID)
	return nil
}

// --- row import --------------------------------------------------

func (c *RowImportCmd) Run(kctx *kong.Context, root *CLI) error {
	// Parse --column-mapping flag values into a map. Format: TABLE-COL=CSV-COL.
	mapping, err := parseColumnMapping(c.ColumnMapping)
	if err != nil {
		return err
	}

	// Read the CSV up front so header validation runs BEFORE we ask the
	// server for anything. A large CSV that buffers fully is acceptable
	// at v1 — bulk-import flows that exceed RAM should use the upcoming
	// bulk endpoint, not this command. We document the bound in --help.
	f, err := os.Open(c.CSV)
	if err != nil {
		return fmt.Errorf("row import: open %s: %w", c.CSV, err)
	}
	defer f.Close()
	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1 // tolerate stragglers in trailing newlines
	records, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("row import: parse %s: %w", c.CSV, err)
	}
	if len(records) == 0 {
		return &molterrors.InvalidInputError{
			Field:  "--csv",
			Detail: fmt.Sprintf("%s is empty.", c.CSV),
		}
	}

	header := records[0]
	dataRows := records[1:]

	client, err := newAPIClient(root)
	if err != nil {
		return err
	}

	// Fetch the table's columns so we can validate the CSV header against
	// real column names. This is a single round-trip; subsequent row
	// POSTs use the column NAMES directly (the server resolves them).
	tableCols, err := fetchColumnNames(client, c.Table)
	if err != nil {
		return err
	}

	// Build the per-row value extractor. The CSV column at index i maps
	// to a table column name; for each table column we record which CSV
	// index produces its value. Columns absent from both CSV header and
	// mapping are simply skipped (server treats missing as empty).
	colToCSVIndex, validationErr := buildHeaderMapping(header, tableCols, mapping)
	if validationErr != nil {
		return validationErr
	}

	report := importReport{Errors: []importError{}}
	ctx := context.Background()

	for rowNum, record := range dataRows {
		oneBased := rowNum + 1
		values := make(map[string]string, len(colToCSVIndex))
		for colName, csvIdx := range colToCSVIndex {
			if csvIdx < len(record) {
				values[colName] = record[csvIdx]
			}
		}

		if err := postRow(ctx, client, c.Table, values); err != nil {
			report.Skipped++
			if len(report.Errors) < importErrorCap {
				report.Errors = append(report.Errors, importError{
					Row:     oneBased,
					Message: err.Error(),
				})
			}
			if !c.JSON {
				// Single "x" character per failed row keeps the dots
				// row visually distinct without scrolling — same shape
				// gh uses for its import progress.
				fmt.Fprint(kctx.Stdout, "x")
			}
			continue
		}
		report.Imported++
		if !c.JSON {
			fmt.Fprint(kctx.Stdout, ".")
		}
	}

	if c.JSON {
		return output.Print(kctx.Stdout, report, c.JQ)
	}
	// TTY: terminate the progress dots line + print a summary.
	if len(dataRows) > 0 {
		fmt.Fprintln(kctx.Stdout)
	}
	fmt.Fprintf(kctx.Stdout, "Imported %d row(s); skipped %d.\n", report.Imported, report.Skipped)
	if report.Skipped > 0 {
		fmt.Fprintln(kctx.Stdout, "Re-run with --json to inspect per-row errors.")
	}
	return nil
}

// parseColumnMapping turns the repeatable `--column-mapping K=V` flag
// into a map where the key is the TABLE column name and the value is
// the CSV column header. Empty input → empty map (no remapping).
func parseColumnMapping(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, entry := range raw {
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 || eq == len(entry)-1 {
			return nil, &molterrors.InvalidInputError{
				Field:  "--column-mapping",
				Detail: fmt.Sprintf("Expected table-col=csv-col; got %q.", entry),
			}
		}
		tableCol := strings.TrimSpace(entry[:eq])
		csvCol := strings.TrimSpace(entry[eq+1:])
		if tableCol == "" || csvCol == "" {
			return nil, &molterrors.InvalidInputError{
				Field:  "--column-mapping",
				Detail: fmt.Sprintf("Expected table-col=csv-col; got %q.", entry),
			}
		}
		out[tableCol] = csvCol
	}
	return out, nil
}

// buildHeaderMapping returns a map of TABLE column name → CSV column
// index, given the CSV header row + the table's known column names +
// any explicit --column-mapping overrides.
//
// Validation rules (errors reported in one sentence, like gh import):
//   - Every column listed in --column-mapping (key) must exist on the
//     table; values must exist in the CSV header.
//   - After mapping is applied, the remaining CSV header columns must
//     each map to a real table column. Any extras are reported.
//   - At least one CSV column must map to a real table column, else
//     the import is a no-op (we treat that as an error so the user sees
//     the typo instead of a silent success).
func buildHeaderMapping(header []string, tableCols []string, mapping map[string]string) (map[string]int, error) {
	tableColSet := make(map[string]bool, len(tableCols))
	for _, c := range tableCols {
		tableColSet[c] = true
	}
	headerIdx := make(map[string]int, len(header))
	for i, h := range header {
		headerIdx[h] = i
	}

	out := make(map[string]int, len(header))

	// Apply explicit mappings first.
	var unknownMappedTableCols, unknownMappedCSVCols []string
	for tableCol, csvCol := range mapping {
		if !tableColSet[tableCol] {
			unknownMappedTableCols = append(unknownMappedTableCols, tableCol)
			continue
		}
		idx, ok := headerIdx[csvCol]
		if !ok {
			unknownMappedCSVCols = append(unknownMappedCSVCols, csvCol)
			continue
		}
		out[tableCol] = idx
	}
	if len(unknownMappedTableCols) > 0 || len(unknownMappedCSVCols) > 0 {
		parts := []string{}
		if len(unknownMappedTableCols) > 0 {
			parts = append(parts, fmt.Sprintf("unknown table columns: %s", strings.Join(unknownMappedTableCols, ", ")))
		}
		if len(unknownMappedCSVCols) > 0 {
			parts = append(parts, fmt.Sprintf("unknown CSV columns: %s", strings.Join(unknownMappedCSVCols, ", ")))
		}
		return nil, &molterrors.InvalidInputError{
			Field:  "--column-mapping",
			Detail: strings.Join(parts, "; ") + ".",
		}
	}

	// Then implicit (CSV header == table column name) mappings, skipping
	// any CSV header that was already claimed by an explicit mapping.
	claimedCSV := make(map[int]bool, len(out))
	for _, idx := range out {
		claimedCSV[idx] = true
	}
	var extra []string
	for i, h := range header {
		if claimedCSV[i] {
			continue
		}
		if tableColSet[h] {
			if _, already := out[h]; !already {
				out[h] = i
			}
			continue
		}
		extra = append(extra, h)
	}

	// Missing = table columns NOT covered by any mapping AND NOT in the
	// CSV header. We only flag missing when there's also no mapping at
	// all — partial CSVs (5 of 8 columns) are a perfectly reasonable
	// import case, so a strict "every column must be present" rule
	// would be hostile. But if NONE of the table's columns map to the
	// CSV, that's almost certainly a typo and we should fail loudly.
	if len(out) == 0 {
		var missing []string
		for _, c := range tableCols {
			if _, ok := out[c]; !ok {
				missing = append(missing, c)
			}
		}
		return nil, &molterrors.InvalidInputError{
			Field:  "--csv",
			Detail: fmt.Sprintf("CSV header does not match any table column. Missing: %s; extra: %s.",
				strings.Join(missing, ", "), strings.Join(extra, ", ")),
		}
	}
	if len(extra) > 0 {
		// Soft warning would be nicer, but we don't have a stderr-noise
		// channel for `row import` today and silently dropping columns
		// is the kind of thing that bites Sales-Ops 6 months later. Fail
		// loudly with the extras so the user maps them or removes them.
		return nil, &molterrors.InvalidInputError{
			Field:  "--csv",
			Detail: fmt.Sprintf("CSV has columns not on the table: %s. Map them with --column-mapping or remove them.",
				strings.Join(extra, ", ")),
		}
	}
	return out, nil
}

// fetchColumnNames runs GET /v1/tables/{id}/columns and extracts just the
// `name` field of each entry. Used by row import for header validation.
func fetchColumnNames(client *httpc.Client, tableID string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpc.Request{
		Method: http.MethodGet,
		Path:   "/v1/tables/" + tableID + "/columns",
	})
	if err != nil {
		return nil, err
	}
	if err := mapStatusError(resp.StatusCode, resp.Body, "table", tableID); err != nil {
		return nil, err
	}
	var raw []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return nil, fmt.Errorf("row import: decode columns: %w", err)
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.Name)
	}
	return out, nil
}

// postRow issues one POST /v1/tables/{id}/rows with the supplied value
// map. Returns nil on success, or a one-line error suitable for the
// import report. We extract the server's error message when present so
// per-row failures explain themselves.
func postRow(ctx context.Context, client *httpc.Client, tableID string, values map[string]string) error {
	body, err := json.Marshal(rowCreateRequest{Values: values})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	subCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := client.Do(subCtx, httpc.Request{
		Method: http.MethodPost,
		Path:   "/v1/tables/" + tableID + "/rows",
		Body:   body,
	})
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Surface the server-provided message when possible; that's what
	// most agents want in the per-row error report.
	var er struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &er); err == nil && er.Error.Message != "" {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, er.Error.Message)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

