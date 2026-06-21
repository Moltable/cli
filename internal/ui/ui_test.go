package ui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// withTTYProbe swaps the spinner's TTY probe for the duration of a
// test. Mirrors how output.ResetForTest works in the sibling package.
func withTTYProbe(t *testing.T, isTTY bool) {
	t.Helper()
	prev := ttyProbe
	ttyProbe = func() bool { return isTTY }
	t.Cleanup(func() { ttyProbe = prev })
}

// ──────────────────────────────────────────────────────────────────────
// Style helpers — apply() is the single TTY gate; verify both branches.
// ──────────────────────────────────────────────────────────────────────

// TestStyles_PlainWhenNotTTY locks in the contract that non-interactive
// callers (CI, pipes, NO_COLOR users) see exactly the input string with
// no ANSI escape sequences. Without this test, any future change to
// lipgloss internals or the apply() gate could silently leak escapes
// into log files.
func TestStyles_PlainWhenNotTTY(t *testing.T) {
	// IsTTY is governed by the output package's sync.Once cache, and in
	// the test binary stdout isn't a terminal, so apply() falls into the
	// plain branch naturally. Just verify the rendered string IS the
	// input string when that's the case.
	cases := []struct {
		name string
		fn   func(string) string
		in   string
	}{
		{"Success", Success, "ok"},
		{"Error", Error, "boom"},
		{"Warn", Warn, "yellow"},
		{"Hint", Hint, "try X"},
		{"Accent", Accent, "id_123"},
		{"Bold", Bold, "emphasis"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.fn(tc.in)
			if got != tc.in {
				t.Fatalf("non-TTY %s(%q) = %q; want exactly %q (no ANSI escapes)",
					tc.name, tc.in, got, tc.in)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────
// ProgressBar — shape contract must survive the styling refactor.
// ──────────────────────────────────────────────────────────────────────

// TestProgressBar_ShapeContract — the watch loop and any future
// scripts depend on (a) the leading \r so the line overwrites, (b)
// the visible done/total counts, and (c) the jobID being in the
// output somewhere. Lock those in; the surrounding bytes can change.
func TestProgressBar_ShapeContract(t *testing.T) {
	got := ProgressBar(7, 10, "job_X")
	if !strings.HasPrefix(got, "\r") {
		t.Errorf("ProgressBar must start with \\r so it overwrites; got %q", got)
	}
	if !strings.Contains(got, "7/10 cells") {
		t.Errorf("ProgressBar missing 'N/M cells' segment; got %q", got)
	}
	if !strings.Contains(got, "job_X") {
		t.Errorf("ProgressBar missing job ID; got %q", got)
	}
}

// TestProgressBar_OvershootClamps — done > total can happen when
// late-arriving cell:update events race the column:progress totals.
// The bar must clamp at 100% rather than printing more cells than
// the constant width allows.
func TestProgressBar_OvershootClamps(t *testing.T) {
	// 150% — done exceeds total. Must not panic, must produce a
	// well-shaped line.
	got := ProgressBar(15, 10, "job_X")
	if !strings.Contains(got, "15/10 cells") {
		t.Errorf("counts pass through verbatim even when overshooting; got %q", got)
	}
}

// TestProgressBar_ZeroTotal — pre-first-event state: total is 0, done
// is 0. Must not divide by zero, must produce a parseable empty bar.
func TestProgressBar_ZeroTotal(t *testing.T) {
	got := ProgressBar(0, 0, "job_X")
	if !strings.Contains(got, "0/0 cells") {
		t.Errorf("zero/zero must render counts cleanly; got %q", got)
	}
}

// ──────────────────────────────────────────────────────────────────────
// Spinner — gating + lifecycle + no-leaks.
// ──────────────────────────────────────────────────────────────────────

// TestSpinner_NonTTYIsNoop — when stderr isn't a terminal, Start
// MUST NOT spawn the goroutine and MUST NOT write to the buffer.
// This is the contract that lets CI runs of `moltable auth login`
// stay clean — no animated noise in build logs.
func TestSpinner_NonTTYIsNoop(t *testing.T) {
	withTTYProbe(t, false)

	var buf bytes.Buffer
	s := NewSpinner(&buf, "should not appear")
	s.Start()
	// Wait longer than several frames would take if it WAS running.
	time.Sleep(300 * time.Millisecond)
	s.Stop()

	if buf.Len() > 0 {
		t.Fatalf("non-TTY spinner wrote to stderr: %q", buf.String())
	}
}

// TestSpinner_TTYWritesAndErases — when stderr IS a terminal, the
// spinner emits frames during its lifetime AND erases them on Stop
// so the next caller print starts at column 0. The erase rule
// matters because the watch flow prints status lines after auth
// completes; an un-erased spinner would leave garbage in front of
// "Logged in as ...".
func TestSpinner_TTYWritesAndErases(t *testing.T) {
	withTTYProbe(t, true)

	var buf bytes.Buffer
	mu := &sync.Mutex{} // bytes.Buffer is not concurrent-safe by itself
	w := &lockedWriter{w: &buf, mu: mu}

	s := NewSpinner(w, "Working...")
	// Speed up the spinner so the test runs fast.
	s.frameMs = 10 * time.Millisecond
	s.Start()
	time.Sleep(60 * time.Millisecond) // ~6 frames
	s.Stop()

	mu.Lock()
	defer mu.Unlock()
	out := buf.String()

	if !strings.Contains(out, "Working...") {
		t.Errorf("spinner did not emit label; got %q", out)
	}
	// The final write erases — the buffer must end with \r so the
	// next caller's print overwrites the cleared line.
	if !strings.HasSuffix(out, "\r") {
		t.Errorf("spinner final write must end in \\r (erase); got %q", out)
	}
}

// TestSpinner_StopIsIdempotent — defer s.Stop() patterns mean Stop
// can fire from multiple paths (success defer + error early-return).
// Calling Stop twice must not panic or close a closed channel.
func TestSpinner_StopIsIdempotent(t *testing.T) {
	withTTYProbe(t, true)

	var buf bytes.Buffer
	s := NewSpinner(&buf, "test")
	s.frameMs = 5 * time.Millisecond
	s.Start()
	s.Stop()
	s.Stop() // must not panic
	s.Stop() // still must not panic
}

// lockedWriter wraps a bytes.Buffer with a mutex so concurrent writes
// from the spinner goroutine + test goroutine don't trip the race
// detector. Plain bytes.Buffer is documented non-safe for concurrent
// use.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}
