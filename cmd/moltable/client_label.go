// `moltable auth login` — device-label helpers.
//
// Every `auth login` mints a fresh `molt_` key on the server. To make
// the resulting key identifiable in the user's Settings → API Keys
// list, the CLI sends a "client_label" with the handoff init that
// the server uses to compose the api_keys.name. Format the user sees:
//
//	moltable CLI · Claudes-Mac-mini · 2026-06-21
//
// Without this fingerprint the user gets a list of keys all named
// "moltable CLI · 2026-06-21" with no way to tell which machine
// they came from.

package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// envNoHostname disables hostname capture entirely — both in the
// client_label and in the User-Agent. Privacy escape hatch for users
// on shared machines, conference demos, or anyone who'd rather not
// have their hostname stored in the server's api_keys table.
const envNoHostname = "MOLTABLE_NO_HOSTNAME"

// computeClientLabel returns the device fingerprint to send in the
// handoff init request. Precedence:
//
//  1. Explicit `--label` override → use as-is (sanitized + capped)
//  2. MOLTABLE_NO_HOSTNAME set → return "" (server falls back to
//     a date-only key name)
//  3. Default: "<hostname> · <YYYY-MM-DD>" with hostname from
//     os.Hostname(), date in UTC
//
// `now` is injected so tests can pin the date deterministically.
func computeClientLabel(override string, now time.Time) string {
	if override != "" {
		return sanitizeHostname(override)
	}
	if os.Getenv(envNoHostname) != "" {
		return ""
	}
	host := sanitizeHostname(hostnameOrEmpty())
	date := now.UTC().Format("2006-01-02")
	if host == "" {
		return date
	}
	return fmt.Sprintf("%s · %s", host, date)
}

// hostnameOrEmpty returns os.Hostname() or "" on any error. Some
// constrained environments (chroot, certain Docker setups, very early
// boot) fail to resolve a hostname; we treat that as "anonymous" and
// fall through to the date-only label rather than surfacing an
// awkward "unable to detect device" error during auth login.
func hostnameOrEmpty() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// sanitizeHostname normalizes user-supplied or OS-supplied label text
// before it goes out over the wire. The server also sanitizes (it
// can't trust client input), but doing the same work client-side
// gives the user immediate feedback in the User-Agent string and
// keeps the wire payload small. Rules:
//
//   - Trim whitespace
//   - Strip control characters (< 0x20 or 0x7F) — ANSI escapes, CR/LF,
//     anything that could ruin a log line or terminal render
//   - Cap at maxClientLabelLen so a hostname-typo'd-by-cat can't
//     send a 5KB string
func sanitizeHostname(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7F {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > maxClientLabelLen {
		out = out[:maxClientLabelLen]
	}
	return out
}

// maxClientLabelLen mirrors the server's cap (apps/api/internal/
// handler/cli_handoff.go). Keeping these in sync at the constant
// level so both sides truncate to the same width.
const maxClientLabelLen = 128
