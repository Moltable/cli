package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// --- view list ----------------------------------------------------

func TestViewList_HitsRightPath(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tables/tb_X/views" {
			t.Errorf("got %s %s; want GET /v1/tables/tb_X/views", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"vw_default","name":"All rows","is_default":true,"position":0,"created_at":"2026-01-01T00:00:00Z"},
			{"id":"vw_filt","name":"Pending","is_default":false,"position":1,"created_at":"2026-01-02T00:00:00Z"}
		]`))
	})
	_ = s

	code, stdout, stderr := runCLI(t, "--config", cfgPath, "view", "list", "--table", "tb_X")
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	// Human output: header + both rows + default marker on the right one.
	if !strings.Contains(out, "vw_default") || !strings.Contains(out, "vw_filt") {
		t.Errorf("stdout missing rows: %q", out)
	}
	if !strings.Contains(out, "yes") {
		t.Errorf("default marker not rendered: %q", out)
	}
}

func TestViewList_JSONPassesThrough(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"vw_X","name":"v","is_default":true,"position":0}]`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "view", "list", "--table", "tb_X", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode stdout: %v; raw=%q", err, stdout.String())
	}
	if len(got) != 1 || got[0]["id"] != "vw_X" {
		t.Errorf("stdout shape unexpected: %+v", got)
	}
}

func TestViewList_EmptyShowsHint(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "view", "list", "--table", "tb_X")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "No views") {
		t.Errorf("stdout missing empty-state message: %q", stdout.String())
	}
}

// --- view get -----------------------------------------------------

func TestViewGet_HitsRightPath(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tables/tb_X/views/vw_Y" {
			t.Errorf("path = %s; want /v1/tables/tb_X/views/vw_Y", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vw_Y","name":"Pending","is_default":false,"position":1}`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "view", "get", "--table", "tb_X", "vw_Y")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "vw_Y") || !strings.Contains(stdout.String(), "Pending") {
		t.Errorf("stdout missing fields: %q", stdout.String())
	}
}

func TestViewGet_404ExitsWith3(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"View not found"}}`))
	})

	code, _, _ := runCLI(t, "--config", cfgPath, "view", "get", "--table", "tb_X", "vw_missing")
	if code != 3 {
		t.Fatalf("exit = %d; want 3 on 404", code)
	}
}

// --- view search --------------------------------------------------

func TestViewSearch_HitsRightPathWithQuery(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tables/tb_X/views/vw_Y/search" {
			t.Errorf("got %s %s; want GET /v1/tables/tb_X/views/vw_Y/search", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("q") != "linkedin" {
			t.Errorf("q = %q; want linkedin", r.URL.Query().Get("q"))
		}
		// --limit not passed → URL shouldn't carry it.
		if r.URL.Query().Has("limit") {
			t.Errorf("limit unexpectedly present: %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"query": "linkedin",
			"view_id": "vw_Y",
			"match_count": 3,
			"truncated": false,
			"row_set_truncated": false,
			"matches": [
				{"row_id":"row_a","position":0,"matching_column_ids":["col_1","col_2"]},
				{"row_id":"row_b","position":0,"matching_column_ids":["col_1"]}
			]
		}`))
	})
	_ = s

	code, stdout, stderr := runCLI(t, "--config", cfgPath,
		"view", "search", "--table", "tb_X", "--view", "vw_Y", "linkedin")
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	// Summary line uses cell count and row count both.
	if !strings.Contains(out, "3 matched cells") || !strings.Contains(out, "across 2 rows") {
		t.Errorf("summary line wrong: %q", out)
	}
	if !strings.Contains(out, "row_a") || !strings.Contains(out, "col_1,col_2") {
		t.Errorf("rows not rendered: %q", out)
	}
}

func TestViewSearch_TruncatedRendersPlusSuffix(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"query":"x","view_id":"vw_Y",
			"match_count":5000,"truncated":true,"row_set_truncated":false,
			"matches":[{"row_id":"row_a","position":0,"matching_column_ids":["col_1"]}]
		}`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath,
		"view", "search", "--table", "tb_X", "--view", "vw_Y", "x")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "5000") || !strings.Contains(stdout.String(), "cap reached") {
		t.Errorf("truncation suffix missing: %q", stdout.String())
	}
}

func TestViewSearch_RowSetTruncatedRendersHintWhenZeroMatches(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"query":"needle","view_id":"vw_Y",
			"match_count":0,"truncated":false,"row_set_truncated":true,
			"matches":[]
		}`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath,
		"view", "search", "--table", "tb_X", "--view", "vw_Y", "needle")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Narrow") && !strings.Contains(out, "narrow") {
		t.Errorf("row-set-truncated hint missing: %q", out)
	}
	if !strings.Contains(out, "5000") {
		t.Errorf("row-set cap not mentioned in hint: %q", out)
	}
}

func TestViewSearch_EmptyQueryFailsLocally(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	// Stub server must NOT be hit — empty-query guard fails fast.
	hit := false
	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	})

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"view", "search", "--table", "tb_X", "--view", "vw_Y", "   ")
	if code == 0 {
		t.Fatalf("expected non-zero exit on empty query; got 0")
	}
	if hit {
		t.Errorf("server was hit; empty-query guard didn't fire")
	}
	if !strings.Contains(stderr.String(), "query") {
		t.Errorf("stderr should mention query: %q", stderr.String())
	}
}

func TestViewSearch_LimitParamPassedThrough(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "25" {
			t.Errorf("limit = %q; want 25", r.URL.Query().Get("limit"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"x","view_id":"vw_Y","match_count":0,"truncated":false,"row_set_truncated":false,"matches":[]}`))
	})

	code, _, _ := runCLI(t, "--config", cfgPath,
		"view", "search", "--table", "tb_X", "--view", "vw_Y", "--limit", "25", "x")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
}

func TestViewSearch_JSONPassesThrough(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"query":"x","view_id":"vw_Y",
			"match_count":1,"truncated":false,"row_set_truncated":false,
			"matches":[{"row_id":"row_a","position":0,"matching_column_ids":["col_1"]}]
		}`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath,
		"view", "search", "--table", "tb_X", "--view", "vw_Y", "--json", "x")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode stdout: %v; raw=%q", err, stdout.String())
	}
	if got["match_count"].(float64) != 1 {
		t.Errorf("match_count wrong: %v", got["match_count"])
	}
}

func TestViewSearch_429MapsToRateLimit(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"RATE_LIMITED","message":"rate limit"}}`))
	})

	// 429 → RateLimitError. Exit code 4 per internal/errors.ExitCode.
	code, _, _ := runCLI(t, "--config", cfgPath,
		"view", "search", "--table", "tb_X", "--view", "vw_Y", "x")
	if code == 0 {
		t.Fatalf("expected non-zero exit on 429; got 0")
	}
}
