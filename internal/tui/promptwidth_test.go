package tui

import (
	"testing"
)

// TestCursorOffsetWithColoredPrompt verifies that cursor positioning is not
// skewed by ANSI escape sequences embedded in the prompt (which occupy zero
// display cells but were previously counted as one cell each).
func TestCursorOffsetWithColoredPrompt(t *testing.T) {
	prompt := "\033[36m\033[1myou\033[0m ❯ " // cyan+bold "you", reset, " ❯ "
	// Visible prompt text: "you ❯ " = 6 cells.
	want := 6
	if got := visibleCells(prompt); got != want {
		t.Errorf("visibleCells(prompt) = %d, want %d", got, want)
	}

	// Cursor at end of a 10-char input on an 80-col terminal.
	row, col := cursorRowCol(prompt, "helloworld", 10, 80)
	if row != 0 || col != 16 {
		t.Errorf("cursorRowCol = (%d, %d), want (0, 16)", row, col)
	}
}
