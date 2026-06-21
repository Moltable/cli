package httpc

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	molterrors "github.com/moltable/cli/internal/errors"
)

// newTestClient builds a Client whose HTTP transport hits srv.URL and
// whose Sleep is a no-op (so retry backoff doesn't slow the suite).
// The returned Client is configured with the well-known test API key.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(srv.URL, "molt_test_key", "moltable-cli/test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Skip real wall-clock backoff during retry tests.
	c.Sleep = func(time.Duration) {}
	return c
}

// --- New() guards ------------------------------------------------

func TestNew_RejectsEmptyAPIKey(t *testing.T) {
	_, err := New("http://x", "", "ua")
	if err == nil {
		t.Fatal("New: want AuthError, got nil")
	}
	var ae *molterrors.AuthError
	if !stderrors.As(err, &ae) {
		t.Fatalf("err = %T, want *molterrors.AuthError", err)
	}
}

func TestNew_RejectsEmptyBaseURL(t *testing.T) {
	_, err := New("", "molt_x", "ua")
	if err == nil {
		t.Fatal("New: want InvalidInputError, got nil")
	}
	var iie *molterrors.InvalidInputError
	if !stderrors.As(err, &iie) {
		t.Fatalf("err = %T, want *molterrors.InvalidInputError", err)
	}
}

func TestNew_StripsTrailingSlashFromBaseURL(t *testing.T) {
	c, err := New("https://api.moltable.com/", "molt_x", "ua")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.BaseURL != "https://api.moltable.com" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
}

func TestNew_DefaultsUserAgent(t *testing.T) {
	c, err := New("https://x", "molt_x", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(c.UserAgent, "moltable-cli/") {
		t.Errorf("UserAgent = %q, want default prefix", c.UserAgent)
	}
}

func TestNewWithOptions_InsecureSkipTLSVerify(t *testing.T) {
	// Stand up an httptest TLS server with a self-signed cert (httptest
	// uses an in-memory CA). A default httpc client should fail with a
	// TLS error; a client built with InsecureSkipTLSVerify should succeed.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	t.Cleanup(srv.Close)

	// Verify the secure default fails against the self-signed cert.
	secure, err := New(srv.URL, "molt_x", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := secure.HTTP.Get(srv.URL); err == nil {
		t.Fatal("secure client should fail against self-signed TLS server; got nil error")
	}

	// And the insecure-opts client succeeds.
	insecure, err := NewWithOptions(srv.URL, "molt_x", "", Options{InsecureSkipTLSVerify: true})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	resp, err := insecure.HTTP.Get(srv.URL)
	if err != nil {
		t.Fatalf("insecure client failed against self-signed TLS server: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
}

// TLS-skip is double-gated: the caller's InsecureSkipTLSVerify is
// necessary but not sufficient. The resolved baseURL must ALSO be
// loopback (localhost / 127.0.0.1 / ::1). Combining --dev /
// MOLTABLE_DEV with MOLTABLE_API_BASE=https://attacker.example.com
// MUST keep full TLS verification — otherwise it's a silent MITM
// against an arbitrary host with the molt_ Bearer in flight.
func TestNewWithOptions_InsecureRefusedAgainstRemoteHost(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// Same TLS server, two clients: one targeting via httptest's
	// 127.0.0.1 URL (loopback → insecure honored), one targeting via
	// a synthetic remote host pointed at the same socket via the
	// httptest server's Client.Transport (insecure must be IGNORED).
	loopback, err := NewWithOptions(srv.URL, "molt_x", "", Options{InsecureSkipTLSVerify: true})
	if err != nil {
		t.Fatalf("NewWithOptions(loopback): %v", err)
	}
	if loopback.HTTP.Transport == nil {
		t.Fatal("loopback insecure client must have a custom transport (loopback host)")
	}

	// Swap in https://attacker.example.com — same Options flag, must
	// silently re-secure (no custom transport, falls back to default).
	remote, err := NewWithOptions("https://attacker.example.com", "molt_x", "", Options{InsecureSkipTLSVerify: true})
	if err != nil {
		t.Fatalf("NewWithOptions(remote): %v", err)
	}
	if remote.HTTP.Transport != nil {
		t.Fatal("remote-host client MUST NOT install an insecure transport even when InsecureSkipTLSVerify is set — loopback gate failed")
	}
}

func TestIsLoopbackURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://localhost:8080", true},
		{"http://127.0.0.1:3000", true},
		{"https://[::1]:443", true},
		{"https://LOCALHOST", true},
		{"https://localhost.attacker.com", false},
		{"https://api.moltable.io", false},
		{"https://192.168.1.10", false},
		{"https://host.docker.internal", false},
		{"http://10.0.0.1", false},
		{"::garbage://", false},
	}
	for _, tc := range cases {
		got := IsLoopbackURL(tc.in)
		if got != tc.want {
			t.Errorf("IsLoopbackURL(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

// --- Do(): happy path + header parsing ---------------------------

func TestDo_200ReturnsBodyAndAPIVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Moltable-Version", "0.2.0")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/ping"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("Status = %d", resp.StatusCode)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Errorf("Body = %q", resp.Body)
	}
	if resp.APIVersion != "0.2.0" {
		t.Errorf("APIVersion = %q, want 0.2.0", resp.APIVersion)
	}
	if resp.Deprecated {
		t.Error("Deprecated = true, want false (no Sunset header)")
	}
	if resp.SunsetAt != nil {
		t.Error("SunsetAt non-nil, want nil")
	}
}

func TestDo_SendsAuthHeaderAndUserAgent(t *testing.T) {
	var gotAuth, gotUA, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	if _, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/x"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer molt_test_key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotUA != "moltable-cli/test" {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}
}

func TestDo_BodySetsContentTypeJSONByDefault(t *testing.T) {
	var gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(201)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	body := []byte(`{"hello":"world"}`)
	_, err := c.Do(context.Background(), Request{Method: "POST", Path: "/v1/x", Body: body})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if string(gotBody) != `{"hello":"world"}` {
		t.Errorf("Body = %q", gotBody)
	}
}

func TestDo_QueryAppendsToURL(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	q := make(map[string][]string)
	q["limit"] = []string{"10"}
	q["workbook"] = []string{"wb_1"}
	_, err := c.Do(context.Background(), Request{
		Method: "GET", Path: "/v1/tables", Query: q,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !strings.Contains(gotURL, "limit=10") || !strings.Contains(gotURL, "workbook=wb_1") {
		t.Errorf("URL = %q, want both query params", gotURL)
	}
}

// --- Do(): 401 -> AuthError --------------------------------------

func TestDo_401ReturnsAuthErrorWithHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/me"})
	if err == nil {
		t.Fatal("Do: want AuthError, got nil")
	}
	var ae *molterrors.AuthError
	if !stderrors.As(err, &ae) {
		t.Fatalf("err = %T, want *molterrors.AuthError", err)
	}
	wantHint := "Your API key may be invalid or revoked. Run `moltable auth login`."
	if ae.Hint() != wantHint {
		t.Errorf("Hint = %q, want %q", ae.Hint(), wantHint)
	}
	if resp == nil || resp.StatusCode != 401 {
		t.Errorf("Response still expected (with 401); got %+v", resp)
	}
}

// --- Do(): 5xx retry semantics -----------------------------------

func TestDo_503RetriesUpToMaxAttemptsThenFails(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/x"})
	if err == nil {
		t.Fatal("Do: want ServiceUnavailableError, got nil")
	}
	var sue *molterrors.ServiceUnavailableError
	if !stderrors.As(err, &sue) {
		t.Fatalf("err = %T, want *molterrors.ServiceUnavailableError (%v)", err, err)
	}
	if sue.Attempts != MaxAttempts {
		t.Errorf("Attempts = %d, want %d", sue.Attempts, MaxAttempts)
	}
	if sue.StatusCode != 503 {
		t.Errorf("StatusCode = %d, want 503", sue.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); int(got) != MaxAttempts {
		t.Errorf("server hit count = %d, want %d (initial + retries)", got, MaxAttempts)
	}
	if resp == nil || resp.StatusCode != 503 {
		t.Errorf("Response not surfaced; got %+v", resp)
	}
}

func TestDo_503ThenSuccessOnRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`ok`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/x"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("Status = %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server hit count = %d, want 2", got)
	}
}

func TestDo_BackoffGrowsExponentially(t *testing.T) {
	// Record sleep durations so we can prove the exponential growth.
	var sleeps []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	c.Sleep = func(d time.Duration) { sleeps = append(sleeps, d) }

	_, _ = c.Do(context.Background(), Request{Method: "GET", Path: "/v1/x"})

	// MaxAttempts=3 → 2 sleeps. Want BaseBackoff, then 2*BaseBackoff.
	if len(sleeps) != MaxAttempts-1 {
		t.Fatalf("sleeps = %v, want %d entries", sleeps, MaxAttempts-1)
	}
	if sleeps[0] != BaseBackoff {
		t.Errorf("sleeps[0] = %v, want %v", sleeps[0], BaseBackoff)
	}
	if sleeps[1] != BaseBackoff*2 {
		t.Errorf("sleeps[1] = %v, want %v", sleeps[1], BaseBackoff*2)
	}
}

func TestDo_5xxNotRetriedFor500(t *testing.T) {
	// 500 is deterministic-bug territory; we surface it as a generic
	// non-retryable result so the caller can render it without
	// pretending the server might recover.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/x"})
	if err != nil {
		t.Fatalf("Do: unexpected error %v", err)
	}
	if resp.StatusCode != 500 {
		t.Errorf("Status = %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 500)", got)
	}
}

// --- Do(): Sunset header -----------------------------------------

func TestDo_SunsetHeaderSetsDeprecatedAndSunsetAt(t *testing.T) {
	// Pick a date the parser can roundtrip exactly.
	sunset := "Wed, 11 Nov 2026 23:59:59 GMT"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Sunset", sunset)
		w.Header().Set("X-Moltable-Version", "0.5.0")
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/x"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !resp.Deprecated {
		t.Error("Deprecated = false, want true")
	}
	if resp.SunsetAt == nil {
		t.Fatal("SunsetAt nil")
	}
	want := time.Date(2026, 11, 11, 23, 59, 59, 0, time.UTC)
	if !resp.SunsetAt.Equal(want) {
		t.Errorf("SunsetAt = %v, want %v", *resp.SunsetAt, want)
	}
}

func TestDo_SunsetHeaderRFC3339(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Sunset", "2027-01-15T00:00:00Z")
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/x"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.SunsetAt == nil {
		t.Fatal("SunsetAt nil")
	}
}

func TestDo_SunsetUnparseableStillFlagsDeprecated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Sunset", "soon")
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/x"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !resp.Deprecated {
		t.Error("Deprecated = false, want true even when Sunset is unparseable")
	}
	if resp.SunsetAt != nil {
		t.Error("SunsetAt should be nil when header doesn't parse")
	}
}

// --- Do(): server-version floor ----------------------------------

func TestDo_ServerTooOldErrorWhenVersionBelowFloor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Moltable-Version", "0.0.5")
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/x"})
	if err == nil {
		t.Fatal("Do: want ServerTooOldError, got nil")
	}
	var ste *molterrors.ServerTooOldError
	if !stderrors.As(err, &ste) {
		t.Fatalf("err = %T, want *molterrors.ServerTooOldError (%v)", err, err)
	}
	if ste.ServerVersion != "0.0.5" {
		t.Errorf("ServerVersion = %q", ste.ServerVersion)
	}
	if ste.MinServerVersion != MinServerVersion {
		t.Errorf("MinServerVersion = %q, want %q", ste.MinServerVersion, MinServerVersion)
	}
	if resp == nil || resp.APIVersion != "0.0.5" {
		t.Errorf("Response should still carry parsed APIVersion; got %+v", resp)
	}
}

func TestDo_ServerVersionEqualToFloorIsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Moltable-Version", MinServerVersion)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/x"})
	if err != nil {
		t.Errorf("Do: %v", err)
	}
}

func TestDo_ServerVersionGarbageIsIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Moltable-Version", "not-a-version")
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/x"})
	if err != nil {
		t.Errorf("Do: got %v; an unparseable version should NOT fail closed", err)
	}
}

// --- Do(): context cancellation ----------------------------------

func TestDo_ContextCanceledAbortsRetryLoop(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503)
	}))
	t.Cleanup(srv.Close)

	c, _ := New(srv.URL, "molt_test_key", "moltable-cli/test")
	// Tag Sleep that observes ctx cancellation: cancel mid-sleep.
	ctx, cancel := context.WithCancel(context.Background())
	c.Sleep = func(time.Duration) { cancel() }

	_, err := c.Do(ctx, Request{Method: "GET", Path: "/v1/x"})
	if err == nil {
		t.Fatal("Do: want ctx canceled error, got nil")
	}
	if !stderrors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&calls); got > MaxAttempts {
		t.Errorf("server calls = %d > MaxAttempts %d", got, MaxAttempts)
	}
}

// --- Do(): input validation --------------------------------------

func TestDo_EmptyMethodReturnsInvalidInputError(t *testing.T) {
	c, _ := New("https://x", "molt_x", "ua")
	_, err := c.Do(context.Background(), Request{Method: "", Path: "/x"})
	if err == nil {
		t.Fatal("Do: want InvalidInputError, got nil")
	}
	var iie *molterrors.InvalidInputError
	if !stderrors.As(err, &iie) {
		t.Fatalf("err = %T, want *molterrors.InvalidInputError", err)
	}
}

func TestDo_EmptyPathReturnsInvalidInputError(t *testing.T) {
	c, _ := New("https://x", "molt_x", "ua")
	_, err := c.Do(context.Background(), Request{Method: "GET", Path: ""})
	if err == nil {
		t.Fatal("Do: want InvalidInputError, got nil")
	}
	var iie *molterrors.InvalidInputError
	if !stderrors.As(err, &iie) {
		t.Fatalf("err = %T, want *molterrors.InvalidInputError", err)
	}
}

// --- Semver helpers ----------------------------------------------

func TestCompareSemver_Table(t *testing.T) {
	cases := []struct {
		a, b   string
		want   int
		wantOK bool
	}{
		{"0.1.0", "0.1.0", 0, true},
		{"0.0.5", "0.1.0", -1, true},
		{"0.2.0", "0.1.0", 1, true},
		{"v1.0.0", "1.0.0", 0, true},
		{"1.0", "1.0.0", 0, true}, // missing patch treated as 0
		{"1.0.0-beta", "1.0.0", 0, true},
		{"garbage", "0.1.0", 0, false},
		{"0.1.0", "garbage", 0, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_vs_%s", tc.a, tc.b), func(t *testing.T) {
			got, ok := compareSemver(tc.a, tc.b)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("compareSemver(%q,%q) = (%d,%t), want (%d,%t)",
					tc.a, tc.b, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestParseSunset_Layouts(t *testing.T) {
	cases := []struct {
		v       string
		wantOK  bool
		wantUTC time.Time
	}{
		{"Wed, 11 Nov 2026 23:59:59 GMT", true, time.Date(2026, 11, 11, 23, 59, 59, 0, time.UTC)},
		{"2027-01-15T00:00:00Z", true, time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)},
		{"2027-01-15", true, time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)},
		{"soon", false, time.Time{}},
		{"", false, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.v, func(t *testing.T) {
			got, ok := parseSunset(tc.v)
			if ok != tc.wantOK {
				t.Errorf("parseSunset(%q) ok = %t, want %t", tc.v, ok, tc.wantOK)
			}
			if ok && !got.Equal(tc.wantUTC) {
				t.Errorf("parseSunset(%q) = %v, want %v", tc.v, got, tc.wantUTC)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	cases := map[int]bool{
		500: false,
		502: true,
		503: true,
		504: true,
		200: false,
		429: false, // rate limit gets its own typed error
	}
	for code, want := range cases {
		if got := IsRetryable(code); got != want {
			t.Errorf("IsRetryable(%d) = %t, want %t", code, got, want)
		}
	}
}

func TestBackoffFor(t *testing.T) {
	cases := map[int]time.Duration{
		1: BaseBackoff,
		2: BaseBackoff * 2,
		3: BaseBackoff * 4,
		0: BaseBackoff,
	}
	for attempt, want := range cases {
		if got := backoffFor(attempt); got != want {
			t.Errorf("backoffFor(%d) = %v, want %v", attempt, got, want)
		}
	}
}
