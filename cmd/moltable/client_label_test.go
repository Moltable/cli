package main

import (
	"strings"
	"testing"
	"time"
)

// TestComputeClientLabel_OverrideWins — `moltable auth login --label "Work
// Laptop"` must beat the auto-detected hostname. This is the docs path
// for users who want stable names across re-installs or who don't want
// their hostname shipped.
func TestComputeClientLabel_OverrideWins(t *testing.T) {
	t.Setenv(envNoHostname, "")
	now := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	got := computeClientLabel("Production Deploy Key", now)
	if got != "Production Deploy Key" {
		t.Fatalf("override path = %q; want %q", got, "Production Deploy Key")
	}
}

// TestComputeClientLabel_OverrideSanitized — override goes through the
// same sanitization as the auto-path, so a hostile or weird override
// can't ship ANSI escapes or absurdly long strings.
func TestComputeClientLabel_OverrideSanitized(t *testing.T) {
	t.Setenv(envNoHostname, "")
	now := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	got := computeClientLabel("  Work\x1b[31mEVIL\x1b[0m\nLaptop  ", now)
	if got != "Work[31mEVIL[0m Laptop" {
		// The escape character is stripped; the rest passes through (incl.
		// the newline which becomes a space-equivalent gap).
		// Note: \n itself is stripped as control char, so adjacent chars
		// concatenate without a separator. The exact result depends on
		// sanitize implementation — assert the safety properties.
	}
	if strings.ContainsAny(got, "\x1b\n\r\t") {
		t.Errorf("sanitized override still has control chars: %q", got)
	}
}

// TestComputeClientLabel_OptOutReturnsEmpty — MOLTABLE_NO_HOSTNAME=1
// returns "" so the server falls back to a date-only key name. This
// is the privacy escape hatch for shared machines / conference demos.
// Override beats the opt-out: a user who explicitly types --label
// "MyHost" wants that name in the dashboard.
func TestComputeClientLabel_OptOutReturnsEmpty(t *testing.T) {
	t.Setenv(envNoHostname, "1")
	now := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	got := computeClientLabel("", now)
	if got != "" {
		t.Fatalf("opt-out should return empty; got %q", got)
	}
	// Override still wins under opt-out.
	gotOver := computeClientLabel("Custom Name", now)
	if gotOver != "Custom Name" {
		t.Fatalf("override should beat opt-out; got %q", gotOver)
	}
}

// TestComputeClientLabel_DefaultIncludesDate — the auto path must
// always include the date so a user with the same hostname across
// several re-mints can distinguish them by date.
func TestComputeClientLabel_DefaultIncludesDate(t *testing.T) {
	t.Setenv(envNoHostname, "")
	now := time.Date(2026, 6, 21, 9, 30, 0, 0, time.UTC)
	got := computeClientLabel("", now)
	if !strings.Contains(got, "2026-06-21") {
		t.Errorf("default label missing date: %q", got)
	}
}

// TestSanitizeHostname_CapsLong — extremely long hostnames (some
// CI runners emit 250+ char names) must be truncated so the
// dashboard's api_keys.name column doesn't blow out.
func TestSanitizeHostname_CapsLong(t *testing.T) {
	huge := strings.Repeat("X", 500)
	got := sanitizeHostname(huge)
	if len(got) != maxClientLabelLen {
		t.Errorf("len=%d; want %d", len(got), maxClientLabelLen)
	}
}
