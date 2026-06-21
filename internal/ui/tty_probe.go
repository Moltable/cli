package ui

import "github.com/moltable/cli/internal/output"

// init wires the spinner's ttyProbe to the canonical output.IsTTY()
// function. Lives in a separate file so spinner_test.go can swap
// ttyProbe without dragging the output package into the test set.
func init() {
	ttyProbe = output.IsTTY
}
