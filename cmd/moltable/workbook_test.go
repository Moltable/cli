package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moltable/cli/internal/config"
)

// seedDefaultProfile drops a one-profile config at path so the auth
// resolver picks it up as the default. Used by every cmd test that
// exercises the wire layer; matches the pattern auth_test.go uses.
func seedDefaultProfile(t *testing.T, path string) {
	t.Helper()
	cfg := &config.Config{
		DefaultProfile: "default",
		Profiles: map[string]config.Profile{
			"default": {APIKey: "molt_test_key", Created: time.Now().UTC()},
		},
	}
	if err := config.SaveTo(path, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

// stubServer captures the last request seen + serves the supplied
// handler so individual tests can assert path/body/headers without
// re-wiring httptest each time.
type stubServer struct {
	srv     *httptest.Server
	lastReq *http.Request
	lastBody []byte
}

func newStubServer(t *testing.T, handler http.HandlerFunc) *stubServer {
	t.Helper()
	s := &stubServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.lastReq = r
		s.lastBody = body
		// Restore the body for downstream handlers that re-read it.
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		handler(w, r)
	}))
	t.Setenv(envAPIBase, s.srv.URL)
	t.Cleanup(s.srv.Close)
	return s
}

// --- workbook create ---------------------------------------------

func TestWorkbookCreate_PostsBodyAndPrintsHumanLine(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/workbooks" {
			t.Errorf("got %s %s; want POST /v1/workbooks", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         "wb_abc",
			"name":       "Test",
			"created_at": time.Now().UTC().Format(time.RFC3339),
		})
	})
	_ = s

	code, stdout, stderr := runCLI(t, "--config", cfgPath, "workbook", "create", "Test")
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}

	// Body shape: {"name":"Test"}
	var sent map[string]any
	if err := json.Unmarshal(s.lastBody, &sent); err != nil {
		t.Fatalf("decode request body: %v; raw=%q", err, string(s.lastBody))
	}
	if sent["name"] != "Test" {
		t.Errorf("body name = %v; want %q", sent["name"], "Test")
	}

	out := stdout.String()
	if !strings.Contains(out, "Created workbook") {
		t.Errorf("stdout missing 'Created workbook': %q", out)
	}
	if !strings.Contains(out, "Test") || !strings.Contains(out, "wb_abc") {
		t.Errorf("stdout missing name/id: %q", out)
	}
}

func TestWorkbookCreate_JSONPassesThroughServerResponse(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"wb_abc","name":"Test","extra":"keep_me"}`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "workbook", "create", "Test", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode stdout JSON: %v; raw=%q", err, stdout.String())
	}
	if got["id"] != "wb_abc" || got["name"] != "Test" {
		t.Errorf("missing id/name in JSON: %v", got)
	}
	if got["extra"] != "keep_me" {
		t.Errorf("--json should pass server fields through unchanged: %v", got)
	}
}

// --- workbook list -----------------------------------------------

func TestWorkbookList_JSONReturnsArrayWithExpectedLength(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/workbooks" {
			t.Errorf("got %s %s; want GET /v1/workbooks", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"id":"wb_a","name":"Alpha","created_at":"2026-01-01T00:00:00Z"},
			{"id":"wb_b","name":"Bravo","created_at":"2026-01-02T00:00:00Z"},
			{"id":"wb_c","name":"Charlie","created_at":"2026-01-03T00:00:00Z"}
		]`))
	})

	code, stdout, stderr := runCLI(t, "--config", cfgPath, "workbook", "list", "--json")
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	var arr []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &arr); err != nil {
		t.Fatalf("decode JSON: %v; raw=%q", err, stdout.String())
	}
	if len(arr) != 3 {
		t.Fatalf("len = %d; want 3", len(arr))
	}
}

func TestWorkbookList_TTYRendersTable(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"wb_a","name":"Alpha","created_at":"2026-01-01T00:00:00Z"}]`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "workbook", "list")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "ID") || !strings.Contains(out, "NAME") {
		t.Errorf("missing table header: %q", out)
	}
	if !strings.Contains(out, "wb_a") || !strings.Contains(out, "Alpha") {
		t.Errorf("missing row data: %q", out)
	}
}

func TestWorkbookList_EmptyArrayRendersHint(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "workbook", "list")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "No workbooks") {
		t.Errorf("stdout missing 'No workbooks': %q", stdout.String())
	}
}

// --- auth + error mapping (workbook surface) ---------------------

func TestWorkbookCreate_NoAuthExits2(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	// No seed → no profile, no env.
	t.Setenv("MOLTABLE_API_KEY", "")
	t.Setenv("MOLTABLE_PROFILE", "")

	code, _, stderr := runCLI(t, "--config", cfgPath, "workbook", "create", "X")
	if code != 2 {
		t.Fatalf("exit = %d; want 2 for no-auth", code)
	}
	if !strings.Contains(stderr.String(), "moltable auth login") {
		t.Errorf("stderr missing auth-login hint: %q", stderr.String())
	}
}

func TestWorkbookCreate_API401ExitsWith2AndHint(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"bad key"}}`))
	})

	code, _, stderr := runCLI(t, "--config", cfgPath, "workbook", "create", "X")
	if code != 2 {
		t.Fatalf("exit = %d; want 2 on 401", code)
	}
	if !strings.Contains(stderr.String(), "moltable auth login") {
		t.Errorf("stderr missing auth hint: %q", stderr.String())
	}
}
