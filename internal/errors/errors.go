// Package errors defines the typed error tree the moltable CLI uses
// to render user-facing failures.
//
// Every error returned to the user from a command body should satisfy
// the `error` interface AND expose two human-facing affordances:
//
//   - UserMessage() string — a single complete sentence, no jargon,
//     telling the user what went wrong.
//   - Hint() string — a next-action sentence (often imperative): the
//     exact command to run, the env var to check, the URL to visit.
//
// The top-level error printer in cmd/moltable/main.go calls these
// two methods directly so the stderr layout is uniform across every
// command:
//
//	moltable: <UserMessage>
//	hint: <Hint>
//
// Errors that don't satisfy these interfaces fall through to the
// generic "moltable: %v" path. New error types must be added here,
// not inlined in command code, so the exit-code map stays
// exhaustive.
//
// Exit codes:
//
//	0  success
//	1  generic failure
//	2  auth (AuthError)
//	3  not found (NotFoundError)
//	4  rate limit (RateLimitError)
//	5  deprecation stop (DeprecationStopError)
//
// ServiceUnavailableError, ServerTooOldError, and InvalidInputError
// map to exit code 1 today — they are typed so future versions can
// surface them distinctly in agent output without changing the
// wire contract here.
package errors

import (
	stderrors "errors"
	"fmt"
	"time"
)

// Hinter is satisfied by errors that can suggest a next action to the
// user. The CLI's central error printer detects this interface and
// renders the hint on a follow-up line.
//
// This matches the shape of `auth.Hinter` in
// internal/auth/resolver.go. Both interfaces are intentionally
// structurally identical so any auth-layer error already satisfies
// errors.Hinter and vice versa — no adapter required.
type Hinter interface {
	Hint() string
}

// UserMessenger is satisfied by errors that carry a human-readable
// sentence suitable for direct stderr rendering. Errors that don't
// implement this fall back to Error().
type UserMessenger interface {
	UserMessage() string
}

// AuthError signals an authentication failure: a missing, invalid, or
// revoked API key. Maps to exit code 2.
type AuthError struct {
	// Reason is an optional short tag for logs ("missing", "invalid",
	// "revoked"). It is NOT shown to users.
	Reason string
}

func (e *AuthError) Error() string       { return e.UserMessage() }
func (e *AuthError) UserMessage() string { return "Authentication failed." }
func (e *AuthError) Hint() string {
	return "Your API key may be invalid or revoked. Run `moltable auth login`."
}

// NotFoundError signals a 404 from the API or a local "no such
// profile/workbook/table" failure. Maps to exit code 3.
type NotFoundError struct {
	// Kind is the noun ("table", "workbook", "column", "profile").
	Kind string
	// ID is the identifier that wasn't found.
	ID string
}

func (e *NotFoundError) Error() string { return e.UserMessage() }
func (e *NotFoundError) UserMessage() string {
	if e.Kind == "" {
		return "Not found."
	}
	if e.ID == "" {
		return fmt.Sprintf("%s not found.", capitalize(e.Kind))
	}
	return fmt.Sprintf("%s %q not found.", capitalize(e.Kind), e.ID)
}
func (e *NotFoundError) Hint() string {
	if e.Kind == "" {
		return "Double-check the identifier and try again."
	}
	return fmt.Sprintf("Run `moltable %s list` to see available %ss.", e.Kind, e.Kind)
}

// RateLimitError signals a 429 from the API. Maps to exit code 4.
type RateLimitError struct {
	// RetryAfter is parsed from the Retry-After response header. Zero
	// when the header was absent or unparseable.
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return e.UserMessage() }
func (e *RateLimitError) UserMessage() string {
	return "Rate limit exceeded."
}
func (e *RateLimitError) Hint() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("Wait %s and retry, or back off your request rate.", e.RetryAfter.Round(time.Second))
	}
	return "Wait a moment and retry, or back off your request rate."
}

// DeprecationStopError signals that the server responded with a
// Sunset date that has now passed — the CLI refuses to proceed. Maps
// to exit code 5.
type DeprecationStopError struct {
	// SunsetAt is the parsed Sunset header value.
	SunsetAt time.Time
}

func (e *DeprecationStopError) Error() string { return e.UserMessage() }
func (e *DeprecationStopError) UserMessage() string {
	if e.SunsetAt.IsZero() {
		return "This API endpoint has been sunset."
	}
	return fmt.Sprintf("This API endpoint was sunset on %s.", e.SunsetAt.UTC().Format("2006-01-02"))
}
func (e *DeprecationStopError) Hint() string {
	return "Upgrade the CLI with `moltable upgrade` to use the supported endpoints."
}

// ServiceUnavailableError signals a persistent 5xx after all retries
// were exhausted. Maps to exit code 1 (generic) but typed so callers
// can render it specially.
type ServiceUnavailableError struct {
	// Attempts is how many times we retried before giving up. Used in
	// the user message so support tickets carry the count.
	Attempts int
	// StatusCode is the last status seen (typically 503 or 504).
	StatusCode int
}

func (e *ServiceUnavailableError) Error() string { return e.UserMessage() }
func (e *ServiceUnavailableError) UserMessage() string {
	if e.Attempts > 0 {
		return fmt.Sprintf("The moltable API is unavailable (failed after %d attempts).", e.Attempts)
	}
	return "The moltable API is unavailable."
}
func (e *ServiceUnavailableError) Hint() string {
	return "Check https://status.moltable.com or retry in a few minutes."
}

// HandoffNotSupportedError signals that the moltable API the CLI
// reached doesn't expose the `/v1/cli/handoff/*` browser-handoff
// endpoints. Typically the server predates the CLI feature, or the
// user is pointed at a stale instance. Surfaced from runHandoffLogin
// when init returns 404 / 405.
type HandoffNotSupportedError struct {
	// APIBase is the base URL the CLI was talking to — included in the
	// hint so the user can see which server replied.
	APIBase string
}

func (e *HandoffNotSupportedError) Error() string { return e.UserMessage() }
func (e *HandoffNotSupportedError) UserMessage() string {
	return "The moltable server doesn't support CLI browser-handoff auth."
}
func (e *HandoffNotSupportedError) Hint() string {
	if e.APIBase != "" {
		return fmt.Sprintf(
			"Server %s has no /v1/cli/handoff/* endpoints. Try `moltable --dev auth login` for a local dev server, or run `moltable upgrade` once the feature ships.",
			e.APIBase,
		)
	}
	return "Try `moltable --dev auth login` for a local dev server, or run `moltable upgrade` once the feature ships."
}

// ServerTooOldError signals that the server's advertised X-Moltable-
// Version is older than the CLI's MinServerVersion floor. Maps to
// exit code 1 today; the hint guides users to either upgrade the
// server or downgrade the CLI.
type ServerTooOldError struct {
	// ServerVersion is the value of the X-Moltable-Version response
	// header at the time of the failure.
	ServerVersion string
	// MinServerVersion is the CLI's compile-time floor.
	MinServerVersion string
}

func (e *ServerTooOldError) Error() string { return e.UserMessage() }
func (e *ServerTooOldError) UserMessage() string {
	return fmt.Sprintf(
		"This CLI requires moltable API %s or newer (server reports %s).",
		e.MinServerVersion, e.ServerVersion,
	)
}
func (e *ServerTooOldError) Hint() string {
	return "Upgrade the server, or install an older CLI with `moltable upgrade --version <X>`."
}

// InvalidInputError signals a client-side validation failure — a flag
// value that doesn't parse, a file that's missing required columns,
// etc. Maps to exit code 1.
type InvalidInputError struct {
	// Field is the user-facing name of the bad input ("--workbook",
	// "rows.csv:column 3").
	Field string
	// Detail is a one-line explanation of why it's bad.
	Detail string
}

func (e *InvalidInputError) Error() string { return e.UserMessage() }
func (e *InvalidInputError) UserMessage() string {
	if e.Field == "" {
		return e.Detail
	}
	if e.Detail == "" {
		return fmt.Sprintf("Invalid value for %s.", e.Field)
	}
	return fmt.Sprintf("Invalid value for %s: %s", e.Field, e.Detail)
}
func (e *InvalidInputError) Hint() string {
	return "Run the command with `--help` to see expected values."
}

// LoginTimeoutError signals that the browser-handoff poll loop ran
// out of time before the user approved the request. Maps to exit code 1.
type LoginTimeoutError struct {
	// Timeout is the wall-clock budget that elapsed. Included in the
	// message so a future support ticket carries the bound.
	Timeout time.Duration
}

func (e *LoginTimeoutError) Error() string { return e.UserMessage() }
func (e *LoginTimeoutError) UserMessage() string {
	return "Login timed out before approval."
}
func (e *LoginTimeoutError) Hint() string {
	return "Run `moltable auth login` again."
}

// LoginExpiredError signals that the handoff endpoint returned 410 —
// the code itself expired on the server side. Maps to exit code 1.
type LoginExpiredError struct{}

func (e *LoginExpiredError) Error() string { return e.UserMessage() }
func (e *LoginExpiredError) UserMessage() string {
	return "This login attempt expired."
}
func (e *LoginExpiredError) Hint() string {
	return "Run `moltable auth login` again."
}

// LoginCancelledError signals that the handoff endpoint returned 410
// with a typed "REJECTED" code — the user (or someone in front of the
// browser) clicked "I didn't start this" on /cli-auth. Distinct from
// LoginExpiredError so the CLI can surface the social-engineering
// signal: when the cancel wasn't the legitimate user, it almost always
// means the original `moltable auth login` URL was opened by someone
// else (phishing). Maps to exit code 1, same class as
// LoginExpiredError — the recovery path (re-run login) is identical.
type LoginCancelledError struct{}

func (e *LoginCancelledError) Error() string { return e.UserMessage() }
func (e *LoginCancelledError) UserMessage() string {
	return "Login was cancelled in the browser."
}
func (e *LoginCancelledError) Hint() string {
	return "If you didn't click Cancel yourself, the link you opened may have been a phishing attempt — do not run `moltable auth login` from a URL someone sent you."
}

// StateMismatchError signals that the handoff endpoint returned 403 —
// the state we passed didn't match the entry the server has. Almost
// always indicates a CSRF-shaped problem (someone else's tab finished
// the dance for our code). Maps to exit code 1.
type StateMismatchError struct{}

func (e *StateMismatchError) Error() string { return e.UserMessage() }
func (e *StateMismatchError) UserMessage() string {
	return "Login handoff state mismatch."
}
func (e *StateMismatchError) Hint() string {
	return "Another browser tab may have approved this code. Run `moltable auth login` again."
}

// GenericError is a catch-all for failures that don't fit any other
// shape. It still implements Hint() so the printer is uniform.
type GenericError struct {
	Msg      string
	HintText string
}

func (e *GenericError) Error() string       { return e.Msg }
func (e *GenericError) UserMessage() string { return e.Msg }
func (e *GenericError) Hint() string {
	if e.HintText != "" {
		return e.HintText
	}
	return "Run with `--help` for usage, or re-run with `--debug` for details."
}

// --- Exit code mapping --------------------------------------------

// Exit codes. The numbering is part of the CLI's public contract —
// scripts (especially CI / agent harnesses) branch on these.
const (
	ExitOK                 = 0
	ExitGeneric            = 1
	ExitAuth               = 2
	ExitNotFound           = 3
	ExitRateLimit          = 4
	ExitDeprecationStop    = 5
)

// ExitCode maps an error to its exit code. Wrapped errors are
// unwrapped via errors.As so the table is order-independent for
// callers that chain typed errors with fmt.Errorf("%w", ...).
//
// Order of checks matches the numbering above: auth → not-found →
// rate-limit → deprecation-stop → everything else.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ae *AuthError
	if stderrors.As(err, &ae) {
		return ExitAuth
	}
	var nfe *NotFoundError
	if stderrors.As(err, &nfe) {
		return ExitNotFound
	}
	var rle *RateLimitError
	if stderrors.As(err, &rle) {
		return ExitRateLimit
	}
	var dse *DeprecationStopError
	if stderrors.As(err, &dse) {
		return ExitDeprecationStop
	}
	return ExitGeneric
}

// --- Did-you-mean -------------------------------------------------

// DidYouMean returns the closest candidate to input within a
// Levenshtein distance of 2, or empty string if no candidate is close
// enough. Ties resolve to the first candidate in `candidates` (stable
// across calls — callers control the priority by ordering the slice).
//
// The 2-edit threshold mirrors what `clap-rs` and the GitHub CLI use:
// it catches "tabl" → "table" and "lst" → "list" without producing
// nonsense suggestions for genuinely unrecognized input.
func DidYouMean(input string, candidates []string) string {
	if input == "" || len(candidates) == 0 {
		return ""
	}
	best := ""
	bestDist := 3 // strictly greater than threshold so first hit wins
	for _, c := range candidates {
		d := levenshtein(input, c)
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	if bestDist > 2 {
		return ""
	}
	return best
}

// levenshtein computes the classic edit distance between a and b. The
// algorithm is O(len(a)*len(b)) time and O(min(len(a),len(b))) space —
// adequate for command/flag suggestion sets numbering in the dozens.
//
// We intentionally avoid pulling in a fuzzy-match library: this stays
// dependency-free and the constants below (insert/delete/substitute
// cost = 1) are tuned for CLI-style typos.
func levenshtein(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	// Always iterate over the shorter dimension to minimize the
	// rolling-row buffer.
	if len(ar) < len(br) {
		ar, br = br, ar
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := 0; j <= len(br); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			// min(delete, insert, substitute)
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

// capitalize returns s with its first ASCII letter upper-cased. Used
// only for UserMessage strings (so "table not found" reads as
// "Table not found.").
func capitalize(s string) string {
	if s == "" {
		return s
	}
	c := s[0]
	if c >= 'a' && c <= 'z' {
		return string(c-32) + s[1:]
	}
	return s
}
