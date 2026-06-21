// Package output formats data the CLI prints to stdout/stderr.
//
// This file specifically implements TTY detection: "is stdout
// connected to a real terminal and is the user OK with color
// output?" Almost every command consults IsTTY() to decide between
// human-readable formatting (tables, colors, spinners) and the
// machine-readable JSON path.
//
// Detection rule (all must be true):
//
//   - stdout is a terminal (golang.org/x/term.IsTerminal)
//   - $NO_COLOR is unset (de-facto cross-tool standard, see no-color.org)
//   - $MOLTABLE_NO_COLOR is unset (moltable-specific escape hatch)
//   - $CI is unset (CI runners advertise this; their "TTYs" are pty
//     emulations but no human watches them)
//
// The result is cached at process start via sync.Once so repeated
// IsTTY() calls are zero-cost and consistent within a single run. For
// tests, ResetForTest() forces a re-evaluation under whatever env you
// set; production code never touches it.
package output

import (
	"os"
	"sync"

	"golang.org/x/term"
)

const (
	envNoColor         = "NO_COLOR"
	envMoltableNoColor = "MOLTABLE_NO_COLOR"
	envCI              = "CI"
)

var (
	ttyOnce   sync.Once
	ttyCached bool

	// ttyProbe is the function used to ask "is this fd a terminal?".
	// It defaults to the x/term implementation but is swappable in
	// tests so we never depend on the actual test runner's tty state.
	ttyProbe = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
)

// IsTTY reports whether stdout is suitable for interactive,
// color-capable output. The result is computed once at first call and
// cached for the life of the process.
//
// Returns true only when ALL of these hold:
//
//   - stdout is a terminal
//   - NO_COLOR is unset
//   - MOLTABLE_NO_COLOR is unset
//   - CI is unset
func IsTTY() bool {
	ttyOnce.Do(func() {
		ttyCached = computeIsTTY()
	})
	return ttyCached
}

// computeIsTTY is the uncached evaluation — extracted so tests can
// call it via ResetForTest without depending on internal state.
func computeIsTTY() bool {
	if os.Getenv(envNoColor) != "" {
		return false
	}
	if os.Getenv(envMoltableNoColor) != "" {
		return false
	}
	if os.Getenv(envCI) != "" {
		return false
	}
	return ttyProbe()
}

// ResetForTest clears the cached IsTTY value AND lets a test inject
// a custom probe function. It is intended ONLY for use from _test.go
// files in this module; production code should never call it.
//
// Pass nil for probe to keep the default (real terminal check).
//
// Usage:
//
//	defer output.ResetForTest(nil)
//	output.ResetForTest(func() bool { return true })  // simulate TTY
//	t.Setenv("NO_COLOR", "1")
//	if output.IsTTY() { t.Fatal("expected non-TTY under NO_COLOR") }
func ResetForTest(probe func() bool) {
	ttyOnce = sync.Once{}
	ttyCached = false
	if probe == nil {
		ttyProbe = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
	} else {
		ttyProbe = probe
	}
}
