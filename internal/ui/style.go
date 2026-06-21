// Package ui owns the CLI's terminal rendering — colors, styled
// progress bars, spinners. Everything here is gated on
// output.IsTTY() so non-interactive callers (CI, pipes, `--json`
// consumers) get plain ASCII with no ANSI noise.
//
// We use lipgloss for color/style primitives but explicitly do NOT
// pull in bubbletea for spinners/progress — those would force a full
// TUI Program lifecycle for what's a one-shot ticker. Hand-rolled
// helpers stay simpler and lighter.
//
// Color palette is conservative (8 ANSI base colors, no 256-color or
// truecolor). Keeps the binary smaller and renders well in every
// terminal including ssh-into-tmux scenarios where truecolor can
// degrade unpredictably.
package ui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/moltable/cli/internal/output"
)

// Color tokens. ANSI 16-color palette so colors render consistently
// across light + dark themes without per-terminal tuning.
var (
	successColor = lipgloss.Color("2")  // green
	errorColor   = lipgloss.Color("1")  // red
	warnColor    = lipgloss.Color("3")  // yellow
	hintColor    = lipgloss.Color("8")  // bright black / dim
	accentColor  = lipgloss.Color("6")  // cyan — used for codes, IDs, keys
)

// Pre-built styles. lipgloss.Style is cheap to copy; we hand back the
// rendered string from the helpers below rather than the Style itself
// so call sites stay one-liners.
var (
	successStyle = lipgloss.NewStyle().Foreground(successColor).Bold(true)
	errorStyle   = lipgloss.NewStyle().Foreground(errorColor).Bold(true)
	warnStyle    = lipgloss.NewStyle().Foreground(warnColor).Bold(true)
	hintStyle    = lipgloss.NewStyle().Foreground(hintColor)
	accentStyle  = lipgloss.NewStyle().Foreground(accentColor)
	boldStyle    = lipgloss.NewStyle().Bold(true)
)

// Success renders s in green bold when stderr is a TTY, plain
// otherwise. Used for "Logged in as ...", "Job succeeded", etc.
func Success(s string) string { return apply(successStyle, s) }

// Error renders s in red bold. Used for the "moltable: <msg>" prefix
// in the central error printer.
func Error(s string) string { return apply(errorStyle, s) }

// Warn renders s in yellow bold. Used for the upgrade nudge + the
// "your key is still active" logout warning.
func Warn(s string) string { return apply(warnStyle, s) }

// Hint renders s in dim. Used for the "hint: ..." line under errors.
func Hint(s string) string { return apply(hintStyle, s) }

// Accent renders s in cyan. Used for inline IDs, handoff codes, and
// other "this is the thing you care about in the line" values.
func Accent(s string) string { return apply(accentStyle, s) }

// Bold renders s in bold without color — used for emphasizing
// keywords in otherwise-plain status lines.
func Bold(s string) string { return apply(boldStyle, s) }

// apply runs the style through lipgloss when output is TTY-eligible;
// otherwise returns s unchanged. The TTY check uses output.IsTTY()
// (same gate that controls JSON-vs-human formatting) so a user that
// sets MOLTABLE_NO_COLOR or NO_COLOR gets plain output everywhere
// consistently.
func apply(style lipgloss.Style, s string) string {
	if !output.IsTTY() {
		return s
	}
	return style.Render(s)
}
