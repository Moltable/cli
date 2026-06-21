package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// --- moltable stop ------------------------------------------------

func TestStop_PostsExpectedPath(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tables/tb_X/stop" {
			t.Errorf("got %s %s; want POST /v1/tables/tb_X/stop", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"stopped"}`))
	})
	_ = s

	code, stdout, stderr := runCLI(t, "--config", cfgPath, "stop", "tb_X")
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Stopped active jobs on table tb_X") {
		t.Errorf("stdout missing human summary: %q", stdout.String())
	}
}

func TestStop_JSONPassesThroughServerEnvelope(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"stopped","stopped":3}`))
	})

	code, stdout, stderr := runCLI(t, "--config", cfgPath, "stop", "tb_X", "--json")
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode stdout: %v; raw=%q", err, stdout.String())
	}
	if got["status"] != "stopped" {
		t.Errorf("--json should pass status through: %v", got)
	}
	if got["stopped"] != float64(3) {
		t.Errorf("--json should pass extra fields through: %v", got)
	}
}

func TestStop_404_MapsToNotFoundExitCode(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"Table not found"}}`))
	})

	code, _, stderr := runCLI(t, "--config", cfgPath, "stop", "tb_ghost")
	if code != 3 {
		t.Fatalf("exit = %d; want 3 (not found); stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Table") {
		t.Errorf("stderr should name the missing kind: %q", stderr.String())
	}
}

func TestStop_401_MapsToAuthExitCode(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	code, _, stderr := runCLI(t, "--config", cfgPath, "stop", "tb_X")
	if code != 2 {
		t.Fatalf("exit = %d; want 2 (auth); stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Authentication") {
		t.Errorf("stderr should mention authentication: %q", stderr.String())
	}
}
