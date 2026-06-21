package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// --- run table ----------------------------------------------------

func TestRunTable_PostsExpectedPathAndPrintsHumanLine(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tables/tb_X/execute/table" {
			t.Errorf("got %s %s; want POST /v1/tables/tb_X/execute/table", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"job_X","table_id":"tb_X","status":"pending"}`))
	})
	_ = s

	code, stdout, stderr := runCLI(t, "--config", cfgPath, "run", "table", "tb_X")
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Started job job_X on table tb_X") {
		t.Errorf("stdout missing summary: %q", stdout.String())
	}
}

func TestRunTable_JSONPassesThroughServerResponse(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"job_X","table_id":"tb_X","status":"pending","extra":"keep"}`))
	})

	code, stdout, _ := runCLI(t, "--config", cfgPath, "run", "table", "tb_X", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode stdout: %v; raw=%q", err, stdout.String())
	}
	if got["id"] != "job_X" || got["extra"] != "keep" {
		t.Errorf("--json should pass server JSON through unchanged: %v", got)
	}
}

func TestRunTable_404_MapsToNotFoundExitCode(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	code, _, stderr := runCLI(t, "--config", cfgPath, "run", "table", "tb_ghost")
	if code != 3 {
		t.Fatalf("exit = %d; want 3; stderr=%q", code, stderr.String())
	}
}

func TestRunTable_WatchWaitMutuallyExclusive(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	// Unreachable API: if the mutual-exclusivity guard fires we never
	// hit the network so the test stays deterministic.
	t.Setenv(envAPIBase, "http://127.0.0.1:1")

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"run", "table", "tb_X", "--watch", "--wait",
	)
	if code != 1 {
		t.Fatalf("exit = %d; want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "watch") || !strings.Contains(stderr.String(), "wait") {
		t.Errorf("stderr should mention --watch and --wait: %q", stderr.String())
	}
}

// --- run cell -----------------------------------------------------

func TestRunCell_PostsExpectedBody(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tables/tb_X/execute/cell" {
			t.Errorf("got %s %s; want POST /v1/tables/tb_X/execute/cell", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"job_C","table_id":"tb_X","status":"pending"}`))
	})

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"run", "cell",
		"--table", "tb_X",
		"--row", "row_Y",
		"--column", "col_Z",
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}

	var sent struct {
		RowID    string `json:"row_id"`
		ColumnID string `json:"column_id"`
	}
	if err := json.Unmarshal(s.lastBody, &sent); err != nil {
		t.Fatalf("decode body: %v; raw=%q", err, string(s.lastBody))
	}
	if sent.RowID != "row_Y" || sent.ColumnID != "col_Z" {
		t.Errorf("body = %+v; want {row_Y, col_Z}", sent)
	}
}

// --- run table --wait (polling) ----------------------------------

func TestRunTable_Wait_TerminalSuccess(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	var jobCalls atomic.Int32

	s := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tables/tb_X/execute/table":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"job_X","table_id":"tb_X","status":"pending"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_X":
			n := jobCalls.Add(1)
			status := "success"
			if n < 2 {
				// First poll: still running, then succeed on the second
				// so we exercise the loop without hammering the test
				// with synthetic 5s sleeps.
				status = "running"
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"id":"job_X","table_id":"tb_X","status":%q,"done_cells":10,"total_cells":10}`,
				status,
			)))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	_ = s

	// 30s timeout is way more than the test needs but the polling
	// interval is 5s so we'd hit it on the second iteration anyway.
	// Use a short timeout to keep CI fast.
	code, _, stderr := runCLI(t, "--config", cfgPath,
		"run", "table", "tb_X", "--wait", "--timeout", "30s",
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Job job_X success") {
		t.Errorf("stderr should report terminal success: %q", stderr.String())
	}
}

func TestRunTable_Wait_TerminalFailure_ExitsNonZero(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"job_X","table_id":"tb_X","status":"pending"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_X":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"job_X","status":"failed","done_cells":4,"total_cells":10}`))
		}
	})

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"run", "table", "tb_X", "--wait", "--timeout", "30s",
	)
	if code != 1 {
		t.Fatalf("exit = %d; want 1 (failure); stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "failed") {
		t.Errorf("stderr should mention failure: %q", stderr.String())
	}
}

// --- run table --watch (SSE) -------------------------------------

// sseHandler builds an SSE-compliant http.HandlerFunc that emits the
// supplied events in order. Each event becomes one framed `event:` +
// `data:` block on the wire. When closeAfter > 0 the handler closes
// the FIRST connection after the Nth event so we can exercise the
// reconnect-with-Last-Event-ID path; the second connection (and later)
// always streams to completion. State is kept across connections via
// the captured counter so the helper can be embedded in a larger
// request multiplexer without losing reconnect semantics.
func sseHandler(t *testing.T, events []sseEvent, closeAfter int) http.HandlerFunc {
	t.Helper()
	var conns atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("response writer not a flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		thisConn := conns.Add(1)

		// Parse Last-Event-ID so we resume from the right place on
		// reconnect. Test sends 1-based sequence numbers — 0 means
		// "send from the start".
		startSeq := 0
		if h := r.Header.Get("Last-Event-ID"); h != "" {
			fmt.Sscanf(h, "%d", &startSeq)
		}

		for i, ev := range events {
			seq := i + 1
			if seq <= startSeq {
				continue
			}
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", seq, ev.name, ev.data)
			flusher.Flush()

			if thisConn == 1 && closeAfter > 0 && seq == closeAfter {
				// Force an abrupt TCP-level disconnect so r3labs treats
				// the drop as an error (and reconnects with
				// Last-Event-ID). A clean handler-return triggers EOF,
				// which r3labs interprets as "stream ended cleanly" and
				// does NOT retry — exactly what we DON'T want here.
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, _ := hj.Hijack()
					_ = conn.Close()
				}
				return
			}
		}

		// Hold the connection open briefly so the client treats normal
		// EOF as "server done" rather than retrying again. r3labs
		// reconnects on any disconnect — we rely on context timeout to
		// tear it down once the terminal event is processed.
		<-r.Context().Done()
	}
}

type sseEvent struct {
	name string
	data string
}

// stubAPI wires together the POST endpoint and the SSE endpoint behind
// one httptest.Server. Mirrors the dual-endpoint shape `run table
// --watch` exercises in production.
func stubAPI(t *testing.T, postPath string, postBody string, sseEvents []sseEvent, closeAfter int) *httptest.Server {
	t.Helper()
	streamHandler := sseHandler(t, sseEvents, closeAfter)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == postPath:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(postBody))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			streamHandler(w, r)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestRunTable_WatchJSON_EmitsJSONLinesAndExitsOnTerminal(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	events := []sseEvent{
		{"cell:update", `{"row_id":"row_Y","column_id":"col_Z","status":"success"}`},
		{"column:progress", `{"column_id":"col_Z","success":1,"queued":0,"running":0,"failed":0}`},
		{"job:update", `{"job_id":"job_X","status":"success","done_cells":1,"total_cells":1}`},
	}
	srv := stubAPI(t,
		"/v1/tables/tb_X/execute/table",
		`{"id":"job_X","table_id":"tb_X","status":"pending"}`,
		events, 0)
	defer srv.Close()
	t.Setenv(envAPIBase, srv.URL)

	code, stdout, stderr := runCLI(t, "--config", cfgPath,
		"run", "table", "tb_X", "--watch", "--json", "--timeout", "10s",
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d JSON-lines; want 3; stdout=%q", len(lines), stdout.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode first line: %v; raw=%q", err, lines[0])
	}
	if first["event"] != "cell:update" {
		t.Errorf("first event field = %v; want cell:update", first["event"])
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("decode last line: %v; raw=%q", err, lines[len(lines)-1])
	}
	if last["event"] != "job:update" || last["status"] != "success" {
		t.Errorf("last event = %v; want job:update success", last)
	}
}

func TestRunTable_WatchJSON_TerminalFailure_ExitsNonZero(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	events := []sseEvent{
		{"job:update", `{"job_id":"job_X","status":"failed","done_cells":2,"total_cells":10}`},
	}
	srv := stubAPI(t,
		"/v1/tables/tb_X/execute/table",
		`{"id":"job_X","table_id":"tb_X","status":"pending"}`,
		events, 0)
	defer srv.Close()
	t.Setenv(envAPIBase, srv.URL)

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"run", "table", "tb_X", "--watch", "--json", "--timeout", "10s",
	)
	if code != 1 {
		t.Fatalf("exit = %d; want 1; stderr=%q", code, stderr.String())
	}
}

func TestRunTable_WatchJSON_FiltersOtherJobsEvents(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	events := []sseEvent{
		// Unrelated job — must be filtered out of stdout.
		{"job:update", `{"job_id":"job_OTHER","status":"running","done_cells":1,"total_cells":2}`},
		{"cell:update", `{"row_id":"row_Y","column_id":"col_Z","status":"success"}`},
		{"job:update", `{"job_id":"job_X","status":"success","done_cells":1,"total_cells":1}`},
	}
	srv := stubAPI(t,
		"/v1/tables/tb_X/execute/table",
		`{"id":"job_X","table_id":"tb_X","status":"pending"}`,
		events, 0)
	defer srv.Close()
	t.Setenv(envAPIBase, srv.URL)

	code, stdout, _ := runCLI(t, "--config", cfgPath,
		"run", "table", "tb_X", "--watch", "--json", "--timeout", "10s",
	)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(stdout.String(), "job_OTHER") {
		t.Errorf("stdout should NOT contain other-job event: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "job_X") {
		t.Errorf("stdout should contain target-job terminal event: %q", stdout.String())
	}
}
