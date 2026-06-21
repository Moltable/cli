package errors

import (
	stderrors "errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- AuthError ----------------------------------------------------

func TestAuthError_UserMessageAndHint(t *testing.T) {
	e := &AuthError{}
	if got := e.UserMessage(); got != "Authentication failed." {
		t.Errorf("UserMessage = %q", got)
	}
	wantHint := "Your API key may be invalid or revoked. Run `moltable auth login`."
	if got := e.Hint(); got != wantHint {
		t.Errorf("Hint = %q, want %q", got, wantHint)
	}
	// AuthError must satisfy error AND both human-facing interfaces.
	var _ error = e
	var _ Hinter = e
	var _ UserMessenger = e
}

func TestAuthError_ImplementsAuthHinterShape(t *testing.T) {
	// The package-local Hinter interface must structurally match
	// auth.Hinter so the central printer doesn't need an adapter. We
	// can't import the auth package here without creating a cycle, so
	// the surrogate interface below is exactly the auth package's
	// definition copy-pasted — if it goes out of sync the build
	// breaks somewhere else first, but this guards against silent
	// drift.
	type authHinter interface {
		Hint() string
	}
	var e error = &AuthError{}
	if _, ok := e.(authHinter); !ok {
		t.Fatal("AuthError does not satisfy auth.Hinter shape")
	}
}

// --- NotFoundError ------------------------------------------------

func TestNotFoundError_KindAndIDInMessage(t *testing.T) {
	e := &NotFoundError{Kind: "table", ID: "tbl_123"}
	got := e.UserMessage()
	if !strings.Contains(got, "Table") || !strings.Contains(got, "tbl_123") {
		t.Errorf("UserMessage = %q", got)
	}
	if got := e.Hint(); !strings.Contains(got, "table list") {
		t.Errorf("Hint = %q, want it to suggest `moltable table list`", got)
	}
}

func TestNotFoundError_KindOnly(t *testing.T) {
	e := &NotFoundError{Kind: "workbook"}
	got := e.UserMessage()
	if !strings.Contains(got, "Workbook") {
		t.Errorf("UserMessage = %q", got)
	}
}

func TestNotFoundError_BareDefaults(t *testing.T) {
	e := &NotFoundError{}
	if got := e.UserMessage(); got != "Not found." {
		t.Errorf("UserMessage = %q", got)
	}
	if got := e.Hint(); got == "" {
		t.Error("Hint should be non-empty even for bare NotFoundError")
	}
}

// --- RateLimitError -----------------------------------------------

func TestRateLimitError_HintWithRetryAfter(t *testing.T) {
	e := &RateLimitError{RetryAfter: 12 * time.Second}
	if got := e.Hint(); !strings.Contains(got, "12s") {
		t.Errorf("Hint = %q, want it to contain `12s`", got)
	}
}

func TestRateLimitError_HintWithoutRetryAfter(t *testing.T) {
	e := &RateLimitError{}
	if got := e.Hint(); !strings.Contains(strings.ToLower(got), "back off") {
		t.Errorf("Hint = %q, want it to mention backing off", got)
	}
}

// --- DeprecationStopError -----------------------------------------

func TestDeprecationStopError_WithSunsetDate(t *testing.T) {
	d := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	e := &DeprecationStopError{SunsetAt: d}
	if got := e.UserMessage(); !strings.Contains(got, "2026-06-30") {
		t.Errorf("UserMessage = %q, want it to contain 2026-06-30", got)
	}
	if got := e.Hint(); !strings.Contains(got, "upgrade") {
		t.Errorf("Hint = %q, want it to mention upgrading", got)
	}
}

// --- ServiceUnavailableError --------------------------------------

func TestServiceUnavailableError_MessageIncludesAttempts(t *testing.T) {
	e := &ServiceUnavailableError{Attempts: 3, StatusCode: 503}
	if got := e.UserMessage(); !strings.Contains(got, "3") {
		t.Errorf("UserMessage = %q, want it to contain the attempt count", got)
	}
}

// --- ServerTooOldError --------------------------------------------

func TestServerTooOldError_MessageIncludesBothVersions(t *testing.T) {
	e := &ServerTooOldError{ServerVersion: "0.0.5", MinServerVersion: "0.1.0"}
	got := e.UserMessage()
	if !strings.Contains(got, "0.0.5") || !strings.Contains(got, "0.1.0") {
		t.Errorf("UserMessage = %q, want both versions", got)
	}
}

// --- InvalidInputError --------------------------------------------

func TestInvalidInputError_FieldAndDetail(t *testing.T) {
	e := &InvalidInputError{Field: "--workbook", Detail: "must start with wb_"}
	got := e.UserMessage()
	if !strings.Contains(got, "--workbook") || !strings.Contains(got, "wb_") {
		t.Errorf("UserMessage = %q", got)
	}
}

func TestInvalidInputError_DetailOnly(t *testing.T) {
	e := &InvalidInputError{Detail: "missing required header"}
	if got := e.UserMessage(); got != "missing required header" {
		t.Errorf("UserMessage = %q", got)
	}
}

// --- GenericError -------------------------------------------------

func TestGenericError_DefaultHint(t *testing.T) {
	e := &GenericError{Msg: "boom"}
	if got := e.UserMessage(); got != "boom" {
		t.Errorf("UserMessage = %q", got)
	}
	if got := e.Hint(); !strings.Contains(got, "--help") {
		t.Errorf("Hint = %q, want default hint to mention --help", got)
	}
}

func TestGenericError_OverrideHint(t *testing.T) {
	e := &GenericError{Msg: "boom", HintText: "do the thing"}
	if got := e.Hint(); got != "do the thing" {
		t.Errorf("Hint = %q", got)
	}
}

// --- LoginCancelledError ------------------------------------------

func TestLoginCancelledError_UserMessageAndHint(t *testing.T) {
	e := &LoginCancelledError{}
	msg := e.UserMessage()
	if msg == "" {
		t.Error("UserMessage should be non-empty")
	}
	if !strings.Contains(strings.ToLower(msg), "cancel") {
		t.Errorf("UserMessage = %q, want it to mention cancellation", msg)
	}
	hint := e.Hint()
	if hint == "" {
		t.Error("Hint should be non-empty")
	}
	if !strings.Contains(strings.ToLower(hint), "phish") {
		t.Errorf("Hint = %q, want it to surface the phishing/security framing", hint)
	}
	// LoginCancelledError must satisfy the same human-facing interfaces
	// as its sibling LoginExpiredError so the central printer renders
	// it uniformly.
	var _ error = e
	var _ Hinter = e
	var _ UserMessenger = e
}

// --- ExitCode -----------------------------------------------------

func TestExitCode_TablePerErrorType(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitOK},
		{"auth", &AuthError{}, ExitAuth},
		{"not_found", &NotFoundError{Kind: "table"}, ExitNotFound},
		{"rate_limit", &RateLimitError{}, ExitRateLimit},
		{"deprecation", &DeprecationStopError{SunsetAt: time.Now()}, ExitDeprecationStop},
		{"service_unavailable", &ServiceUnavailableError{Attempts: 3}, ExitGeneric},
		{"server_too_old", &ServerTooOldError{ServerVersion: "0.0.5", MinServerVersion: "0.1.0"}, ExitGeneric},
		{"invalid_input", &InvalidInputError{Field: "--x"}, ExitGeneric},
		{"generic", &GenericError{Msg: "x"}, ExitGeneric},
		{"unknown", stderrors.New("raw"), ExitGeneric},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode(%T) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestExitCode_UnwrapsWrappedErrors(t *testing.T) {
	// Wrapping with fmt.Errorf("%w", ...) must not lose the typed
	// exit code — the command-body layer wraps liberally and the
	// printer still needs to know it's an auth failure.
	wrapped := fmt.Errorf("doing the thing: %w", &AuthError{})
	if got := ExitCode(wrapped); got != ExitAuth {
		t.Errorf("ExitCode(wrapped AuthError) = %d, want %d", got, ExitAuth)
	}
}

// --- DidYouMean ---------------------------------------------------

func TestDidYouMean_FindsClosestWithinThreshold(t *testing.T) {
	got := DidYouMean("tabl create", []string{"table create", "table list"})
	if got != "table create" {
		t.Errorf("DidYouMean = %q, want %q", got, "table create")
	}
}

func TestDidYouMean_ReturnsEmptyWhenNoneClose(t *testing.T) {
	got := DidYouMean("xyz", []string{"table create", "table list", "workbook create"})
	if got != "" {
		t.Errorf("DidYouMean = %q, want empty", got)
	}
}

func TestDidYouMean_EmptyInput(t *testing.T) {
	if got := DidYouMean("", []string{"a", "b"}); got != "" {
		t.Errorf("DidYouMean('', ...) = %q, want empty", got)
	}
}

func TestDidYouMean_EmptyCandidates(t *testing.T) {
	if got := DidYouMean("table", nil); got != "" {
		t.Errorf("DidYouMean(nil candidates) = %q, want empty", got)
	}
}

func TestDidYouMean_SingleCharTypo(t *testing.T) {
	if got := DidYouMean("lst", []string{"list", "log", "lock"}); got != "list" {
		// distance("lst","list")=1, distance("lst","log")=2,
		// distance("lst","lock")=2 — "list" wins.
		t.Errorf("DidYouMean(lst) = %q, want %q", got, "list")
	}
}

func TestDidYouMean_TiesGoToFirstCandidate(t *testing.T) {
	// distance("ab", "ax")=1, distance("ab", "by")=2 — but to test the
	// tie path we use two equal-distance candidates.
	got := DidYouMean("ab", []string{"ax", "ay"})
	if got != "ax" {
		t.Errorf("DidYouMean tie = %q, want first candidate %q", got, "ax")
	}
}

func TestLevenshtein_KnownDistances(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"kitten", "sitting", 3}, // canonical example
		{"flaw", "lawn", 2},
		{"tabl", "table", 1},
	}
	for _, tc := range cases {
		got := levenshtein(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
