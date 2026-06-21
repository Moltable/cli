// Package httpc is the moltable CLI's internal HTTP client. It wraps
// net/http with the conventions every command body relies on:
//
//   - Authorization: Bearer molt_xxx attached automatically.
//   - 30s default timeout (overrideable per-request via context).
//   - Retry on 502/503/504 with exponential backoff (max 3 attempts).
//   - X-Moltable-Version response header parsed → typed Response.
//   - Sunset response header parsed → Response.Deprecated + SunsetAt.
//   - Typed errors from the internal/errors package: AuthError on 401,
//     ServiceUnavailableError after retries exhausted on 5xx,
//     ServerTooOldError when the server version is below the floor.
//
// Callers use Do() and inspect the returned *Response. Higher-level
// helpers (GetJSON, etc.) live in the command packages that need
// them — we don't ship half-spec'd convenience wrappers here.
//
// Pattern reference: gh CLI's pkg/cmd/api package — same shape, same
// header-driven deprecation contract, same backoff knobs.
package httpc

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	molterrors "github.com/moltable/cli/internal/errors"
)

// MinServerVersion is the lowest server version this CLI will talk
// to. Compared against the server's X-Moltable-Version response
// header; a server below this floor raises ServerTooOldError. Bump
// here when introducing CLI features that depend on newer
// server-side endpoints or response shapes.
const MinServerVersion = "0.1.0"

// DefaultTimeout is the per-request wall clock budget. Picked at 30s
// because table list/get/create round trips comfortably finish under
// 5s and we want stuck connections to surface fast, not stretch into
// a 5-minute Kong-default mystery hang.
const DefaultTimeout = 30 * time.Second

// MaxAttempts is the cap on total tries (initial + retries) for
// idempotent failures that 5xx-class servers occasionally return.
const MaxAttempts = 3

// BaseBackoff is the initial sleep between retries. With exponential
// growth (BaseBackoff * 2^(attempt-1)) and MaxAttempts=3, the worst-
// case sleeping wait is BaseBackoff * (1 + 2) = 3 * BaseBackoff.
const BaseBackoff = 200 * time.Millisecond

// retryableStatusCodes is the closed set of response codes the client
// will retry. 502/503/504 are transient by definition; 500 is not (it
// usually indicates a deterministic bug we shouldn't paper over).
var retryableStatusCodes = map[int]bool{
	http.StatusBadGateway:         true, // 502
	http.StatusServiceUnavailable: true, // 503
	http.StatusGatewayTimeout:     true, // 504
}

// Client is a thin wrapper over net/http that injects auth, parses
// moltable-specific headers, and renders typed errors.
//
// Construct via New; the zero value is not safe to use.
type Client struct {
	// BaseURL is the moltable API root (e.g. https://api.moltable.com).
	// Trailing slashes are stripped during request construction.
	BaseURL string

	// APIKey is the molt_ token attached as Authorization: Bearer ...
	// Required; New rejects empty strings.
	APIKey string

	// UserAgent identifies the CLI build to the server. Defaults to
	// "moltable-cli/dev" when New is called with an empty value.
	UserAgent string

	// HTTP is the underlying net/http client. Tests inject a custom
	// Transport via this field to point at httptest.Server URLs.
	HTTP *http.Client

	// Now returns the current time; swappable for deterministic
	// retry-timing tests. Defaults to time.Now in New.
	Now func() time.Time

	// Sleep blocks for d. Swappable so tests don't wait real seconds
	// during backoff verification. When nil, Do() uses a context-aware
	// timer (preferred for production so ctrl-C tears down retries
	// cleanly). Tests set this to a no-op or a recording function.
	Sleep func(d time.Duration)
}

// IsLoopbackURL reports whether baseURL targets a loopback host. The
// only addresses we treat as loopback are `localhost`, `127.0.0.1`,
// and `::1`. Anything else — including 192.168.x.x and other RFC1918
// space — counts as remote, because the user might legitimately have
// `host.docker.internal` in their MOLTABLE_API_BASE, but we cannot
// know whether that maps to a host they control or to an attacker
// behind the same proxy.
//
// Lives here (not in cmd/moltable) so the TLS gate inside
// NewWithOptions can be enforced as a defense-in-depth check rather
// than relying on every call site to do the right thing.
func IsLoopbackURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// New constructs a Client with sane defaults.
//
//   - APIKey is required and validated; missing returns AuthError.
//   - BaseURL is required; missing returns InvalidInputError.
//   - UserAgent defaults to "moltable-cli/dev".
//   - HTTP timeout defaults to DefaultTimeout.
//
// New does NOT call out to the network; auth validation is purely
// a molt_ prefix shape check, no /v1/me round-trip.
func New(baseURL, apiKey, userAgent string) (*Client, error) {
	return NewWithOptions(baseURL, apiKey, userAgent, Options{})
}

// Options carries the rarely-set knobs that don't belong on every New
// call site. Today only InsecureSkipTLSVerify lives here; more options
// can be added without breaking the cheap (baseURL, apiKey, userAgent)
// 3-arg path that 90% of call sites use.
type Options struct {
	// InsecureSkipTLSVerify drops certificate verification on the
	// underlying HTTP transport. Use ONLY for local development
	// against self-signed devcerts (e.g. https://localhost:8080).
	// The --dev / MOLTABLE_DEV path in cmd/moltable enables this
	// automatically; production callers must leave it false.
	InsecureSkipTLSVerify bool
}

// NewWithOptions is New plus the Options struct. Kept separate so the
// common case stays a 3-arg call.
//
// InsecureSkipTLSVerify is double-gated: the caller's flag is necessary
// but not sufficient. The resolved baseURL must ALSO be a loopback host
// (localhost / 127.0.0.1 / ::1). Combining --dev / MOLTABLE_DEV with
// MOLTABLE_API_BASE=https://attacker.example.com would otherwise skip
// cert verification against an arbitrary host with the molt_ Bearer
// token in flight — a silent MITM vector. When the gate trips the
// transport keeps full TLS verification AND a one-line stderr warning
// would ideally fire, but to keep this package side-effect-free we
// silently degrade to secure mode here; the cmd layer is responsible
// for surfacing the "--dev ignored on non-loopback host" warning.
func NewWithOptions(baseURL, apiKey, userAgent string, opts Options) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, &molterrors.InvalidInputError{
			Field:  "base URL",
			Detail: "moltable API URL is required",
		}
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, &molterrors.AuthError{Reason: "missing"}
	}
	if userAgent == "" {
		userAgent = "moltable-cli/dev"
	}
	httpClient := &http.Client{Timeout: DefaultTimeout}
	if opts.InsecureSkipTLSVerify && IsLoopbackURL(baseURL) {
		// Clone DefaultTransport so we keep the production tuning
		// (idle pool, dial timeouts) and only flip the TLS verification
		// flag. The cloned transport is scoped to this Client; other
		// Clients in the same process keep their secure transports.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- gated on caller-explicit opts.InsecureSkipTLSVerify AND loopback host check.
		httpClient.Transport = tr
	}
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		UserAgent: userAgent,
		HTTP:      httpClient,
		Now:       time.Now,
		// Sleep intentionally left nil — Do() falls back to the
		// context-aware timer so retries are cancellable. Tests
		// override this field directly.
	}, nil
}

// Response is the parsed envelope every Do() call returns. It bundles
// the raw body with the moltable-specific headers so callers can
// react to deprecation + version drift without re-parsing.
type Response struct {
	// StatusCode is the final response's HTTP status.
	StatusCode int

	// Body is the response body bytes — fully drained and closed.
	Body []byte

	// Header is the response header map (copied; safe to mutate).
	Header http.Header

	// APIVersion is the X-Moltable-Version header value, empty if
	// the server didn't emit one. Used by the update-check nudge.
	APIVersion string

	// Deprecated mirrors "did the response carry a Sunset header?".
	// Callers should print a stderr warning when this is true.
	Deprecated bool

	// SunsetAt is the parsed Sunset header time. Nil when absent or
	// unparseable. Compare against time.Now() to decide whether to
	// emit a soft warning vs a hard DeprecationStopError.
	SunsetAt *time.Time
}

// Request is the input shape for Do. Method + Path are required; the
// rest are optional. Path may be absolute ("/v1/tables") or include
// query string; the client joins it with BaseURL.
type Request struct {
	Method string
	Path   string

	// Body is sent verbatim. Nil means no body. The client does not
	// auto-marshal — callers JSON-encode upstream (the JSON path is
	// uniform via output.Print).
	Body []byte

	// Query is appended to the URL as ?k=v&k=v. Order is the slice
	// order; same-key entries become repeated parameters.
	Query url.Values

	// Headers are layered on top of the client defaults (auth + UA +
	// Accept). Caller-supplied values WIN on conflict.
	Headers http.Header

	// ContentType, if non-empty, sets Content-Type. Defaults to
	// application/json when Body is non-nil.
	ContentType string
}

// Do executes req under the client's retry + auth conventions and
// returns the parsed Response. The provided context bounds the entire
// retry loop (not per-attempt).
//
// Error contract:
//
//   - *AuthError on 401 (any attempt — never retried).
//   - *ServiceUnavailableError after MaxAttempts retries on 5xx.
//   - *ServerTooOldError when X-Moltable-Version < MinServerVersion.
//   - context.Canceled / context.DeadlineExceeded propagated as-is.
//   - Other 4xx return (Response, nil) — the caller decides whether
//     it's a NotFoundError, RateLimitError, validation error, etc.
//     Status-to-typed-error mapping lives in the command layer.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	if req.Method == "" {
		return nil, &molterrors.InvalidInputError{Field: "method", Detail: "HTTP method is required"}
	}
	if req.Path == "" {
		return nil, &molterrors.InvalidInputError{Field: "path", Detail: "request path is required"}
	}

	var lastResp *Response
	var lastStatus int
	attempts := 0
	for attempts < MaxAttempts {
		attempts++
		resp, err := c.doOnce(ctx, req)
		if err != nil {
			// Network / context errors are not retried — they almost
			// always indicate a misconfiguration (bad host, DNS) or
			// a deliberate caller-side cancel.
			return nil, err
		}
		lastResp = resp
		lastStatus = resp.StatusCode

		// Auth failures are terminal even on the first try — never
		// retry an unauthorized request (it would just rate-limit us).
		if resp.StatusCode == http.StatusUnauthorized {
			return resp, &molterrors.AuthError{Reason: "401"}
		}

		// Server-version floor check runs regardless of status code,
		// because a 4xx from an old server still tells us our wire
		// contract is mismatched.
		if resp.APIVersion != "" {
			cmp, ok := compareSemver(resp.APIVersion, MinServerVersion)
			if ok && cmp < 0 {
				return resp, &molterrors.ServerTooOldError{
					ServerVersion:    resp.APIVersion,
					MinServerVersion: MinServerVersion,
				}
			}
		}

		// Retry on transient 5xx with backoff. Only sleep when there
		// are attempts remaining — last attempt falls through to the
		// ServiceUnavailableError below.
		if retryableStatusCodes[resp.StatusCode] {
			if attempts < MaxAttempts {
				if err := c.sleepWithCtx(ctx, backoffFor(attempts)); err != nil {
					return resp, err
				}
				continue
			}
			return resp, &molterrors.ServiceUnavailableError{
				Attempts:   attempts,
				StatusCode: resp.StatusCode,
			}
		}

		return resp, nil
	}

	// Should be unreachable — the loop returns within each branch.
	// Defensive return so the function signature compiles.
	return lastResp, &molterrors.ServiceUnavailableError{
		Attempts:   attempts,
		StatusCode: lastStatus,
	}
}

// doOnce performs a single HTTP attempt without retry semantics.
func (c *Client) doOnce(ctx context.Context, req Request) (*Response, error) {
	fullURL := c.BaseURL + req.Path
	if len(req.Query) > 0 {
		sep := "?"
		if strings.Contains(req.Path, "?") {
			sep = "&"
		}
		fullURL = fullURL + sep + req.Query.Encode()
	}

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("httpc: build request: %w", err)
	}

	// Default headers — caller overrides win on the same key.
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("User-Agent", c.UserAgent)
	httpReq.Header.Set("Accept", "application/json")
	if len(req.Body) > 0 {
		ct := req.ContentType
		if ct == "" {
			ct = "application/json"
		}
		httpReq.Header.Set("Content-Type", ct)
	}
	for k, vs := range req.Headers {
		httpReq.Header[k] = vs
	}

	httpResp, err := c.HTTP.Do(httpReq)
	if err != nil {
		// Pass through context errors unwrapped so callers using
		// errors.Is(err, context.Canceled) still match.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("httpc: %w", err)
	}
	defer httpResp.Body.Close()

	bodyBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("httpc: read body: %w", err)
	}

	r := &Response{
		StatusCode: httpResp.StatusCode,
		Body:       bodyBytes,
		Header:     httpResp.Header.Clone(),
		APIVersion: strings.TrimSpace(httpResp.Header.Get("X-Moltable-Version")),
	}

	if sunset := strings.TrimSpace(httpResp.Header.Get("Sunset")); sunset != "" {
		r.Deprecated = true
		if t, ok := parseSunset(sunset); ok {
			r.SunsetAt = &t
		}
	}

	return r, nil
}

// sleepWithCtx sleeps for d unless ctx is canceled first.
//
// Production path (c.Sleep == nil): use a timer + ctx.Done() so
// ctrl-C tears down the current backoff window cleanly.
//
// Test path (c.Sleep set by the test): call the override directly
// (typically a no-op or recorder). The override skips real wall-clock
// time but still respects ctx cancellation via the pre-check.
func (c *Client) sleepWithCtx(ctx context.Context, d time.Duration) error {
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

// backoffFor returns the exponential backoff for the Nth attempt
// (attempt indexed from 1). Attempt 1 = BaseBackoff, attempt 2 =
// BaseBackoff * 2, attempt 3 = BaseBackoff * 4.
func backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		return BaseBackoff
	}
	return BaseBackoff << (attempt - 1)
}

// parseSunset accepts both forms RFC 8594 allows:
//
//   - HTTP-date (RFC 1123): "Wed, 11 Nov 2026 23:59:59 GMT"
//   - ISO 8601 / RFC 3339: "2026-11-11T23:59:59Z"
//
// Returns the parsed time + true on success, zero time + false on
// failure. Failures are not fatal; callers still flag Deprecated.
func parseSunset(v string) (time.Time, bool) {
	layouts := []string{
		time.RFC1123,
		http.TimeFormat, // alias of RFC1123 with GMT
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// compareSemver compares two semantic-version-ish strings ("0.1.0",
// "1.2.3") and returns (-1, 0, 1) for (a<b, a==b, a>b). The second
// return is false if either input failed to parse (caller treats as
// "no constraint check possible" and proceeds).
//
// The parser is deliberately permissive:
//
//   - Strips a leading "v".
//   - Splits on "-" so a build/pre-release suffix is ignored.
//   - Treats missing minor/patch as zero ("1" == "1.0.0").
//   - Returns false on any non-numeric segment so a server emitting
//     a garbage version doesn't accidentally trigger ServerTooOldError.
func compareSemver(a, b string) (int, bool) {
	pa, ok := parseSemver(a)
	if !ok {
		return 0, false
	}
	pb, ok := parseSemver(b)
	if !ok {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		switch {
		case pa[i] < pb[i]:
			return -1, true
		case pa[i] > pb[i]:
			return 1, true
		}
	}
	return 0, true
}

func parseSemver(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// IsRetryable reports whether status is a 5xx code this client would
// retry. Exposed so unit tests + downstream callers can mirror the
// policy without duplicating the table.
func IsRetryable(status int) bool {
	return retryableStatusCodes[status]
}

// ErrEmptyResponse is returned for the (rare) zero-byte body case
// when a caller expects JSON. Not constructed by Do itself; exported
// so command bodies can share it.
var ErrEmptyResponse = errors.New("httpc: empty response body")
