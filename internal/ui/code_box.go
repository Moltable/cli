package ui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// codeBoxBorder is a thick rounded border applied to the prompt
// `moltable auth login` paints around the handoff code. Picked over
// the default thin border for readability: the user is meant to
// SEE the code, type it into the browser, and not confuse it with
// surrounding log lines.
var codeBoxBorder = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(accentColor).
	Padding(0, 3)

// codeInBox styles the code itself — bold + accent + wide letter
// spacing so a 9-character XXX-XXX-XXX renders large enough to read
// across a room (or, more realistically, to read once and type
// without backtracking).
var codeInBox = lipgloss.NewStyle().
	Foreground(accentColor).
	Bold(true)

// CodeBox renders a prominent display block for a short login code.
// Shape (TTY mode):
//
//	    ┌──────────────────────┐
//	    │   CJG-X72-1T4        │
//	    └──────────────────────┘
//
// In non-TTY mode (CI, pipes, NO_COLOR) it falls back to plain
// text so log scrapers don't choke on the box-drawing characters:
//
//	  Code: CJG-X72-1T4
//
// Pair with a directly-above instruction line like "Type this code
// in your browser when prompted:" so users know what to do with it.
//
// The writer is passed in (rather than always going to os.Stderr)
// so tests can capture the output by injecting a bytes.Buffer.
func CodeBox(w io.Writer, code string) {
	if !IsTTYEligible() {
		fmt.Fprintf(w, "  Code: %s\n", code)
		return
	}
	rendered := codeBoxBorder.Render(codeInBox.Render(code))
	// Indent the box two spaces so it visually nests under whatever
	// instruction line the caller printed above it.
	indented := indentLines(rendered, "  ")
	fmt.Fprintln(w, indented)
}

// indentLines prefixes every line of s with prefix. lipgloss's
// border rendering is multi-line so a single Fprintf with "  %s"
// only indents the first line; we walk the lines explicitly.
func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
