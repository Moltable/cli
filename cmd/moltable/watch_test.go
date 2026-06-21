package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// jobLookupStubAPI wires together GET /v1/jobs/{id} (returns the
// supplied table ID) and the SSE stream so the watch commands can do
// the "resolve job → open table stream" dance against one test server.
func jobLookupStubAPI(t *testing.T, jobID, tableID string, sseEvents []sseEvent) *httptest.Server {
	t.Helper()
	streamHandler := sseHandler(t, sseEvents, 0)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/"+jobID:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"id":%q,"table_id":%q,"status":"running","done_cells":0,"total_cells":10}`,
				jobID, tableID)))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tables/"+tableID+"/stream":
			streamHandler(w, r)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// --- moltable run watch -------------------------------------------

func TestRunWatch_LooksUpJobAndFiltersToTarget(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	events := []sseEvent{
		{"job:update", `{"job_id":"job_OTHER","status":"running","done_cells":1,"total_cells":2}`},
		{"job:update", `{"job_id":"job_X","status":"success","done_cells":2,"total_cells":2}`},
	}
	srv := jobLookupStubAPI(t, "job_X", "tb_X", events)
	defer srv.Close()
	t.Setenv(envAPIBase, srv.URL)

	code, stdout, stderr := runCLI(t, "--config", cfgPath,
		"run", "watch", "job_X", "--json", "--timeout", "10s",
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "job_OTHER") {
		t.Errorf("stdout should filter out other job's job:update: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "job_X") {
		t.Errorf("stdout should contain target job event: %q", stdout.String())
	}
}

// --- moltable watch (top-level alias) -----------------------------

func TestWatch_TopLevelAlias_WorksLikeRunWatch(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	events := []sseEvent{
		{"job:update", `{"job_id":"job_X","status":"success","done_cells":1,"total_cells":1}`},
	}
	srv := jobLookupStubAPI(t, "job_X", "tb_X", events)
	defer srv.Close()
	t.Setenv(envAPIBase, srv.URL)

	code, stdout, stderr := runCLI(t, "--config", cfgPath,
		"watch", "job_X", "--json", "--timeout", "10s",
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	var got map[string]any
	line := strings.TrimSpace(stdout.String())
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode stdout: %v; raw=%q", err, line)
	}
	if got["event"] != "job:update" || got["status"] != "success" {
		t.Errorf("emitted event = %v; want job:update success", got)
	}
}

func TestRunWatch_404_OnJobLookup_MapsToNotFoundExit(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	_ = newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	code, _, stderr := runCLI(t, "--config", cfgPath,
		"run", "watch", "job_GHOST", "--json", "--timeout", "5s",
	)
	if code != 3 {
		t.Fatalf("exit = %d; want 3; stderr=%q", code, stderr.String())
	}
}

// --- reconnect dance ---------------------------------------------

func TestRunTable_Watch_SurvivesMidStreamDisconnect(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	seedDefaultProfile(t, cfgPath)

	// Three events; close after the first → the r3labs library
	// reconnects with Last-Event-ID=1, the test handler resumes at
	// seq 2. The final terminal job:update still arrives.
	events := []sseEvent{
		{"cell:update", `{"row_id":"row_Y","column_id":"col_Z","status":"queued"}`},
		{"cell:update", `{"row_id":"row_Y","column_id":"col_Z","status":"success"}`},
		{"job:update", `{"job_id":"job_X","status":"success","done_cells":1,"total_cells":1}`},
	}
	var streamHits, postCalls atomic.Int32
	// Build the SSE handler ONCE so its internal conns counter persists
	// across the initial connect + the post-disconnect reconnect.
	streamH := sseHandler(t, events, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tables/tb_X/execute/table":
			postCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"job_X","table_id":"tb_X","status":"pending"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tables/tb_X/stream":
			streamHits.Add(1)
			streamH(w, r)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv(envAPIBase, srv.URL)

	code, stdout, stderr := runCLI(t, "--config", cfgPath,
		"run", "table", "tb_X", "--watch", "--json", "--timeout", "20s",
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	// Must have received the terminal job:update (so the reconnect
	// dance recovered the trailing two events).
	if !strings.Contains(stdout.String(), `"status":"success"`) {
		t.Errorf("stdout missing terminal success event after reconnect: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"event":"job:update"`) {
		t.Errorf("stdout missing job:update event: %q", stdout.String())
	}
}

// --- sseChannelClosedError -----------------------------------------
//
// Regression coverage for the P1 fix: the SSE events channel closing
// mid-watch (because r3labs/sse exhausted its reconnect strategy) used
// to silently return nil and look like the job had succeeded. It must
// now surface as an error UNLESS ctx is already done (timeout / ctrl-C).

func TestSSEChannelClosedError_ReturnsErrorWhenCtxAlive(t *testing.T) {
	// ctx that is NOT done — simulates "library gave up reconnecting
	// before the user's --timeout fired".
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	err := sseChannelClosedError(ctx)
	if err == nil {
		t.Fatalf("expected non-nil error when ctx is alive; got nil (the original silent-success bug)")
	}
	if !strings.Contains(err.Error(), "reconnects exhausted") {
		t.Errorf("error message should explain why; got %q", err.Error())
	}
}

func TestSSEChannelClosedError_ReturnsCtxErrWhenCtxDone(t *testing.T) {
	// ctx is already done — the close is a side-effect of the user
	// hitting --timeout (or ctrl-C), so we should echo ctx.Err() and
	// let the ctx.Done branch's typed timeout error take over upstream.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sseChannelClosedError(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled when ctx is done; got %v", err)
	}
}

func TestSSEChannelClosedError_DeadlineExceededPropagates(t *testing.T) {
	// Concrete deadline-exceeded case: confirm we don't mask the
	// deadline error with the generic reconnect-exhausted message.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := sseChannelClosedError(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded; got %v", err)
	}
}
