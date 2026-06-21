package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ProgressBarWidth is the fixed character width of the bar. We DON'T
// stretch to terminal width — keeps the look identical in 80-col
// terminals and 200-col ones, which the original ASCII bar deliberately
// chose. Easier to scan in mixed window setups.
const ProgressBarWidth = 24

var (
	progressFilledStyle = lipgloss.NewStyle().Foreground(accentColor)
	progressEmptyStyle  = lipgloss.NewStyle().Foreground(hintColor)
)

// ProgressBar renders a left-justified bar with done/total counts and
// the job ID, prefixed with \r so successive calls overwrite the same
// line (the watch loop pattern).
//
// Colors are gated by the same TTY check the Style helpers use: when
// !TTY, we emit plain ASCII (same shape the original formatProgressBar
// did so non-interactive logs stay parseable).
func ProgressBar(done, total int, jobID string) string {
	filled := 0
	if total > 0 {
		filled = (done * ProgressBarWidth) / total
	}
	if filled > ProgressBarWidth {
		filled = ProgressBarWidth
	}
	filledStr := strings.Repeat("█", filled)
	emptyStr := strings.Repeat("░", ProgressBarWidth-filled)

	// apply() does the TTY gate; styles are no-ops in plain mode.
	bar := apply(progressFilledStyle, filledStr) + apply(progressEmptyStyle, emptyStr)
	return fmt.Sprintf("\r[%s] %d/%d cells (%s)", bar, done, total, Accent(jobID))
}
