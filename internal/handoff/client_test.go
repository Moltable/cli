package handoff

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	molterrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/httpc"
)

// newTestClient wires a Client against srv with deterministic state
// and no real sleeping. browserCalls captures every URL passed to
// OpenBrowser so tests can assert on it.
func newTestClient(t *testing.T, srv *httptest.Server) (*Client, *[]string) {
	t.Helper()
	hc, err := httpc.New(srv.URL, "molt_unused", "moltable-cli/test")
	if err != nil {
		t.Fatalf("httpc.New: %v", err)
	}
	hc.Sleep = func(time.Duration) {}

	browserURLs := []string{}
	c := &Client{
		HTTP:         hc,
		APIBase:      srv.URL,
		PollInterval: 1 * time.Millisecond,
		Timeout:      2 * time.Second,
		Sleep:        func(time.Duration) {},
		RandomState:  func() (string, error) { return strings.Repeat("a", 64), nil },
		Stderr:       new(bytes.Buffer),
		OpenBrowser: func(url string) error {
			browserURLs = append(browserURLs, url)
			return nil
		},
	}
	return c, &browserURLs
}

// --- Happy path ---------------------------------------------------

func TestLogin_HappyPath(t *testing.T) {
	var pollCalls atomic.Int32
	var initState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/cli/handoff/init":
			body := decodeJSON(t, r)
			initState = body["state"].(string)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"code":"ABC-DEF-GHI","verification_uri":"https://web.test/cli-auth?code=ABC-DEF-GHI&state=%s","expires_in":300}`, initState)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/cli/handoff/ABC-DEF-GHI/poll":
			n := pollCalls.Add(1)
			if r.URL.Query().Get("state") != initState {
				t.Errorf("poll: state mismatch — got %q want %q", r.URL.Query().Get("state"), initState)
			}
			if n == 1 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"api_key":"molt_real_key_xxx","expires_in":280}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c, browserURLs := newTestClient(t, srv)
	res, err := c.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.APIKey != "molt_real_key_xxx" {
		t.Errorf("APIKey = %q", res.APIKey)
	}
	if pollCalls.Load() < 2 {
		t.Errorf("expected at least 2 poll calls, got %d", pollCalls.Load())
	}
	if len(*browserURLs) != 1 {
		t.Errorf("expected 1 browser open call, got %d", len(*browserURLs))
	}
	if len(*browserURLs) == 1 && !strings.Contains((*browserURLs)[0], "ABC-DEF-GHI") {
		t.Errorf("browser URL = %q, missing code", (*browserURLs)[0])
	}
	if initState != strings.Repeat("a", 64) {
		t.Errorf("init state = %q, want repeated-a", initState)
	}
}

// HandoffNotSupportedError fires when init returns 404 (server too old
// or wrong host) so the renderer can show a user-actionable hint rather
// than the bare "init returned 404" string the user gets from a raw
// fmt.Errorf. Covers both 404 and 405; the typed error must carry the
// APIBase so the hint can reflect the host the user actually targeted.
func TestLogin_InitNotSupported(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"404 not found", http.StatusNotFound},
		{"405 method not allowed", http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)

			c, _ := newTestClient(t, srv)
			_, err := c.Login(context.Background())
			if err == nil {
				t.Fatal("Login returned nil error; want HandoffNotSupportedError")
			}
			var hns *molterrors.HandoffNotSupportedError
			if !stderrors.As(err, &hns) {
				t.Fatalf("err = %T %v; want *HandoffNotSupportedError", err, err)
			}
			if hns.APIBase != srv.URL {
				t.Errorf("APIBase = %q; want %q", hns.APIBase, srv.URL)
			}
		})
	}
}

// --- Init 503 retries via httpc-shaped retry policy --------------

func TestLogin_InitRetriesOn503(t *testing.T) {
	var initCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cli/handoff/init" {
			if initCalls.Add(1) < 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":"X","verification_uri":"u","expires_in":300}`))
			return
		}
		if r.URL.Path == "/v1/cli/handoff/X/poll" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"api_key":"molt_key","expires_in":300}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv)
	if _, err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if initCalls.Load() < 2 {
		t.Errorf("expected at least 2 init attempts (retry on 503), got %d", initCalls.Load())
	}
}

// Regression: doHTTPWithRetry must rewind the init body between attempts.
// Without req.GetBody (set by doInit), net/http consumes the body on the
// first attempt; the retried request would arrive empty and the server
// would reject as 400 "missing state", silently turning a transient 5xx
// into a permanent failure.
func TestLogin_InitRetryReplaysRequestBody(t *testing.T) {
	var initCalls atomic.Int32
	var attemptBodies sync.Map // attempt number → body bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cli/handoff/init" {
			body, _ := io.ReadAll(r.Body)
			n := initCalls.Add(1)
			attemptBodies.Store(n, string(body))
			if n < 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":"X","verification_uri":"u","expires_in":300}`))
			return
		}
		if r.URL.Path == "/v1/cli/handoff/X/poll" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"api_key":"molt_key","expires_in":300}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv)
	if _, err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got := initCalls.Load(); got < 2 {
		t.Fatalf("expected at least 2 init attempts, got %d", got)
	}
	first, _ := attemptBodies.Load(int32(1))
	second, _ := attemptBodies.Load(int32(2))
	firstBody, _ := first.(string)
	secondBody, _ := second.(string)
	if firstBody == "" {
		t.Fatal("first attempt sent empty body — unexpected; test setup issue")
	}
	if secondBody != firstBody {
		t.Fatalf("retry sent different body than first attempt — body rewind broken\n  first: %q\n second: %q", firstBody, secondBody)
	}
}

// --- Init persistent 503 → ServiceUnavailableError ----------------

func TestLogin_InitExhaustedReturnsServiceUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv)
	_, err := c.Login(context.Background())
	if err == nil {
		t.Fatal("Login: want ServiceUnavailableError, got nil")
	}
	var sue *molterrors.ServiceUnavailableError
	if !stderrors.As(err, &sue) {
		t.Fatalf("err = %T (%v), want *ServiceUnavailableError", err, err)
	}
}

// --- Poll timeout -------------------------------------------------

func TestLogin_PollTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cli/handoff/init" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":"X","verification_uri":"u","expires_in":300}`))
			return
		}
		w.WriteHeader(http.StatusAccepted) // always pending
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv)
	c.Timeout = 30 * time.Millisecond
	c.PollInterval = 5 * time.Millisecond
	// Use real sleep so the deadline timer trips.
	c.Sleep = nil

	_, err := c.Login(context.Background())
	if err == nil {
		t.Fatal("Login: want LoginTimeoutError, got nil")
	}
	var le *molterrors.LoginTimeoutError
	if !stderrors.As(err, &le) {
		t.Fatalf("err = %T (%v), want *LoginTimeoutError", err, err)
	}
}

// --- Poll 410 → LoginExpiredError --------------------------------

func TestLogin_Poll410ReturnsExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cli/handoff/init" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":"X","verification_uri":"u","expires_in":300}`))
			return
		}
		w.WriteHeader(http.StatusGone)
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv)
	_, err := c.Login(context.Background())
	if err == nil {
		t.Fatal("Login: want LoginExpiredError, got nil")
	}
	var le *molterrors.LoginExpiredError
	if !stderrors.As(err, &le) {
		t.Fatalf("err = %T (%v), want *LoginExpiredError", err, err)
	}
}

// Server emits 410 with a typed REJECTED code when the user clicked
// "I didn't start this" on /cli-auth. The CLI must preserve that
// signal — masking it as "expired" hides a possible phishing attempt.
func TestPoll_410_REJECTED_ReturnsCancelledError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cli/handoff/init" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":"X","verification_uri":"u","expires_in":300}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":{"code":"REJECTED","message":"handoff was cancelled in the browser"}}`))
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv)
	_, err := c.Login(context.Background())
	if err == nil {
		t.Fatal("Login: want LoginCancelledError, got nil")
	}
	var lce *molterrors.LoginCancelledError
	if !stderrors.As(err, &lce) {
		t.Fatalf("err = %T (%v), want *LoginCancelledError", err, err)
	}
}

// 410 with the existing "GONE" code (TTL elapsed or already consumed)
// must still produce LoginExpiredError — the historical default.
func TestPoll_410_GONE_ReturnsExpiredError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cli/handoff/init" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":"X","verification_uri":"u","expires_in":300}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":{"code":"GONE","message":"handoff code expired"}}`))
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv)
	_, err := c.Login(context.Background())
	if err == nil {
		t.Fatal("Login: want LoginExpiredError, got nil")
	}
	var lee *molterrors.LoginExpiredError
	if !stderrors.As(err, &lee) {
		t.Fatalf("err = %T (%v), want *LoginExpiredError", err, err)
	}
}

// Graceful fallback: a server that returns 410 with no body (or any
// future shape we don't recognize) must keep the historical
// LoginExpiredError behavior — the CLI's terminal state machine can't
// break the moment the server changes its error envelope.
func TestPoll_410_EmptyBody_FallsBackToExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cli/handoff/init" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":"X","verification_uri":"u","expires_in":300}`))
			return
		}
		w.WriteHeader(http.StatusGone)
		// no body
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv)
	_, err := c.Login(context.Background())
	if err == nil {
		t.Fatal("Login: want LoginExpiredError, got nil")
	}
	var lee *molterrors.LoginExpiredError
	if !stderrors.As(err, &lee) {
		t.Fatalf("err = %T (%v), want *LoginExpiredError", err, err)
	}
}

// --- Poll 403 → StateMismatchError --------------------------------

func TestLogin_Poll403ReturnsStateMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cli/handoff/init" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":"X","verification_uri":"u","expires_in":300}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv)
	_, err := c.Login(context.Background())
	if err == nil {
		t.Fatal("Login: want StateMismatchError, got nil")
	}
	var se *molterrors.StateMismatchError
	if !stderrors.As(err, &se) {
		t.Fatalf("err = %T (%v), want *StateMismatchError", err, err)
	}
}

// --- OpenBrowser failure does not block poll ---------------------

func TestLogin_OpenBrowserFailureDoesNotBlockPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cli/handoff/init" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":"X","verification_uri":"u","expires_in":300}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_key":"molt_key","expires_in":300}`))
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv)
	c.OpenBrowser = func(url string) error { return fmt.Errorf("no display") }
	stderr := new(bytes.Buffer)
	c.Stderr = stderr

	res, err := c.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.APIKey != "molt_key" {
		t.Errorf("APIKey = %q", res.APIKey)
	}
	if !strings.Contains(stderr.String(), "manually") {
		t.Errorf("stderr missing manual-open hint: %q", stderr.String())
	}
}

// --- Helpers -----------------------------------------------------

func decodeJSON(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	defer r.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}
