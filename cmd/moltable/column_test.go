package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withStdinReader temporarily swaps the package-level stdinReader. The
// real CLI binds it to os.Stdin; tests need a string-backed reader so
// they can drive `--config-stdin` deterministically.
func withStdinReader(t *testing.T, body string) {
	t.Helper()
	prev := stdinReader
	stdinReader = strings.NewReader(body)
	t.Cleanup(func() { stdinReader = prev })
}

// --- column add ---------------------------------------------------

func TestColumnAdd_StdinPostsExpectedBody(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tables/tb_X/columns" {
			t.Errorf("got %s %s; want POST /v1/tables/tb_X/columns", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"col_1","name":"Cuisine","source_type":"moltygent"}`))
	})

	withStdinReader(t, `{"prompt":"hi","temperature":0.2}`)

	code, stdout, stderr := runCLI(t, "--config", cfgPath,
		"column", "add",
		"--table", "tb_X",
		"--name", "Cuisine",
		"--source", "moltygent",
		"--config-stdin",
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}

	var sent map[string]any
	if err := json.Unmarshal(s.lastBody, &sent); err != nil {
		t.Fatalf("decode body: %v; raw=%q", err, string(s.lastBody))
	}
	if sent["name"] != "Cuisine" {
		t.Errorf("body name = %v", sent["name"])
	}
	if sent["source_type"] != "moltygent" {
		t.Errorf("body source_type = %v", sent["source_type"])
	}
	sc, ok := sent["source_config"].(map[string]any)
	if !ok {
		t.Fatalf("source_config missing or wrong shape: %v", sent["source_config"])
	}
	if sc["prompt"] != "hi" {
		t.Errorf("source_config.prompt = %v", sc["prompt"])
	}

	if !strings.Contains(stdout.String(), `Created column "Cuisine"`) ||
		!strings.Contains(stdout.String(), "col_1") {
		t.Errorf("stdout missing expected line: %q", stdout.String())
	}
}

func TestColumnAdd_InvalidSourceType_ClientSideError(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	// Stdin shouldn't even be touched, but provide a valid JSON just so
	// the flow doesn't depend on order of validation.
	withStdinReader(t, `{}`)

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"column", "add",
		"--table", "tb_X",
		"--name", "Bad",
		"--source", "imaginary",
		"--config-stdin",
	)
	if code != 1 {
		t.Fatalf("exit = %d; want 1 for invalid input", code)
	}
	if !strings.Contains(stderr.String(), "Source must be one of") {
		t.Errorf("stderr missing sentence: %q", stderr.String())
	}
}

func TestColumnAdd_StdinMalformedJSON_FailsBeforeHTTP(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	// No stub server: if we reach the HTTP layer the test should fail
	// with a connection error, not the JSON sentence.
	t.Setenv(envAPIBase, "http://127.0.0.1:1") // deliberately unroutable

	withStdinReader(t, `{ this is not json`)

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"column", "add",
		"--table", "tb_X",
		"--name", "Foo",
		"--source", "moltygent",
		"--config-stdin",
	)
	if code != 1 {
		t.Fatalf("exit = %d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "Invalid JSON in stdin") {
		t.Errorf("stderr missing 'Invalid JSON in stdin' sentence: %q", stderr.String())
	}
}

func TestColumnAdd_MultipleConfigFlags_Error(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	// Pre-create a file for --config-file.
	cfgFile := filepath.Join(t.TempDir(), "src.json")
	if err := os.WriteFile(cfgFile, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}

	withStdinReader(t, `{}`)

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"column", "add",
		"--table", "tb_X",
		"--name", "Foo",
		"--source", "moltygent",
		"--config-stdin",
		"--config-file", cfgFile,
	)
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "Provide only one of") {
		t.Errorf("stderr missing 'Provide only one of': %q", stderr.String())
	}
}

func TestColumnAdd_NoConfigFlag_Error(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"column", "add",
		"--table", "tb_X",
		"--name", "Foo",
		"--source", "moltygent",
	)
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "--config-stdin") {
		t.Errorf("stderr missing flag hint: %q", stderr.String())
	}
}

func TestColumnAdd_ConfigArg_PostsExpectedBody(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"col_2","name":"Email","source_type":"input"}`))
	})

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"column", "add",
		"--table", "tb_X",
		"--name", "Email",
		"--source", "input",
		"--config-arg", `{"col_type":"text"}`,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(s.lastBody, &sent); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if sent["name"] != "Email" || sent["source_type"] != "input" {
		t.Errorf("body fields = %v", sent)
	}
}

func TestColumnAdd_ConfigFile_PostsExpectedBody(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	cfgFile := filepath.Join(t.TempDir(), "src.json")
	if err := os.WriteFile(cfgFile, []byte(`{"prompt":"from file"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"col_3","name":"X","source_type":"moltygent"}`))
	})

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"column", "add",
		"--table", "tb_X",
		"--name", "X",
		"--source", "moltygent",
		"--config-file", cfgFile,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(s.lastBody, &sent); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	sc, _ := sent["source_config"].(map[string]any)
	if sc["prompt"] != "from file" {
		t.Errorf("source_config.prompt = %v", sc["prompt"])
	}
}

// --- column list --------------------------------------------------

func TestColumnList_JSONReturnsArray(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tables/tb_X/columns" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"col_a","name":"Name","source_type":"input"},
			{"id":"col_b","name":"Domain","source_type":"http"}
		]`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath,
		"column", "list", "--table", "tb_X", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var arr []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &arr); err != nil {
		t.Fatalf("decode stdout: %v; raw=%q", err, stdout.String())
	}
	if len(arr) != 2 {
		t.Errorf("len = %d; want 2", len(arr))
	}
}

func TestColumnList_TTYRendersTable(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"col_a","name":"Email","source_type":"input"}]`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath,
		"column", "list", "--table", "tb_X")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"ID", "NAME", "SOURCE", "col_a", "Email", "input"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in stdout: %q", want, out)
		}
	}
}

func TestColumnList_EmptyArrayRendersHint(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath,
		"column", "list", "--table", "tb_X")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "No columns") {
		t.Errorf("stdout missing 'No columns': %q", stdout.String())
	}
}

// --- error mapping ------------------------------------------------

func TestColumnAdd_API401ExitsWith2(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"bad key"}}`))
	})

	withStdinReader(t, `{}`)

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"column", "add",
		"--table", "tb_X",
		"--name", "Foo",
		"--source", "moltygent",
		"--config-stdin",
	)
	if code != 2 {
		t.Fatalf("exit = %d; want 2 on 401", code)
	}
	if !strings.Contains(stderr.String(), "moltable auth login") {
		t.Errorf("stderr missing auth hint: %q", stderr.String())
	}
}

func TestColumnList_API404ExitsWith3(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"Table not found"}}`))
	})

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"column", "list", "--table", "tb_missing")
	if code != 3 {
		t.Fatalf("exit = %d; want 3 on 404", code)
	}
	if !strings.Contains(stderr.String(), `"tb_missing" not found`) {
		t.Errorf("stderr missing sentence: %q", stderr.String())
	}
}
