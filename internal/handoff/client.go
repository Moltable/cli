// Package handoff is the moltable CLI's browser-handoff client. It
// drives the `POST /v1/cli/handoff/init` → "open browser" → `GET
// /v1/cli/handoff/{code}/poll` dance the moltable API exposes for
// browser-based credential issue (no copy-pasting `molt_` keys).
//
// The package is intentionally split from cmd/moltable/auth.go so it
// can be tested in isolation: every external seam (HTTP, browser
// open, random state, sleep) is injectable. The cmd layer plugs in
// production implementations; tests plug in httptest stubs +
// recording closures.
//
// Wire shape:
//
//	POST /v1/cli/handoff/init
//	  body:   {"state": "<32+ char hex>"}
//	  reply:  {"code": "...", "verification_uri": "...", "expires_in": 300}
//
//	GET  /v1/cli/handoff/{code}/poll?state=<state>
//	  200 reply: {"api_key": "molt_...", "expires_in": ...}
//	  202: still pending — keep polling
//	  403: state mismatch (CSRF)
//	  404: unknown code (server forgot us)
//	  410: code expired, already consumed, or rejected by the user
//
// State is sent in the URL query string. Hex (not base64url) is used
// so the value composes cleanly in URLs and logs without escape noise.
package handoff

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	molterrors "github.com/moltable/cli/internal/errors"
	"github.com/moltable/cli/internal/httpc"
	"github.com/moltable/cli/internal/ui"
)

// DefaultPollInterval is how often Login polls the server after the
// init call. 2s mirrors what `stripe login` uses and stays well clear
// of the per-code rate limiter the API server applies.
const DefaultPollInterval = 2 * time.Second

// DefaultTimeout caps the entire Login dance — init + poll loop. Set
// to match the server-side handoff TTL so a CLI that gives up at
// timeout maps cleanly to a server-side 410 on any later poll.
const DefaultTimeout = 5 * time.Minute

// StateBytes is how many random bytes we draw for the state nonce.
// 32 bytes → 64 hex chars, comfortably above the server's 32-char
// minimum.
const StateBytes = 32

// LoginResult is what a successful Login returns. KeyID is filled
// only when the poll response carries it (the v1 wire shape only
// returns api_key; KeyID is kept so future server changes can populate
// it without touching the caller signature).
type LoginResult struct {
	APIKey string
	KeyID  string
}

// initResponse is the wire shape of POST /v1/cli/handoff/init's reply.
type initResponse struct {
	Code            string `json:"code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
}

// pollResponse is the wire shape of GET /v1/cli/handoff/{code}/poll's
// 200 reply.
type pollResponse struct {
	APIKey    string `json:"api_key"`
	ExpiresIn int    `json:"expires_in"`
	KeyID     string `json:"key_id,omitempty"`
}

// Client runs the browser-handoff login dance.
//
// Construct via New for production use; tests instantiate directly so
// they can inject httptest URLs, deterministic state, no-op browser
// opener, and a synchronous Sleep.
type Client struct {
	// HTTP is the moltable HTTP client. We bypass it for the unauth'd
	// init + poll calls (those don't need a molt_ key — the code IS
	// the secret) and call a sub-resource of net/http via the same
	// HTTP transport so tests share the httptest.Server cleanup hook.
	HTTP *httpc.Client

	// APIBase is the moltable API root, e.g. https://api.moltable.io.
	// Trailing slashes are tolerated; we trim them at request time.
	APIBase string

	// OpenBrowser opens url in the user's default browser. On any
	// error, Login swallows it and just prints a "open manually"
	// message — the poll loop still runs.
	OpenBrowser func(url string) error

	// PollInterval is the delay between poll attempts. Defaults to
	// DefaultPollInterval when zero.
	PollInterval time.Duration

	// Timeout caps the entire dance. Defaults to DefaultTimeout when
	// zero.
	Timeout time.Duration

	// Sleep blocks for d. Injectable for tests; production uses a
	// context-aware timer via select.
	Sleep func(time.Duration)

	// RandomState returns a fresh nonce. Defaults to crypto/rand
	// reads of StateBytes hex-encoded. Tests inject deterministic
	// strings.
	RandomState func() (string, error)

	// Stderr is where the URL prompt + "open manually" messages go.
	// Defaults to os.Stderr in New; tests pass a *bytes.Buffer.
	Stderr interface {
		Write(p []byte) (int, error)
	}

	// ClientLabel is the device fingerprint sent in the init request
	// body. The server persists it on the handoff row and embeds it
	// in the minted api_keys.name so the user can identify this
	// device in the dashboard — e.g. "Claudes-Mac-mini · 2026-06-21"
	// renders as "moltable CLI · Claudes-Mac-mini · 2026-06-21".
	//
	// Empty string is OK and is the contract for a privacy-conscious
	// user (or a CLI version that doesn't compute one); the server
	// falls back to a date-only name. Computed in cmd/moltable/auth.go
	// from os.Hostname() unless MOLTABLE_NO_HOSTNAME is set or the
	// user passed an explicit --label.
	ClientLabel string
}

// New constructs a Client with sane defaults. The HTTP client passed
// in is reused for its transport; its API key is irrelevant for the
// handoff calls (init + poll are unauthenticated).
func New(hc *httpc.Client, apiBase string) *Client {
	return &Client{
		HTTP:         hc,
		APIBase:      strings.TrimRight(apiBase, "/"),
		OpenBrowser:  openBrowser,
		PollInterval: DefaultPollInterval,
		Timeout:      DefaultTimeout,
		RandomState:  defaultRandomState,
	}
}

// Login runs the full handoff dance:
//
//  1. Generate a state nonce.
//  2. POST /v1/cli/handoff/init with the state.
//  3. Print verification_uri to stderr; try to open the browser.
//  4. Poll GET /v1/cli/handoff/{code}/poll every PollInterval until
//     200 (success), 410 (expired), 403 (state mismatch), or Timeout.
//
// Returns the api_key on success. On failure returns a typed error:
//
//   - *molterrors.LoginTimeoutError on timeout
//   - *molterrors.LoginExpiredError on 410
//   - *molterrors.StateMismatchError on 403
//   - *molterrors.ServiceUnavailableError on persistent init 5xx
//     (propagated from the HTTP client's retry layer)
func (c *Client) Login(ctx context.Context) (*LoginResult, error) {
	if c.APIBase == "" {
		return nil, &molterrors.InvalidInputError{Field: "API base URL", Detail: "moltable API base URL is required"}
	}
	pollInterval := c.PollInterval
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	// Generate state nonce.
	state, err := c.RandomState()
	if err != nil {
		return nil, fmt.Errorf("handoff: generate state: %w", err)
	}

	// Apply the dance's overall timeout to the child context so init
	// + poll share one deadline.
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	init, err := c.doInit(dctx, state)
	if err != nil {
		return nil, err
	}

	// Print the URL + the code prominently. The web confirmation
	// page asks the user to type the code that the CLI displays —
	// this is the human-attestation step that defends against
	// phishing-link approvals (an attacker who sends a victim the
	// /cli-auth URL alone can't make the victim type a code they
	// didn't see in their own terminal). Errors swallowed: the user
	// can always click the printed URL.
	if c.Stderr != nil {
		fmt.Fprintf(c.Stderr,
			"%s\n  %s\n\n",
			"Open this URL in your browser:",
			init.VerificationURI,
		)
		fmt.Fprintln(c.Stderr, "When the page asks, enter this code:")
		ui.CodeBox(c.Stderr, init.Code)
		fmt.Fprintln(c.Stderr)
	}
	if c.OpenBrowser != nil {
		if oerr := c.OpenBrowser(init.VerificationURI); oerr != nil && c.Stderr != nil {
			fmt.Fprintf(c.Stderr, "Couldn't open browser automatically — open the URL above manually.\n")
		}
	}

	// Spin while we wait for the user to click Approve in the browser.
	// ui.NewSpinner is a no-op when stderr isn't a TTY (CI, pipes,
	// tests injecting bytes.Buffer), so this is safe for every caller.
	// Stop() before returning so the line is erased and the next
	// caller print starts at column 0.
	if c.Stderr != nil {
		sp := ui.NewSpinner(c.Stderr, "Waiting for browser approval...")
		sp.Start()
		defer sp.Stop()
	}

	// Poll until terminal.
	return c.doPoll(dctx, init.Code, state, pollInterval, timeout)
}

// doInit posts the state nonce to /v1/cli/handoff/init and returns the
// parsed response. Surfaces typed errors directly so the caller can
// react to "API is unavailable" vs "we got a code" without inspecting
// HTTP status.
func (c *Client) doInit(ctx context.Context, state string) (*initResponse, error) {
	// client_label is optional on the wire. Older servers that don't
	// know about it ignore the field; newer servers persist it and
	// use it to name the minted api_keys row.
	reqBody := map[string]string{"state": state}
	if c.ClientLabel != "" {
		reqBody["client_label"] = c.ClientLabel
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("handoff: marshal init body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBase+"/v1/cli/handoff/init", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("handoff: build init request: %w", err)
	}
	// GetBody lets doHTTPWithRetry replay the body on retry. net/http
	// consumes Body on the first attempt; without GetBody the second
	// 5xx-triggered call would send an empty request and the server
	// would reject it as 400 ("missing state"), turning a transient 5xx
	// into a permanent client error.
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.HTTP != nil && c.HTTP.UserAgent != "" {
		req.Header.Set("User-Agent", c.HTTP.UserAgent)
	}

	// Use the HTTP client's transport directly so tests sharing the
	// httptest.Server see the same connection pool. We bypass the
	// auth-injecting Do() because init is unauthenticated and adding a
	// stale molt_ Bearer would be misleading in the access log.
	//
	// Apply the same retry policy as httpc.Client for transient 5xx —
	// init is idempotent up to the state value, so retries are safe.
	resp, err := c.doHTTPWithRetry(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 5 {
		return nil, &molterrors.ServiceUnavailableError{Attempts: 1, StatusCode: resp.StatusCode}
	}
	// 404 / 405 on init means the server doesn't expose /v1/cli/handoff/*
	// at all (typically a server too old for this CLI feature, or the
	// user is pointed at a non-moltable host). Surface a typed error so
	// the renderer can show a useful hint instead of "init returned 404".
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, &molterrors.HandoffNotSupportedError{APIBase: c.APIBase}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("handoff: init returned %d", resp.StatusCode)
	}

	var out initResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("handoff: decode init response: %w", err)
	}
	if out.Code == "" || out.VerificationURI == "" {
		return nil, fmt.Errorf("handoff: init response missing code or verification_uri")
	}
	return &out, nil
}

// doPoll loops `GET /v1/cli/handoff/{code}/poll?state=X` until terminal.
func (c *Client) doPoll(ctx context.Context, code, state string, interval, timeout time.Duration) (*LoginResult, error) {
	deadline := time.Now().Add(timeout)

	for {
		// Honor context cancel + overall timeout.
		if ctx.Err() != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return nil, &molterrors.LoginTimeoutError{Timeout: timeout}
			}
			return nil, ctx.Err()
		}
		if !time.Now().Before(deadline) {
			return nil, &molterrors.LoginTimeoutError{Timeout: timeout}
		}

		pollURL := c.APIBase + "/v1/cli/handoff/" + url.PathEscape(code) + "/poll?state=" + url.QueryEscape(state)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		if err != nil {
			return nil, fmt.Errorf("handoff: build poll request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		if c.HTTP != nil && c.HTTP.UserAgent != "" {
			req.Header.Set("User-Agent", c.HTTP.UserAgent)
		}

		resp, err := c.rawDo(req)
		if err != nil {
			// Network errors during polling — fail fast rather than
			// silently looping forever on a dead network.
			return nil, fmt.Errorf("handoff: poll: %w", err)
		}

		switch resp.StatusCode {
		case http.StatusOK:
			defer resp.Body.Close()
			var out pollResponse
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				return nil, fmt.Errorf("handoff: decode poll response: %w", err)
			}
			if out.APIKey == "" {
				return nil, fmt.Errorf("handoff: poll 200 missing api_key")
			}
			return &LoginResult{APIKey: out.APIKey, KeyID: out.KeyID}, nil
		case http.StatusAccepted:
			// Still pending — close and sleep.
			_ = resp.Body.Close()
		case http.StatusForbidden:
			_ = resp.Body.Close()
			return nil, &molterrors.StateMismatchError{}
		case http.StatusGone:
			// Two distinct typed codes ride on 410: "GONE" (TTL elapsed
			// or code already consumed) and "REJECTED" (user clicked
			// "I didn't start this" on /cli-auth). The second is a real
			// social-engineering signal — drop it and the operator can't
			// tell phishing from a stale link. Decode the body to branch;
			// any decode failure / empty body / unknown code falls back
			// to LoginExpiredError so a future server change can't break
			// the CLI's terminal state machine.
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if len(body) > 0 {
				_ = json.Unmarshal(body, &env)
			}
			if env.Error.Code == "REJECTED" {
				return nil, &molterrors.LoginCancelledError{}
			}
			return nil, &molterrors.LoginExpiredError{}
		case http.StatusNotFound:
			_ = resp.Body.Close()
			return nil, &molterrors.LoginExpiredError{}
		default:
			_ = resp.Body.Close()
			if resp.StatusCode/100 == 5 {
				// Transient; retry after interval. Don't surface to user.
			} else {
				return nil, fmt.Errorf("handoff: poll returned %d", resp.StatusCode)
			}
		}

		// Sleep then loop.
		if err := c.sleep(ctx, interval); err != nil {
			if err == context.DeadlineExceeded {
				return nil, &molterrors.LoginTimeoutError{Timeout: timeout}
			}
			return nil, err
		}
	}
}

// rawDo executes req against the wrapped HTTP client without retry
// logic. Used by the poll loop, where each tick is its own attempt.
func (c *Client) rawDo(req *http.Request) (*http.Response, error) {
	if c.HTTP != nil && c.HTTP.HTTP != nil {
		return c.HTTP.HTTP.Do(req)
	}
	return http.DefaultClient.Do(req)
}

// doHTTPWithRetry runs an idempotent request with the same retry
// policy as httpc.Client: 5xx → exponential backoff up to MaxAttempts.
// Auth/4xx/2xx all return immediately.
//
// Callers with a request body MUST set req.GetBody so the body can be
// replayed on retry. net/http consumes req.Body on the first attempt;
// without GetBody, attempt 2+ would send an empty body and the server
// would reject it as 400, silently converting a transient 5xx into a
// permanent client-side failure. If req.GetBody is nil, the body is
// not rewound (correct for body-less requests).
func (c *Client) doHTTPWithRetry(req *http.Request) (*http.Response, error) {
	attempts := 0
	var lastResp *http.Response
	for attempts < httpc.MaxAttempts {
		attempts++
		if attempts > 1 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("handoff: rewind request body for retry: %w", err)
			}
			req.Body = body
		}
		resp, err := c.rawDo(req)
		if err != nil {
			return nil, err
		}
		lastResp = resp
		if !httpc.IsRetryable(resp.StatusCode) {
			return resp, nil
		}
		// Drain to EOF before close so net/http returns the connection
		// to the pool. A bare Close() on chunked / streaming responses
		// abandons the connection (it can't be reused) and persistent
		// 5xx exhausts the pool after MaxAttempts iterations.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if attempts >= httpc.MaxAttempts {
			// Synthesize a 503-shaped response so the caller can map to
			// ServiceUnavailableError without inventing a new code path.
			return nil, &molterrors.ServiceUnavailableError{
				Attempts:   attempts,
				StatusCode: resp.StatusCode,
			}
		}
		// Mirror httpc's backoff schedule.
		backoff := httpc.BaseBackoff << (attempts - 1)
		if err := c.sleep(req.Context(), backoff); err != nil {
			return lastResp, err
		}
	}
	return lastResp, nil
}

// sleep is a context-aware sleep that honors the injected Sleep
// override (tests use this to skip real wall-clock waits).
func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if c.Sleep != nil {
		c.Sleep(d)
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// defaultRandomState reads StateBytes from crypto/rand and hex-encodes.
func defaultRandomState() (string, error) {
	buf := make([]byte, StateBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("crypto/rand read: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// openBrowser attempts to open the URL in the user's default browser.
// Selection is by GOOS — mirrors how `stripe login` and `gh auth login`
// do it. Failures are surfaced to the caller; Login() decides whether
// to print a "open manually" message.
func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		// "start" is a cmd builtin, not a standalone executable.
		cmd = exec.Command("cmd", "/c", "start", target)
	default:
		// Linux / BSD — xdg-open is the de-facto desktop-environment
		// shim. Headless boxes don't have it; the caller will see the
		// error and print the URL instead.
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}
