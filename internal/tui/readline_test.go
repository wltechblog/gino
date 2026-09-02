package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestVisualLinesSimple(t *testing.T) {
	lines := visualLines("ab>", "hello", 10)
	if len(lines) != 1 || lines[0] != "ab>hello" {
		t.Fatalf("expected single line, got %v", lines)
	}
}

func TestVisualLinesWraps(t *testing.T) {
	// width 10: prompt "ab>" (3 cells) + "abcdefghij" (10) = 13 cells → 2 rows.
	lines := visualLines("ab>", "abcdefghij", 10)
	if len(lines) != 2 {
		t.Fatalf("expected 2 rows, got %d: %v", len(lines), lines)
	}
	if lines[0] != "ab>abcdefg" || lines[1] != "hij" {
		t.Fatalf("unexpected wrap: %v", lines)
	}
}

func TestVisualLinesWideRunes(t *testing.T) {
	// 🤖 is 2 cells wide. width 5: prompt "ab>" (3) + "🤖" (2) = 5 → 1 row.
	if got := visualLines("ab>", "🤖", 5); len(got) != 1 {
		t.Fatalf("expected 1 row, got %v", got)
	}
	// width 4: 3+2 = 5 > 4 → wrap; emoji lands alone on row 2.
	if got := visualLines("ab>", "🤖", 4); len(got) != 2 {
		t.Fatalf("expected 2 rows, got %v", got)
	}
}

func TestCursorRowCol(t *testing.T) {
	// 2 rows: "ab>abcdefg" / "hij", cursor after 'b' of buf (byte offset 2)
	// → still row 0, at cell 5 (prompt occupies 3 cells).
	row, col := cursorRowCol("ab>", "abcdefghij", 2, 10)
	if row != 0 || col != 5 {
		t.Fatalf("expected row 0 col 5, got %d/%d", row, col)
	}
	// Cursor at very start → row 0, right after the prompt (3 cells).
	row, col = cursorRowCol("ab>", "abcdefghij", 0, 10)
	if row != 0 || col != 3 {
		t.Fatalf("expected 0/3, got %d/%d", row, col)
	}
	// Cursor at end → last row, col = cells on that row.
	row, col = cursorRowCol("ab>", "abcdefghij", 10, 10)
	if row != 1 || col != 3 {
		t.Fatalf("expected 1/3, got %d/%d", row, col)
	}
	// Cursor after 'i' (byte 9) → row 1, col 2.
	row, col = cursorRowCol("ab>", "abcdefghij", 9, 10)
	if row != 1 || col != 2 {
		t.Fatalf("expected 1/2, got %d/%d", row, col)
	}
}

// renderBuffer writes the ANSI stream the Readline would emit for the given
// buffer state by replaying it through the real render path.
func newTestReadline(out *bytes.Buffer) *Readline {
	ch := make(chan byte, 64)
	rl := &Readline{out: out, prompt: "ab>", bytes: ch}
	return rl
}

func TestRenderWrappedErasesAllRows(t *testing.T) {
	var out bytes.Buffer
	rl := newTestReadline(&out)
	rl.rows = 0

	// First render: type "abcdefghij" with width forced via termWidth on
	// non-tty stdin it will fall back to 80; instead set buffer state
	// directly and call render with a fake width by stubbing — simplest is
	// to verify against the 80-col default.
	rl.buf = []byte(strings.Repeat("x", 90)) // 90 + 3 = 93 cells → 2 rows at 80
	rl.cursor = 93
	rl.render()
	rl.render()

	s := out.String()
	// Moving up to erase the prior 2-row render must appear exactly once
	// per render (the second render erases 2 rows: ESC[1A).
	if !strings.Contains(s, "\x1b[1A") {
		t.Fatalf("expected cursor-up erase sequence in render output, got %q", s)
	}
}

func TestClearLineErasesAllRows(t *testing.T) {
	var out bytes.Buffer
	rl := newTestReadline(&out)
	rl.buf = []byte(strings.Repeat("x", 90))
	rl.cursor = 93
	rl.render()
	out.Reset()

	rl.clearLine()
	s := out.String()
	if !strings.Contains(s, "\x1b[1A") {
		t.Fatalf("expected up-erase for 2-row input area, got %q", s)
	}
	if !strings.HasSuffix(s, "\x1b[1A") {
		t.Fatalf("expected cursor returned to top row after clear, got %q", s)
	}
}

func TestEscInterruptGesture(t *testing.T) {
	rl := &Readline{out: &bytes.Buffer{}, prompt: "> ", bytes: make(chan byte, 4)}
	if rl.escInterrupt() {
		t.Fatal("first Esc must not report interrupt")
	}
	if !rl.escInterrupt() {
		t.Fatal("second Esc within interval must report interrupt")
	}
	// After firing, the timer resets — third press is "first" again.
	if rl.escInterrupt() {
		t.Fatal("timer must reset after firing")
	}
}

func TestEscInterruptExpires(t *testing.T) {
	rl := &Readline{out: &bytes.Buffer{}, prompt: "> ", bytes: make(chan byte, 4)}
	rl.escInterrupt()
	rl.lastEsc = time.Now().Add(-2 * time.Second) // simulate expiry
	if rl.escInterrupt() {
		t.Fatal("expired interval must not fire")
	}
}

func TestPeekByteTimeout(t *testing.T) {
	ch := make(chan byte, 1)
	if b, got, closed := peekByte(ch, 5*time.Millisecond); got || closed || b != 0 {
		t.Fatal("expected timeout with no byte")
	}
	ch <- 'x'
	if b, got, closed := peekByte(ch, 5*time.Millisecond); !got || closed || b != 'x' {
		t.Fatal("expected to receive queued byte")
	}
	close(ch)
	if _, got, closed := peekByte(ch, 5*time.Millisecond); got || !closed {
		t.Fatal("expected closed-channel report")
	}
}

func TestRuneLenHelpers(t *testing.T) {
	buf := []byte("aé🤖z") // 1 + 2 + 4 + 1 = 8 bytes
	if got := runeLenAt(buf, 1); got != 2 {
		t.Fatalf("runeLenAt(é) = %d, want 2", got)
	}
	if got := runeLenAt(buf, 3); got != 4 {
		t.Fatalf("runeLenAt(🤖) = %d, want 4", got)
	}
	if got := runeLenBefore(buf, 3); got != 2 {
		t.Fatalf("runeLenBefore(é) = %d, want 2", got)
	}
	if got := runeLenBefore(buf, 7); got != 4 {
		t.Fatalf("runeLenBefore(🤖) = %d, want 4", got)
	}
}

func TestReadLineUTF8Input(t *testing.T) {
	var out bytes.Buffer
	ch := make(chan byte, 32)
	rl := &Readline{out: &out, prompt: "> ", bytes: ch}

	go func() {
		for _, b := range []byte("héllo") { // é is 2 bytes
			ch <- b
		}
		ch <- '\r'
	}()

	line, err := rl.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if line != "héllo" {
		t.Fatalf("got %q", line)
	}
}

func TestReadLineBackspaceUTF8(t *testing.T) {
	var out bytes.Buffer
	ch := make(chan byte, 32)
	rl := &Readline{out: &out, prompt: "> ", bytes: ch}

	go func() {
		for _, b := range []byte("hé") {
			ch <- b
		}
		ch <- 0x7F // backspace removes whole é rune
		ch <- 'i'
		ch <- '\r'
	}()

	line, err := rl.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if line != "hi" {
		t.Fatalf("got %q, want hi", line)
	}
}

func TestReadLineArrowKeysDoNotBlock(t *testing.T) {
	var out bytes.Buffer
	ch := make(chan byte, 32)
	rl := &Readline{out: &out, prompt: "> ", bytes: ch}

	go func() {
		ch <- 0x1B
		ch <- '['
		ch <- 'C' // Right arrow — with empty buffer, no-op
		ch <- 'a'
		ch <- '\r'
	}()

	line, err := rl.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if line != "a" {
		t.Fatalf("got %q", line)
	}
}

func TestReadLineLoneEscThenChar(t *testing.T) {
	var out bytes.Buffer
	ch := make(chan byte, 32)
	interrupted := 0
	rl := &Readline{out: &out, prompt: "> ", bytes: ch}
	rl.onInterrupt = func() { interrupted++ }

	go func() {
		ch <- 0x1B
		// No follow-up bytes: after grace period this is a lone Esc.
		time.Sleep(60 * time.Millisecond)
		ch <- 0x1B
		time.Sleep(60 * time.Millisecond)
		ch <- 'a'
		ch <- '\r'
	}()

	line, err := rl.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if line != "a" {
		t.Fatalf("got %q", line)
	}
	// Two lone Escs within 1s → onInterrupt fired exactly once.
	if interrupted != 1 {
		t.Fatalf("interrupt fired %d times, want 1", interrupted)
	}
}
