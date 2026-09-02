package tui

import (
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// termWidth returns the current terminal width in columns, or 80 if it
// cannot be determined. fd should be the terminal file descriptor.
func termWidth(fd int) int {
	type winsize struct {
		Row, Col, Xpixel, Ypixel uint16
	}
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}

// visualLines splits prompt+buffer into the visual rows the terminal will
// wrap them onto, given the terminal width. Backspace-friendly: it works on
// display cells, not bytes, so UTF-8 and wide runes are handled.
func visualLines(prompt, buf string, width int) []string {
	if width < 4 {
		width = 4
	}
	full := prompt + buf
	cells := 0
	lines := []string{}
	cur := &strings.Builder{}
	inEsc := false
	for _, r := range full {
		// ANSI escape sequences occupy no display cells; pass them through
		// without affecting the wrap computation.
		if inEsc {
			cur.WriteRune(r)
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			cur.WriteRune(r)
			inEsc = true
			continue
		}
		rw := runeDisplayWidth(r)
		if cells+rw > width {
			lines = append(lines, cur.String())
			cur.Reset()
			cells = 0
		}
		cur.WriteRune(r)
		cells += rw
	}
	lines = append(lines, cur.String())
	return lines
}

// visibleCells returns the number of display cells a string occupies,
// ignoring ANSI escape sequences (which take no space on screen).
func visibleCells(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		n += runeDisplayWidth(r)
	}
	return n
}

// cursorRowCol computes the 0-based visual row and cell-column of the cursor
// given the cursor's byte offset within buf (buffer-relative, NOT including
// the prompt). ANSI escape sequences in the prompt occupy no display cells.
func cursorRowCol(prompt, buf string, cursorBytes, width int) (row, col int) {
	if width < 4 {
		width = 4
	}
	cells := 0
	row = 0
	consumed := 0 // bytes consumed of prompt+buf
	inEsc := false
	for _, r := range prompt + buf {
		if consumed >= len(prompt) && consumed-len(prompt) >= cursorBytes {
			break
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			consumed += len(string(r))
			continue
		}
		if r == '\x1b' {
			inEsc = true
			consumed += len(string(r))
			continue
		}
		rw := runeDisplayWidth(r)
		if cells+rw > width {
			row++
			cells = 0
		}
		cells += rw
		consumed += len(string(r))
	}
	return row, cells
}

// peekByte waits up to d for the next byte. It returns (b, true, false)
// when a byte arrived (and is consumed), (0, false, false) on timeout, and
// (0, false, true) when the channel is closed.
func peekByte(ch <-chan byte, d time.Duration) (byte, bool, bool) {
	select {
	case b, ok := <-ch:
		if !ok {
			return 0, false, true
		}
		return b, true, false
	case <-time.After(d):
		return 0, false, false
	}
}

// runeLenAt returns the byte length of the UTF-8 rune starting at i in buf.
func runeLenAt(buf []byte, i int) int {
	if i >= len(buf) {
		return 1
	}
	c := buf[i]
	n := 1
	switch {
	case c >= 0xF0:
		n = 4
	case c >= 0xE0:
		n = 3
	case c >= 0xC0:
		n = 2
	}
	if i+n > len(buf) {
		return len(buf) - i
	}
	return n
}

// runeLenBefore returns the byte length of the UTF-8 rune ending at i in buf.
func runeLenBefore(buf []byte, i int) int {
	if i <= 0 {
		return 1
	}
	start := i - 1
	for start > 0 && buf[start]&0xC0 == 0x80 {
		start--
	}
	return i - start
}

// handlePrintable inserts a printable byte at the cursor and re-renders.
func (rl *Readline) handlePrintable(c byte) {
	rl.buf = append(rl.buf[:rl.cursor], append([]byte{c}, rl.buf[rl.cursor:]...)...)
	rl.cursor++
	rl.render()
}
