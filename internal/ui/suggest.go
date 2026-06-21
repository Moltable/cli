package ui

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Suggest returns candidate commands a user probably meant when they
// typed an unknown token. Matches both prefix hits and short-edit
// typos:
//
//   - Any candidate that has `typed` as a case-insensitive prefix
//     (catches incomplete commands: "workbo" → "workbook")
//   - Any candidate within Levenshtein distance maxDist of `typed`
//     (catches transpositions / single-char drops: "auht" → "auth",
//     "lgin" → "login")
//
// Results are deduped, sorted alphabetically, and capped at 10
// entries so the "Did you mean this?" list stays scannable. gh uses
// the same cap.
//
// Pass maxDist = 2 for the canonical gh / cobra behavior. Higher
// values surface unrelated commands; lower values miss
// double-typos.
func Suggest(typed string, candidates []string, maxDist int) []string {
	if typed == "" || len(candidates) == 0 {
		return nil
	}
	low := strings.ToLower(typed)
	seen := map[string]bool{}
	var out []string
	for _, c := range candidates {
		lc := strings.ToLower(c)
		switch {
		case strings.HasPrefix(lc, low):
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		case levenshtein(low, lc) <= maxDist:
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

// levenshtein returns the edit distance between two ASCII-ish
// strings. Classic dynamic-programming implementation; not optimized
// for Unicode (CLI command names are ASCII). Allocates one (n+1)-wide
// row at a time to keep memory bounded for long inputs.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// UnknownCommand renders the gh-style "did you mean this?" screen
// to w. cliPath is the dotted path the user invoked (e.g. "moltable"
// for top-level, "moltable auth" for a subtree); typed is the bad
// token; suggestions is the result of Suggest(); allCommands is the
// full visible list at that scope (rendered under "Available
// commands" so the user can scroll-pick when no suggestion lands).
//
// Output goes to stderr by convention (the caller picks the writer).
// Colors are TTY-gated via the ui package's apply().
func UnknownCommand(w io.Writer, cliPath, typed string, suggestions, allCommands []string) {
	// Header: "unknown command "X" for "moltable [scope]"".
	// gh uses exactly this phrasing; mirroring it keeps muscle
	// memory portable for users who already use gh.
	fmt.Fprintf(w, "%s unknown command %q for %q\n",
		Error("error:"),
		typed,
		cliPath,
	)
	fmt.Fprintln(w)

	if len(suggestions) > 0 {
		fmt.Fprintln(w, Bold("Did you mean this?"))
		for _, s := range suggestions {
			fmt.Fprintf(w, "    %s\n", Accent(s))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "%s %s <command> <subcommand> [flags]\n\n",
		Bold("Usage:"),
		cliPath,
	)

	if len(allCommands) > 0 {
		fmt.Fprintln(w, Bold("Available commands:"))
		for _, c := range allCommands {
			fmt.Fprintf(w, "  %s\n", c)
		}
	}
}
