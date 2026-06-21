package ui

import (
	"bytes"
	"strings"
	"testing"
)

// Levenshtein — sanity table. Tiny, but the algorithm is the
// load-bearing piece of Suggest's typo-tolerance, so locking the
// known-good values here protects against accidental regressions
// (off-by-one in the DP table, etc.) the renderer tests wouldn't
// catch.
func TestLevenshtein_KnownDistances(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"auht", "auth", 2},       // transposition costs 2 (delete + insert)
		{"lgin", "login", 1},      // single insertion
		{"workbo", "workbook", 2}, // two trailing chars
		{"runn", "run", 1},        // single trailing char
		{"a", "auth", 3},          // 1 → 4 chars
	}
	for _, tc := range cases {
		t.Run(tc.a+"_to_"+tc.b, func(t *testing.T) {
			if got := levenshtein(tc.a, tc.b); got != tc.want {
				t.Errorf("levenshtein(%q,%q) = %d; want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// Suggest — the actual matching behavior end users feel. Each case
// pairs a realistic typo + the moltable command vocabulary at the
// scope where that typo would land.
func TestSuggest_PrefixAndLevenshtein(t *testing.T) {
	root := []string{"auth", "workbook", "table", "column", "row", "run", "watch", "stop", "config", "profile", "skills", "version", "upgrade"}
	auth := []string{"login", "logout", "check"}

	cases := []struct {
		name       string
		typed      string
		candidates []string
		want       []string
	}{
		{
			// Single 'a' — alphabetical, only `auth` is the realistic
			// "did you mean". 2-char commands at distance ≤2 from "a"
			// would also match, but moltable has none.
			name:       "single-char-prefix",
			typed:      "a",
			candidates: root,
			want:       []string{"auth"},
		},
		{
			name:       "incomplete-prefix",
			typed:      "workbo",
			candidates: root,
			want:       []string{"workbook"},
		},
		{
			name:       "single-typo-trailing-double",
			typed:      "runn",
			candidates: root,
			want:       []string{"run"},
		},
		{
			// Subtree scope: the user typed "lgin" while inside the
			// auth noun. Should suggest auth's verbs, not root.
			name:       "subtree-typo",
			typed:      "lgin",
			candidates: auth,
			want:       []string{"login"},
		},
		{
			// Unknown enough to match nothing — empty result, not a
			// panic and not a noisy "all commands" dump.
			name:       "no-match",
			typed:      "qqqqqzzz",
			candidates: root,
			want:       nil,
		},
		{
			name:       "empty-input",
			typed:      "",
			candidates: root,
			want:       nil,
		},
		{
			// "AUT" lowercases to "aut". Prefix-matches `auth`; also
			// within Levenshtein distance 2 of `run` (a→r, u→u, t→n).
			// That extra noise is consistent with gh / cobra
			// distance-2 default — surfacing close-but-wrong matches
			// is the cost of catching real typos.
			name:       "case-insensitive-prefix",
			typed:      "AUT",
			candidates: root,
			want:       []string{"auth", "run"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Suggest(tc.typed, tc.candidates, 2)
			if !sliceEq(got, tc.want) {
				t.Errorf("Suggest(%q, ..., 2) = %v; want %v", tc.typed, got, tc.want)
			}
		})
	}
}

// UnknownCommand — render-shape contract. We don't pin the exact
// bytes (color escapes vary by terminal capability detection); we
// pin the SECTIONS so a reorder or accidental drop fails loudly.
func TestUnknownCommand_RenderShape(t *testing.T) {
	var buf bytes.Buffer
	UnknownCommand(&buf, "moltable", "workbo",
		[]string{"workbook"},
		[]string{"auth", "table", "workbook"})

	out := buf.String()

	mustContain := []string{
		`unknown command "workbo" for "moltable"`,
		"Did you mean this?",
		"workbook",
		"moltable <command> <subcommand> [flags]",
		"Available commands:",
	}
	for _, frag := range mustContain {
		if !strings.Contains(out, frag) {
			t.Errorf("UnknownCommand output missing %q in:\n%s", frag, out)
		}
	}

	// Section order: header → Did you mean → Usage → Available commands.
	// Any reorder breaks user expectation (this matches gh's order).
	order := []string{"unknown command", "Did you mean this?", "Usage:", "Available commands:"}
	last := -1
	for _, frag := range order {
		idx := strings.Index(out, frag)
		if idx < 0 {
			t.Errorf("missing %q in:\n%s", frag, out)
			continue
		}
		if idx <= last {
			t.Errorf("section out of order: %q at %d, prev was at %d in:\n%s",
				frag, idx, last, out)
		}
		last = idx
	}
}

// TestUnknownCommand_NoSuggestionsSkipsBlock — when Suggest returns
// no matches, the "Did you mean this?" header should NOT print
// (printing an empty list would be ugly and confusing).
func TestUnknownCommand_NoSuggestionsSkipsBlock(t *testing.T) {
	var buf bytes.Buffer
	UnknownCommand(&buf, "moltable", "qqqq",
		nil,
		[]string{"auth", "table"})

	out := buf.String()
	if strings.Contains(out, "Did you mean this?") {
		t.Errorf("no-suggestions output should omit the 'Did you mean this?' block:\n%s", out)
	}
	if !strings.Contains(out, "Available commands:") {
		t.Errorf("Available commands list must still render so user can scroll-pick:\n%s", out)
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
