package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- table create -------------------------------------------------

func TestTableCreate_PostsExpectedBody(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tables" {
			t.Errorf("got %s %s; want POST /v1/tables", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"tb_xyz","name":"Y","workbook_id":"wb_X","created_at":"2026-01-01T00:00:00Z"}`))
	})

	code, stdout, stderr := runCLI(t, "--config", cfgPath,
		"table", "create", "--workbook", "wb_X", "--name", "Y", "--json")
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}

	// Body shape: {name:"Y", workbook_id:"wb_X"} — both required.
	var sent map[string]any
	if err := json.Unmarshal(s.lastBody, &sent); err != nil {
		t.Fatalf("decode body: %v; raw=%q", err, string(s.lastBody))
	}
	if sent["name"] != "Y" {
		t.Errorf("body name = %v", sent["name"])
	}
	if sent["workbook_id"] != "wb_X" {
		t.Errorf("body workbook_id = %v", sent["workbook_id"])
	}

	// --json output: server JSON passed through.
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode stdout: %v; raw=%q", err, stdout.String())
	}
	if got["id"] != "tb_xyz" {
		t.Errorf("stdout id = %v", got["id"])
	}
}

func TestTableCreate_HumanLineShowsNameAndID(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"tb_xyz","name":"Y","workbook_id":"wb_X"}`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath,
		"table", "create", "--workbook", "wb_X", "--name", "Y")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), `Created table "Y"`) || !strings.Contains(stdout.String(), "tb_xyz") {
		t.Errorf("stdout missing expected line: %q", stdout.String())
	}
}

// --- table get ----------------------------------------------------

func TestTableGet_JSONExposesColumns(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tables/tb_X" {
			t.Errorf("path = %s; want /v1/tables/tb_X", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "tb_X",
			"name": "Leads",
			"workbook_id": "wb_X",
			"row_count": 100,
			"column_count": 3,
			"columns": [
				{"id":"col_a","name":"Email"},
				{"id":"col_b","name":"Domain"},
				{"id":"col_c","name":"Title"}
			]
		}`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "table", "get", "tb_X", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode stdout: %v; raw=%q", err, stdout.String())
	}
	cols, ok := got["columns"].([]any)
	if !ok {
		t.Fatalf("columns missing or wrong type: %v", got["columns"])
	}
	if len(cols) != 3 {
		t.Errorf("columns len = %d; want 3", len(cols))
	}
}

func TestTableGet_404ExitsWith3AndUserMessage(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"Table not found"}}`))
	})

	// 404 → exit 3 (NotFoundError) is the canonical mapping; this test
	// pins it for `table get` specifically.
	code, _, stderr := runCLI(t, "--config", cfgPath, "table", "get", "tb_missing")
	if code != 3 {
		t.Fatalf("exit = %d; want 3 on 404", code)
	}
	if !strings.Contains(stderr.String(), `"tb_missing" not found`) {
		t.Errorf("stderr missing 'not found' sentence: %q", stderr.String())
	}
}

// --- table list ---------------------------------------------------

func TestTableList_FilteredByWorkbookHitsScopedPath(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})

	code, _, _ := runCLI(t, "--config", cfgPath, "table", "list", "--workbook", "wb_X", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if s.lastReq.URL.Path != "/v1/workbooks/s/wb_X/tables" {
		t.Errorf("path = %s; want /v1/workbooks/s/wb_X/tables", s.lastReq.URL.Path)
	}
}

func TestTableList_UnscopedHitsOrgPath(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})

	code, _, _ := runCLI(t, "--config", cfgPath, "table", "list", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if s.lastReq.URL.Path != "/v1/tables" {
		t.Errorf("path = %s; want /v1/tables", s.lastReq.URL.Path)
	}
}

// --- table export -------------------------------------------------

func TestTableExport_CSVWithFileWritesPath(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	const csvBody = "email,domain\nfoo@x.com,x.com\n"
	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tables/tb_X/export.csv" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		_, _ = w.Write([]byte(csvBody))
	})

	outPath := filepath.Join(t.TempDir(), "x.csv")
	code, _, stderr := runCLI(t, "--config", cfgPath,
		"table", "export", "tb_X", "--format", "csv", "-o", outPath)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read %s: %v", outPath, err)
	}
	if string(got) != csvBody {
		t.Errorf("file content = %q; want %q", string(got), csvBody)
	}
}

func TestTableExport_CSVWithoutFileGoesToStdout(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	const csvBody = "email,domain\nfoo@x.com,x.com\n"
	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		_, _ = w.Write([]byte(csvBody))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath,
		"table", "export", "tb_X", "--format", "csv")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if stdout.String() != csvBody {
		t.Errorf("stdout = %q; want %q", stdout.String(), csvBody)
	}
}

func TestTableExport_JSONFormatHitsTableGet(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"tb_X","name":"Y","workbook_id":"wb_X"}`))
	})

	outPath := filepath.Join(t.TempDir(), "x.json")
	code, _, _ := runCLI(t, "--config", cfgPath,
		"table", "export", "tb_X", "--format", "json", "-o", outPath)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if s.lastReq.URL.Path != "/v1/tables/tb_X" {
		t.Errorf("path = %s; want /v1/tables/tb_X", s.lastReq.URL.Path)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read out file: %v", err)
	}
	if !strings.Contains(string(got), `"id":"tb_X"`) {
		t.Errorf("file content missing id: %q", string(got))
	}
}
