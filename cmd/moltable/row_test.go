package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- row create --------------------------------------------------

func TestRowCreate_PostsExpectedBody(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tables/tb_X/rows" {
			t.Errorf("got %s %s; want POST /v1/tables/tb_X/rows", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"row_1","values":{}}`))
	})

	code, stdout, stderr := runCLI(t, "--config", cfgPath,
		"row", "create",
		"--table", "tb_X",
		"--data", `{"Name":"Au Cheval"}`,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}

	var sent struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(s.lastBody, &sent); err != nil {
		t.Fatalf("decode body: %v; raw=%q", err, string(s.lastBody))
	}
	if sent.Values["Name"] != "Au Cheval" {
		t.Errorf("body.values.Name = %q", sent.Values["Name"])
	}
	if !strings.Contains(stdout.String(), "Created row row_1") {
		t.Errorf("stdout missing human line: %q", stdout.String())
	}
}

func TestRowCreate_InvalidJSON_FailsBeforeHTTP(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	// Unreachable API: if the network is touched, the error wouldn't
	// mention "Invalid JSON" but a dial failure.
	t.Setenv(envAPIBase, "http://127.0.0.1:1")

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"row", "create",
		"--table", "tb_X",
		"--data", `{invalid json`,
	)
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "Invalid JSON") {
		t.Errorf("stderr missing sentence: %q", stderr.String())
	}
}

func TestRowCreate_JSONPassesThroughServerResponse(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"row_1","values":{"Name":"x"},"extra":"keep"}`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath,
		"row", "create",
		"--table", "tb_X",
		"--data", `{"Name":"x"}`,
		"--json",
	)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode stdout: %v", err)
	}
	if got["id"] != "row_1" || got["extra"] != "keep" {
		t.Errorf("--json should pass server JSON through unchanged: %v", got)
	}
}

// --- row import --------------------------------------------------

// importStub serves GET /columns and POST /rows. Tracks the rows it
// receives so tests can assert on per-row payloads.
type importStub struct {
	mu          sync.Mutex
	columnNames []string
	rows        []map[string]string
	rowResponses []func(w http.ResponseWriter) // optional per-row overrides
}

func (s *importStub) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/columns"):
			s.mu.Lock()
			names := s.columnNames
			s.mu.Unlock()
			cols := make([]map[string]string, 0, len(names))
			for i, n := range names {
				cols = append(cols, map[string]string{
					"id":          "col_" + n,
					"name":        n,
					"source_type": "input",
				})
				_ = i
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cols)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rows"):
			var req struct {
				Values map[string]string `json:"values"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			s.mu.Lock()
			s.rows = append(s.rows, req.Values)
			idx := len(s.rows) - 1
			var override func(w http.ResponseWriter)
			if idx < len(s.rowResponses) && s.rowResponses[idx] != nil {
				override = s.rowResponses[idx]
			}
			s.mu.Unlock()
			if override != nil {
				override(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"row_x"}`))

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func writeCSV(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.csv")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	return p
}

func TestRowImport_HappyPath_PostsEachRowAndReportsJSON(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	stub := &importStub{columnNames: []string{"Name", "Cuisine"}}
	_ = newStubServer(t, stub.handler(t))

	csvPath := writeCSV(t, "Name,Cuisine\nAu Cheval,Burgers\nGirl & the Goat,American\n")

	code, stdout, stderr := runCLI(t, "--config", cfgPath,
		"row", "import",
		"--table", "tb_X",
		"--csv", csvPath,
		"--json",
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}

	if len(stub.rows) != 2 {
		t.Fatalf("rows posted = %d; want 2", len(stub.rows))
	}
	if stub.rows[0]["Name"] != "Au Cheval" || stub.rows[0]["Cuisine"] != "Burgers" {
		t.Errorf("row 0 = %v", stub.rows[0])
	}

	var rep importReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("decode report: %v; raw=%q", err, stdout.String())
	}
	if rep.Imported != 2 || rep.Skipped != 0 {
		t.Errorf("report = %+v", rep)
	}
}

func TestRowImport_MismatchedHeader_ListsExtraColumns(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	stub := &importStub{columnNames: []string{"Name", "Cuisine"}}
	_ = newStubServer(t, stub.handler(t))

	// CSV has an extra Phone column not on the table.
	csvPath := writeCSV(t, "Name,Cuisine,Phone\nAu Cheval,Burgers,555\n")

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"row", "import",
		"--table", "tb_X",
		"--csv", csvPath,
		"--json",
	)
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "Phone") {
		t.Errorf("stderr should mention extra column 'Phone': %q", stderr.String())
	}
}

func TestRowImport_NoMatchingColumns_Error(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	stub := &importStub{columnNames: []string{"Name", "Cuisine"}}
	_ = newStubServer(t, stub.handler(t))

	// CSV header doesn't overlap with table columns at all.
	csvPath := writeCSV(t, "foo,bar\n1,2\n")

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"row", "import",
		"--table", "tb_X",
		"--csv", csvPath,
		"--json",
	)
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "does not match") {
		t.Errorf("stderr missing 'does not match': %q", stderr.String())
	}
}

func TestRowImport_ColumnMapping_RenamesCSVColumn(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	stub := &importStub{columnNames: []string{"Name", "Cuisine"}}
	_ = newStubServer(t, stub.handler(t))

	csvPath := writeCSV(t, "restaurant,style\nAu Cheval,Burgers\n")

	code, stdout, stderr := runCLI(t, "--config", cfgPath,
		"row", "import",
		"--table", "tb_X",
		"--csv", csvPath,
		"--column-mapping", "Name=restaurant",
		"--column-mapping", "Cuisine=style",
		"--json",
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	if len(stub.rows) != 1 {
		t.Fatalf("posted rows = %d", len(stub.rows))
	}
	if stub.rows[0]["Name"] != "Au Cheval" || stub.rows[0]["Cuisine"] != "Burgers" {
		t.Errorf("mapped row = %v", stub.rows[0])
	}

	var rep importReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if rep.Imported != 1 {
		t.Errorf("imported = %d; want 1", rep.Imported)
	}
}

func TestRowImport_PerRowFailureCounted(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	stub := &importStub{
		columnNames: []string{"Name"},
		rowResponses: []func(w http.ResponseWriter){
			nil, // row 0 succeeds with default
			func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"bad"}}`))
			},
		},
	}
	_ = newStubServer(t, stub.handler(t))

	csvPath := writeCSV(t, "Name\nAlice\nBob\n")

	code, stdout, _ := runCLI(t, "--config", cfgPath,
		"row", "import",
		"--table", "tb_X",
		"--csv", csvPath,
		"--json",
	)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	var rep importReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Imported != 1 || rep.Skipped != 1 {
		t.Errorf("report = %+v", rep)
	}
	if len(rep.Errors) != 1 || rep.Errors[0].Row != 2 {
		t.Errorf("errors = %+v", rep.Errors)
	}
}

func TestRowImport_BadMapping_ReportsField(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	stub := &importStub{columnNames: []string{"Name"}}
	_ = newStubServer(t, stub.handler(t))

	csvPath := writeCSV(t, "Name\nAlice\n")

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"row", "import",
		"--table", "tb_X",
		"--csv", csvPath,
		"--column-mapping", "NotARealCol=Name",
		"--json",
	)
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown table columns") {
		t.Errorf("stderr missing sentence: %q", stderr.String())
	}
}

// --- error mapping ------------------------------------------------

func TestRowCreate_API404ExitsWith3(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"Table not found"}}`))
	})

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"row", "create",
		"--table", "tb_missing",
		"--data", `{"Name":"x"}`,
	)
	if code != 3 {
		t.Fatalf("exit = %d; want 3", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr missing 'not found': %q", stderr.String())
	}
}
