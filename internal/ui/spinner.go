package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Spinner is a minimal stderr spinner that renders to an io.Writer
// while a background goroutine ticks the animation frame. Use case:
// the `moltable auth login` poll loop (60-second-ish wait while the
// user clicks Approve in the browser) where we want SOME feedback so
// the terminal doesn't look frozen.
//
// We deliberately don't pull in bubbletea here. A spinner inside a
// bubbletea Program would require a full Update/View loop and a
// Quit message; for non-interactive "just animate something until
// I tell you to stop," a ticker + \r-rewrite is 30 lines and ships
// the same UX.
//
// Lifecycle:
//
//	s := ui.NewSpinner(os.Stderr, "Waiting for browser approval...")
//	s.Start()
//	defer s.Stop()
//	... blocking work ...
//
// Stop is idempotent. Start is not — calling it twice spawns two
// goroutines that fight over stderr. Don't.
type Spinner struct {
	w     io.Writer
	label string

	// Frames + frameMs control the animation. Defaults to braille
	// dots at ~12 fps, the same cadence other Go CLIs converge on
	// (gh, doctl, brew).
	frames  []string
	frameMs time.Duration

	mu      sync.Mutex
	done    chan struct{}
	stopped bool
}

// defaultFrames is the canonical braille spinner — readable in every
// modern terminal (Terminal.app, iTerm, Alacritty, WezTerm, etc.).
var defaultFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// NewSpinner constructs a Spinner that will render to w. label is the
// text shown to the right of the animated frame.
func NewSpinner(w io.Writer, label string) *Spinner {
	return &Spinner{
		w:       w,
		label:   label,
		frames:  defaultFrames,
		frameMs: 80 * time.Millisecond,
	}
}

// Start launches the animation goroutine. No-op (and renders nothing)
// when output is not a TTY — callers can `defer s.Stop()` either way
// without worrying about double-cleaning a no-op spinner.
func (s *Spinner) Start() {
	if !IsTTYEligible() {
		return
	}
	s.mu.Lock()
	s.done = make(chan struct{})
	s.stopped = false
	s.mu.Unlock()

	go s.run()
}

// Stop signals the goroutine to exit, waits for it to finish the
// current frame, and erases the spinner line so the next print from
// the caller starts at a clean column. Idempotent.
func (s *Spinner) Stop() {
	s.mu.Lock()
	if s.stopped || s.done == nil {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.done)
	doneCh := s.done
	s.mu.Unlock()

	// The goroutine erases on exit. We don't strictly need to wait
	// for it, but if the caller immediately prints something after
	// Stop() the cursor might land mid-erase. Give it one frame's
	// time-to-finish before returning.
	_ = doneCh
	time.Sleep(s.frameMs)
}

func (s *Spinner) run() {
	t := time.NewTicker(s.frameMs)
	defer t.Stop()
	i := 0
	for {
		select {
		case <-s.done:
			// Erase the spinner line. \r + spaces wider than any
			// frame+label combo, then \r again so the cursor is at
			// column 0 ready for the caller's next print.
			erase := strings.Repeat(" ", len(s.label)+8)
			fmt.Fprintf(s.w, "\r%s\r", erase)
			return
		case <-t.C:
			frame := s.frames[i%len(s.frames)]
			i++
			// Render: \r + accented frame + space + label. apply()
			// returns plain when !TTY but we already early-returned
			// from Start() in that case.
			fmt.Fprintf(s.w, "\r%s %s", Accent(frame), s.label)
		}
	}
}

// IsTTYEligible is exported so call sites can early-skip spinner setup
// in non-TTY contexts (saves the goroutine spawn and the immediate
// no-op return). Mirrors the apply() gate so behavior stays consistent
// across all rendering paths.
func IsTTYEligible() bool {
	// Delegate to the package-private apply path: we can't import
	// output here without a cycle, so we re-derive via Render of
	// an empty string — lipgloss returns "" both when styled and not.
	// Instead, inspect via output package using a tiny helper.
	return ttyProbe()
}

// ttyProbe is swapped in tests. In production it calls output.IsTTY()
// indirectly via the apply() gate. See init() in tty_probe.go.
var ttyProbe = func() bool { return false }
