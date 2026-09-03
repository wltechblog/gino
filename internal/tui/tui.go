// Package tui implements a terminal-based chat interface for Gino.
// It uses the same hub/agent-loop infrastructure as the gateway, so the
// interactive CLI session has full tool access, memory, and session continuity.
package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/wltechblog/gino/internal/agent"
	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// ANSI color codes.
const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	dim     = "\033[2m"
	red     = "\033[31m"
	green   = "\033[32m"
	yellow  = "\033[33m"
	blue    = "\033[34m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
	gray    = "\033[90m"
)

// wprint writes to w and discards the error (terminal writes that fail
// are not actionable).
func wprint(w io.Writer, format string, args ...interface{}) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// wprintln writes a line to w, discarding the error.
func wprintln(w io.Writer) {
	_, _ = fmt.Fprintln(w)
}

// ─── termios helpers ────────────────────────────────────────────

type termios struct {
	IFlag  uint32
	OFlag  uint32
	CFlag  uint32
	LFlag  uint32
	Cc     [19]byte
	Ispeed uint32
	Ospeed uint32
}

const (
	// ioctl numbers (Linux).
	TCGETS = 0x5401
	TCSETS = 0x5402
	// termios local flags.
	ECHO   = 0x00000008
	ICANON = 0x00000002
	IEXTEN = 0x00008000
	ISIG   = 0x00000001
	// input flags.
	IXON  = 0x00000400
	ICRNL = 0x00000100
)

func makeRaw(fd int) (*termios, error) {
	var orig termios
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), TCGETS, uintptr(unsafe.Pointer(&orig))); err != 0 {
		return nil, err
	}
	raw := orig
	raw.LFlag &^= ECHO | ICANON | IEXTEN | ISIG
	raw.IFlag &^= IXON | ICRNL
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), TCSETS, uintptr(unsafe.Pointer(&raw))); err != 0 {
		return nil, err
	}
	return &orig, nil
}

func restoreTerm(fd int, t *termios) {
	if t == nil {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), TCSETS, uintptr(unsafe.Pointer(t)))
}

// ─── Readline ───────────────────────────────────────────────────

// Readline provides raw-mode line editing with command history.
// It reads bytes from a channel (fed by a single stdin-owning goroutine)
// to avoid competing reads.
type Readline struct {
	out     io.Writer
	prompt  string
	history []string
	histIdx int // index into history for navigation; len(history) = "new entry"

	// Current buffer state.
	buf    []byte
	cursor int // byte offset within buf

	// Wrapping state. rows is how many visual rows the last render of the
	// input area occupies; cursorRow is the visual row the cursor was left
	// on. cleared means the input area has already been erased (e.g. by
	// clearLine) and the physical cursor sits at its top-left, so the next
	// render must not issue clear sequences.
	rows      int
	cursorRow int
	cleared   bool

	// lastEsc records when a standalone Esc key was last seen, for the
	// double-Esc interrupt gesture.
	lastEsc time.Time

	// onInterrupt, when non-nil, is invoked when the double-Esc interrupt
	// gesture fires while a line is being edited.
	onInterrupt func()

	// Input source.
	bytes <-chan byte
}

// NewReadline creates a readline instance. The terminal must already be in
// raw mode. It reads from the provided byte channel.
func NewReadline(out io.Writer, prompt string, bytes <-chan byte) *Readline {
	rl := &Readline{
		out:    out,
		prompt: prompt,
		bytes:  bytes,
	}
	rl.histIdx = 0
	rl.render()
	return rl
}

// SetPrompt changes the prompt string.
func (rl *Readline) SetPrompt(p string) {
	rl.prompt = p
}

// escInterrupt is called when the user presses Esc. It returns true when two
// Esc presses arrived within escInterval, signaling an interrupt request.
func (rl *Readline) escInterrupt() bool {
	now := time.Now()
	if now.Sub(rl.lastEsc) <= escInterval {
		rl.lastEsc = time.Time{}
		return true
	}
	rl.lastEsc = now
	return false
}

// render redraws the input area, correctly handling wrapped input.
//
// Strategy: compute the visual rows the prompt+buffer occupy, erase exactly
// that many rows (moving the cursor to the top of the input area first),
// print the rows, then position the cursor at its computed row/col.
func (rl *Readline) render() {
	width := termWidth(syscall.Stdin)
	lines := visualLines(rl.prompt, string(rl.buf), width)
	rows := len(lines)

	if !rl.cleared {
		// Move to the top of the input area and erase each row it occupies.
		if rl.rows > 1 {
			wprint(rl.out, "\r\033[%dA", rl.rows-1)
		} else {
			wprint(rl.out, "\r")
		}
		for i := 0; i < rl.rows; i++ {
			wprint(rl.out, "\033[K")
			if i < rl.rows-1 {
				wprint(rl.out, "\033[B")
			}
		}
		// Return to the top row of the input area.
		if rl.rows > 1 {
			wprint(rl.out, "\r\033[%dA", rl.rows-1)
		}
	} else {
		rl.cleared = false
	}

	// Print the wrapped rows.
	for _, l := range lines {
		wprint(rl.out, "%s\r\n", l)
	}
	// We are now one row BELOW the input area (each row printed with \r\n).
	wprint(rl.out, "\033[A") // step back up onto the last input row

	row, col := cursorRowCol(rl.prompt, string(rl.buf), rl.cursor, width)
	if row < rows-1 {
		wprint(rl.out, "\r\033[%dA", rows-1-row) // up to cursor row
	}
	wprint(rl.out, "\r")
	if col > 0 {
		wprint(rl.out, "\033[%dC", col)
	}

	rl.rows = rows
	rl.cursorRow = row
}

// clearLine erases the entire input area (all wrapped rows) and leaves the
// physical cursor at the top-left of where the area used to be.
func (rl *Readline) clearLine() {
	if rl.rows > 0 && !rl.cleared {
		if rl.rows > 1 {
			wprint(rl.out, "\r\033[%dA", rl.rows-1)
		} else {
			wprint(rl.out, "\r")
		}
		for i := 0; i < rl.rows; i++ {
			wprint(rl.out, "\033[K")
			if i < rl.rows-1 {
				wprint(rl.out, "\033[B")
			}
		}
		if rl.rows > 1 {
			wprint(rl.out, "\r\033[%dA", rl.rows-1)
		}
	}
	rl.cleared = true
}

// escInterval is the window for the double-Esc interrupt gesture.
const escInterval = time.Second

// ReadLine blocks until the user enters a complete line (Enter key).
// Handles arrow keys, backspace, Ctrl+A/E/W/K, up/down history, and the
// double-Esc interrupt gesture.
func (rl *Readline) ReadLine() (string, error) {
	for {
		c, ok := <-rl.bytes
		if !ok {
			return "", io.ErrUnexpectedEOF
		}

		switch {
		case c == '\n' || c == '\r':
			line := string(rl.buf)
			if strings.TrimSpace(line) != "" {
				rl.history = append(rl.history, line)
			}
			rl.histIdx = len(rl.history)
			rl.buf = nil
			rl.cursor = 0
			rl.rows = 0 // cursor is already below the input area after \r\n
			rl.cleared = false
			wprintln(rl.out) // move to next line
			return line, nil

		case c == 0x01: // Ctrl+A — move to start
			rl.cursor = 0
			rl.render()

		case c == 0x03: // Ctrl+C — cancel current line
			rl.buf = nil
			rl.cursor = 0
			wprint(rl.out, "\r\033[K^C\n")
			rl.render()
			return "", nil

		case c == 0x05: // Ctrl+E — move to end
			rl.cursor = len(rl.buf)
			rl.render()

		case c == 0x0B: // Ctrl+K — delete to end of line
			rl.buf = rl.buf[:rl.cursor]
			rl.render()

		case c == 0x17: // Ctrl+W — delete previous word
			// Skip trailing spaces, then skip word chars.
			i := rl.cursor
			for i > 0 && (rl.buf[i-1] == ' ' || rl.buf[i-1] == '\t') {
				i--
			}
			for i > 0 && rl.buf[i-1] != ' ' && rl.buf[i-1] != '\t' {
				i--
			}
			rl.buf = append(rl.buf[:i], rl.buf[rl.cursor:]...)
			rl.cursor = i
			rl.render()

		case c == 0x7F || c == 0x08: // Backspace / Ctrl+H
			if rl.cursor > 0 {
				// Walk back over a full UTF-8 rune, not just one byte.
				start := rl.cursor - 1
				for start > 0 && rl.buf[start]&0xC0 == 0x80 {
					start--
				}
				rl.buf = append(rl.buf[:start], rl.buf[rl.cursor:]...)
				rl.cursor = start
				rl.render()
			}

		case c == 0x1B: // Escape sequence or lone Esc key
			// Peek at the next byte with a short grace period to
			// distinguish CSI sequences (ESC [ ...) from a standalone
			// Esc keypress. A lone Esc is the interrupt gesture.
			b1, gotSeq, closed := peekByte(rl.bytes, 40*time.Millisecond)
			if closed {
				// Channel closed.
				continue
			}
			if !gotSeq {
				// No follow-up byte arrived quickly: treat as a
				// standalone Esc press. Two within escInterval
				// signal an interrupt.
				if rl.escInterrupt() && rl.onInterrupt != nil {
					rl.onInterrupt()
				}
				continue
			}
			if b1 != '[' && b1 != 'O' {
				// Not a sequence we handle (e.g. Alt+key); treat
				// the Esc as standalone and deliver the key as
				// typed input.
				if rl.escInterrupt() && rl.onInterrupt != nil {
					rl.onInterrupt()
				}
				// Re-inject the byte so it is not lost.
				rl.handlePrintable(b1)
				continue
			}
			b2, ok := <-rl.bytes
			if !ok {
				continue
			}
			switch b2 {
			case 'A': // Up — previous history
				if rl.histIdx > 0 {
					rl.histIdx--
					rl.buf = []byte(rl.history[rl.histIdx])
					rl.cursor = len(rl.buf)
					rl.render()
				}
			case 'B': // Down — next history
				if rl.histIdx < len(rl.history) {
					rl.histIdx++
					if rl.histIdx == len(rl.history) {
						rl.buf = nil
					} else {
						rl.buf = []byte(rl.history[rl.histIdx])
					}
					rl.cursor = len(rl.buf)
					rl.render()
				}
			case 'C': // Right — move cursor right
				if rl.cursor < len(rl.buf) {
					rl.cursor += runeLenAt(rl.buf, rl.cursor)
					rl.render()
				}
			case 'D': // Left — move cursor left
				if rl.cursor > 0 {
					rl.cursor -= runeLenBefore(rl.buf, rl.cursor)
					rl.render()
				}
			case 'H': // Home
				rl.cursor = 0
				rl.render()
			case 'F': // End
				rl.cursor = len(rl.buf)
				rl.render()
			case '3': // Delete — read one more byte (~)
				tilde, ok := <-rl.bytes
				if !ok {
					continue
				}
				if tilde == '~' && rl.cursor < len(rl.buf) {
					rl.buf = append(rl.buf[:rl.cursor], rl.buf[rl.cursor+runeLenAt(rl.buf, rl.cursor):]...)
					rl.render()
				}
			}

		case c >= 0x20 && c < 0x7F: // Printable ASCII
			rl.handlePrintable(c)

		case c >= 0xC2 && c <= 0xF4: // UTF-8 multibyte rune start
			// Collect the continuation bytes to complete the rune.
			r := []byte{c}
			need := 0
			switch {
			case c < 0xE0:
				need = 1
			case c < 0xF0:
				need = 2
			default:
				need = 3
			}
			for i := 0; i < need; i++ {
				cb, ok := <-rl.bytes
				if !ok {
					break
				}
				r = append(r, cb)
			}
			rl.buf = append(rl.buf[:rl.cursor], append(r, rl.buf[rl.cursor:]...)...)
			rl.cursor += len(r)
			rl.render()

		default:
			// Ignore other control characters.
		}
	}
}

// ─── ChatSession ────────────────────────────────────────────────

// ChatSession holds the state for a terminal chat session.
type ChatSession struct {
	hub       *chat.Hub
	agent     *agent.AgentLoop
	provider  providers.LLMProvider
	cfg       config.Config
	Model     string // exported so main.go can override with -M flag
	homeDir   string
	profileWS string
	ws        string
	herdr     *herdrReporter

	out    io.Writer
	chatID string // unique session ID for hub routing

	// State
	multiLine  bool
	busy       bool
	busyCancel context.CancelFunc

	// Readline + output coordination
	rl       *Readline
	origTerm *termios
	mu       sync.Mutex // protects output interleaving

	// Single stdin owner: rawBytes is fed by one goroutine.
	rawBytes chan byte

	// responseWait is how long sendMessage waits for a reply before cancelling
	// the active turn. Zero means the default of fifteen minutes.
	responseWait time.Duration
}

// defaultResponseWait is the fallback wait for a final reply. Agentic turns
// routinely run many LLM iterations (each up to the provider request timeout)
// plus tool execution; five minutes was routinely too short and caused the
// TUI to give up while the turn was mid-flight — the reply then landed in the
// cliOut buffer and was misprinted as the response to the NEXT prompt.
const defaultResponseWait = 15 * time.Minute

// New creates a new TUI chat session using the configured workspace for both
// profile state and tool execution.
func New(cfg config.Config, provider providers.LLMProvider, homeDir, ws string) *ChatSession {
	return NewWithProject(cfg, provider, homeDir, ws, ws)
}

// NewWithProject creates a TUI session with persistent profile state separated
// from the active project working directory.
func NewWithProject(cfg config.Config, provider providers.LLMProvider, homeDir, profileWS, projectWS string) *ChatSession {
	if profileWS == "" {
		profileWS = projectWS
	}

	model := cfg.Agents.Defaults.Model
	if model == "" {
		model = provider.GetDefaultModel()
	}

	s := &ChatSession{
		cfg:       cfg,
		provider:  provider,
		Model:     model,
		homeDir:   homeDir,
		profileWS: profileWS,
		ws:        projectWS,
		herdr:     newHerdrReporter(),
		out:       os.Stdout,
		chatID:    "tui-" + fmt.Sprintf("%d", time.Now().UnixNano()),
	}
	if w := cfg.Agents.Defaults.TuiResponseWaitS; w > 0 {
		s.responseWait = time.Duration(w) * time.Second
	}
	return s
}

// sessionKey returns the hub session key for the current chat.
func (s *ChatSession) sessionKey() string {
	return "cli:" + s.chatID
}

// writeAbove prints output above the current input line.
// It temporarily clears the input line, prints the output,
// then re-renders the input prompt below.
func (s *ChatSession) writeAbove(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rl != nil && !s.busy {
		s.rl.clearLine()
	}
	wprint(s.out, "%s", text)
	if s.rl != nil && !s.busy {
		s.rl.render()
	}
}

// startStdinReader launches a single goroutine that owns os.Stdin and
// feeds raw bytes into the rawBytes channel. This ensures only one
// goroutine ever reads from stdin.
func (s *ChatSession) startStdinReader(ctx context.Context) {
	s.rawBytes = make(chan byte, 256)
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				close(s.rawBytes)
				return
			}
			select {
			case s.rawBytes <- buf[0]:
			case <-ctx.Done():
				close(s.rawBytes)
				return
			}
		}
	}()
}

// startRuntime constructs the hub and agent loop, then starts both the
// outbound router and AgentLoop.Run. Messages written to hub.In are not
// processed until this returns.
func (s *ChatSession) startRuntime(ctx context.Context) <-chan chat.Outbound {
	s.hub = chat.NewHub(100)

	maxIter := s.cfg.Agents.Defaults.MaxToolIterations
	if maxIter <= 0 {
		maxIter = 100
	}

	s.agent = agent.NewAgentLoopWithProfileWorkspace(
		s.hub, s.provider, s.Model, maxIter, s.ws, s.profileWS,
		nil, // scheduler — cron not active in TUI
		s.cfg.MCPServers, s.cfg.Agents.Defaults.AllowedDirs,
		s.cfg.Agents.Defaults.DisableTools,
		s.cfg.Brain, s.homeDir,
		s.cfg.Agents.Defaults.Sandbox,
		"", // signal socket — not active in TUI
		s.cfg.Agents.Defaults.MaxTurnMessages,
		s.cfg.Agents.Defaults.MaxToolResultChars,
		s.cfg.Agents.Defaults.Compaction,
		s.cfg.Agents.Defaults.Web,
		s.cfg.Agents.Defaults.Search,
		s.cfg.Agents.Defaults.VisionModel,
	)

	if s.cfg.Agents.Defaults.SessionAutoTitle != nil {
		s.agent.SetSessionAutoTitle(*s.cfg.Agents.Defaults.SessionAutoTitle)
	}
	if s.cfg.Agents.Defaults.AutoContinue != nil {
		s.agent.SetAutoContinue(*s.cfg.Agents.Defaults.AutoContinue)
	}

	cliOut := s.hub.Subscribe("cli")
	s.hub.StartRouter(ctx)
	go s.agent.Run(ctx)
	return cliOut
}

// Run starts the interactive chat loop.
func (s *ChatSession) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cliOut := s.startRuntime(ctx)
	defer s.agent.Close()

	s.herdr.report("idle", "ready")
	defer s.herdr.release()

	// Handle Ctrl+C gracefully — the signal handler is a fallback.
	// In raw mode, Ctrl+C is caught by readline (returns empty line).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		if s.origTerm != nil {
			restoreTerm(syscall.Stdin, s.origTerm)
		}
		wprintln(s.out)
		cancel()
		s.herdr.release()
		os.Exit(0)
	}()

	// Print banner before entering raw mode.
	s.printBanner()

	// Enter raw mode for readline.
	var err error
	s.origTerm, err = makeRaw(syscall.Stdin)
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer restoreTerm(syscall.Stdin, s.origTerm)

	// Start the single stdin reader goroutine.
	s.startStdinReader(ctx)

	prompt := fmt.Sprintf("%syou%s ❯ ", cyan+bold, reset)
	s.rl = NewReadline(s.out, prompt, s.rawBytes)
	// Double-Esc while idle does nothing (nothing to interrupt); it is
	// handled inside sendMessage while a turn is running.
	s.rl.onInterrupt = nil

	for {
		line, err := s.rl.ReadLine()
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Handle slash commands — always processed immediately.
		if strings.HasPrefix(line, "/") {
			if !s.handleCommand(line) {
				break // /exit
			}
			continue
		}

		// Send message to agent and wait for response.
		s.sendMessage(ctx, cliOut, line)
		// Re-render the input prompt after the turn completes.
		s.mu.Lock()
		s.rl.render()
		s.mu.Unlock()
	}

	return nil
}

// sendMessage sends a message to the agent loop and waits for the response.
// During the wait, it reads from the shared rawBytes channel to detect /stop.
func (s *ChatSession) sendMessage(ctx context.Context, cliOut <-chan chat.Outbound, text string) {
	msg := chat.Inbound{
		Channel:   "cli",
		SenderID:  "tui-user",
		ChatID:    s.chatID,
		Content:   text,
		Timestamp: time.Now(),
	}

	s.herdr.report("working", "thinking")

	s.hub.In <- msg

	// Mark busy.
	waitCtx, waitCancel := context.WithCancel(context.Background())
	s.busy = true
	s.busyCancel = waitCancel
	defer func() {
		s.busy = false
		s.busyCancel = nil
		s.herdr.report("idle", "ready")
	}()

	// startSpinner launches a spinner goroutine and returns a cancel function
	// that stops it and waits for completion.
	startSpinner := func() func() {
		spCtx, spCancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go s.spinner(spCtx, done)
		return func() {
			spCancel()
			<-done
		}
	}

	stopSpinner := startSpinner()

	wait := s.responseWait
	if wait <= 0 {
		wait = defaultResponseWait
	}
	timeout := time.NewTimer(wait)
	defer timeout.Stop()

	// Buffer for /stop detection — chars read during busy mode.
	// These are NOT consumed by ReadLine (which reads from the same channel).
	// Since we own the channel during sendMessage, any bytes we read here
	// are intentionally consumed.
	var lineBuf []byte

	// lastBusyEsc tracks the previous standalone Esc press for the
	// double-Esc interrupt gesture while a turn is running.
	var lastBusyEsc time.Time

	abort := func() {
		s.agent.StopTurn(s.sessionKey())
		stopSpinner()
		// Drain remaining messages.
		drainTimer := time.NewTimer(2 * time.Second)
	drainLoop:
		for {
			select {
			case out, ok := <-cliOut:
				if !ok || !isActivityNotification(out) {
					break drainLoop
				}
			case <-drainTimer.C:
				break drainLoop
			}
		}
		drainTimer.Stop()
		s.writeAbove(fmt.Sprintf("%s✓ Interrupted.%s\n", yellow, reset))
	}

	for {
		select {
		case <-ctx.Done():
			stopSpinner()
			return

		case <-waitCtx.Done():
			stopSpinner()
			// /stop was invoked — the user asked to abort, so a racing reply
			// is discarded, not delivered. StopTurn sets the agent-side
			// stopped flag (suppressing the queue), but a reply may already
			// be sitting in cliOut from the instant before the stop landed;
			// consume it so it cannot leak into the next prompt's wait loop.
			awaitRacingReply(cliOut, 2*time.Second)
			return

		case c, ok := <-s.rawBytes:
			if !ok {
				stopSpinner()
				return
			}
			if c == 0x1B { // Esc — maybe an arrow key, maybe lone Esc
				b1, gotSeq, chClosed := peekByte(s.rawBytes, 40*time.Millisecond)
				if chClosed {
					continue
				}
				if !gotSeq {
					// Standalone Esc: two within escInterval interrupts.
					now := time.Now()
					if now.Sub(lastBusyEsc) <= escInterval {
						abort()
						return
					}
					lastBusyEsc = now
					continue
				}
				// It's a sequence (e.g. ESC [ A). Consume the rest
				// of it so arrows don't leak into /stop detection.
				if b1 == '[' || b1 == 'O' {
					next, ok := <-s.rawBytes
					if !ok {
						continue
					}
					// CSI parameters are digits; the final byte
					// follows (e.g. ESC [ 3 ~ for Delete).
					if next >= '0' && next <= '9' {
						if fin, ok := <-s.rawBytes; ok && fin != '~' {
							_ = fin
						}
					}
				}
				continue
			}
			if c == '\n' || c == '\r' {
				cmd := strings.TrimSpace(string(lineBuf))
				lineBuf = lineBuf[:0]
				if cmd == "/stop" || cmd == "/abort" || cmd == "/cancel" {
					abort()
					return
				}
			} else if c >= 0x20 {
				lineBuf = append(lineBuf, c)
			}

		case out, ok := <-cliOut:
			if !ok {
				stopSpinner()
				return
			}

			if isActivityNotification(out) {
				stopSpinner()
				s.writeAbove(fmt.Sprintf("%s%s%s\n", dim, out.Content, reset))
				// Restart spinner.
				stopSpinner = startSpinner()
				continue
			}

			// Final response.
			stopSpinner()
			s.writeAbove(fmt.Sprintf("%sgino%s ❯ %s\n\n", magenta+bold, reset, out.Content))
			return

		case <-timeout.C:
			stopSpinner()
			s.agent.StopTurn(s.sessionKey())
			// The response wait expired. StopTurn suppresses the agent-side
			// reply, but one may have been queued in the instant before the
			// stop landed (turn finished normally, timer fired mid-queue) —
			// deliver it instead of letting it leak to the next prompt.
			if out, ok := awaitRacingReply(cliOut, 2*time.Second); ok {
				s.writeAbove(fmt.Sprintf("%sgino%s ❯ %s\n\n", magenta+bold, reset, out.Content))
				return
			}
			s.writeAbove(fmt.Sprintf("%stimeout waiting for response%s\n", red, reset))
			return
		}
	}
}

// awaitRacingReply waits up to d for a final (non-notification) message on
// ch, returning it if one arrives. Used after stopping a turn to catch a
// reply queued in the instant before the stop landed, so it is either
// delivered (timeout path) or consumed (abort path) instead of leaking into
// the next prompt's wait loop.
func awaitRacingReply(ch <-chan chat.Outbound, d time.Duration) (chat.Outbound, bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case out, ok := <-ch:
			if !ok {
				return chat.Outbound{}, false
			}
			if isActivityNotification(out) {
				continue
			}
			return out, true
		case <-t.C:
			return chat.Outbound{}, false
		}
	}
}

// isActivityNotification reports whether an outbound message is a progress
// notification rather than a final reply. The agent tags notifications with a
// "notification" metadata flag, which is authoritative. The prefix sniff is a
// fallback for untagged messages. Note: "⏳" is deliberately NOT in the prefix
// list — the iteration-limit notice is a final reply that starts with ⏳, and
// misclassifying it would leave the TUI waiting for a response that already
// arrived (until the response timeout).
func isActivityNotification(out chat.Outbound) bool {
	if out.Metadata != nil {
		if v, ok := out.Metadata["notification"].(bool); ok {
			return v
		}
	}
	prefixes := []string{"🤖", "📢", "⚠️", "⛔", "🔄", "🗑️", "📥"}
	for _, p := range prefixes {
		if strings.HasPrefix(out.Content, p) {
			return true
		}
	}
	return false
}

// spinner prints an animated spinner on the current line while waiting.
func (s *ChatSession) spinner(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	chars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	i := 0
	for {
		select {
		case <-ctx.Done():
			// Clear the spinner line.
			s.mu.Lock()
			wprint(s.out, "\r\033[K")
			s.mu.Unlock()
			return
		case <-time.After(100 * time.Millisecond):
			s.mu.Lock()
			wprint(s.out, "\r\033[K%s%s thinking...%s", gray, chars[i%len(chars)], reset)
			s.mu.Unlock()
			i++
		}
	}
}

// handleCommand processes slash commands. Returns true to continue the loop,
// false to exit.
func (s *ChatSession) handleCommand(line string) bool {
	parts := strings.Fields(line)
	cmd := parts[0]

	switch cmd {
	case "/exit", "/quit", "/q":
		return false

	case "/help", "/h", "/?":
		s.printHelp()

	case "/clear":
		s.writeAbove("\033[2J\033[H") // clear screen
		s.printBanner()

	case "/new":
		if s.busy {
			s.writeAbove(fmt.Sprintf("%sCannot start new chat while agent is working. Use /stop first.%s\n", red, reset))
			return true
		}
		// Archive current session before starting fresh.
		s.agent.ArchiveSession(s.sessionKey())
		s.chatID = "tui-" + fmt.Sprintf("%d", time.Now().UnixNano())
		s.writeAbove("\033[2J\033[H")
		s.printBanner()
		s.writeAbove(fmt.Sprintf("%s✓ New conversation started (previous session archived)%s\n", green, reset))
		s.writeAbove(fmt.Sprintf("%sUse /sessions to list or /session <N> to switch back%s\n\n", dim, reset))

	case "/sessions":
		sessions := s.agent.ListArchivedSessions(s.sessionKey())
		if cur := s.agent.CurrentSessionSummary(s.sessionKey()); cur != nil {
			age := humanizeAge(cur.UpdatedAt)
			s.writeAbove(fmt.Sprintf("\n%sCurrent session:%s %s (%d msgs, %s)\n", bold, reset, cur.Title, cur.MessageN, age))
		}
		if len(sessions) == 0 {
			if s.agent.CurrentSessionSummary(s.sessionKey()) == nil {
				s.writeAbove(fmt.Sprintf("%sNo saved sessions. Use /new to archive the current one.%s\n\n", dim, reset))
			} else {
				s.writeAbove(fmt.Sprintf("%sNo archived sessions yet. Use /new to save the current one.%s\n\n", dim, reset))
			}
			return true
		}
		s.writeAbove(fmt.Sprintf("%sSaved Sessions:%s\n", bold, reset))
		for i, si := range sessions {
			age := humanizeAge(si.UpdatedAt)
			s.writeAbove(fmt.Sprintf("  %s%d.%s %s (%d msgs, %s)\n", cyan, i+1, reset, si.Title, si.MessageN, age))
		}
		s.writeAbove(fmt.Sprintf("\n%sUse /session <N> to switch.%s\n\n", dim, reset))

	case "/session":
		if len(parts) < 2 {
			s.writeAbove(fmt.Sprintf("%sUsage: /session <N>%s\n", yellow, reset))
			return true
		}
		num, err := strconv.Atoi(parts[1])
		if err != nil || num < 1 {
			s.writeAbove(fmt.Sprintf("%sInvalid session number.%s\n", red, reset))
			return true
		}
		title := s.agent.SwitchToArchivedSession(s.sessionKey(), num-1)
		if title == "" {
			s.writeAbove(fmt.Sprintf("%sSession %d not found.%s\n", red, num, reset))
			return true
		}
		s.writeAbove(fmt.Sprintf("%s✓ Switched to: %s%s\n\n", green, title, reset))

	case "/purge":
		if len(parts) < 2 {
			s.writeAbove(fmt.Sprintf("%sUsage: /purge <days>%s — deletes sessions older than N days\n", yellow, reset))
			return true
		}
		days, err := strconv.Atoi(parts[1])
		if err != nil || days < 1 {
			s.writeAbove(fmt.Sprintf("%sInvalid number of days.%s\n", red, reset))
			return true
		}
		deleted := s.agent.PurgeOldSessions(s.sessionKey(), days)
		s.writeAbove(fmt.Sprintf("%s✓ Purged %d session(s) older than %d day(s).%s\n", green, deleted, days, reset))

	case "/search":
		if len(parts) < 2 {
			s.writeAbove(fmt.Sprintf("%sUsage: /search <text>%s — searches saved sessions for matches\n", yellow, reset))
			return true
		}
		query := strings.Join(parts[1:], " ")
		results := s.agent.SearchArchivedSessions(s.sessionKey(), query)
		if len(results) == 0 {
			s.writeAbove(fmt.Sprintf("%sNo sessions matching \"%s\".%s\n\n", dim, query, reset))
			return true
		}
		s.writeAbove(fmt.Sprintf("\n%sSessions matching \"%s\":%s\n", bold, query, reset))
		for i, r := range results {
			age := humanizeAge(r.UpdatedAt)
			s.writeAbove(fmt.Sprintf("  %s%d.%s %s (%d msgs, %s)\n", cyan, i+1, reset, r.Title, r.MessageN, age))
			if r.Snippet != "" {
				s.writeAbove(fmt.Sprintf("     %s\"%s\"%s\n", dim, r.Snippet, reset))
			}
		}
		s.writeAbove(fmt.Sprintf("\n%sUse /session <N> to switch.%s\n\n", dim, reset))

	case "/stop", "/abort", "/cancel":
		if !s.busy {
			s.writeAbove(fmt.Sprintf("%sNothing to stop.%s\n", dim, reset))
		} else if s.busyCancel != nil {
			s.agent.StopTurn(s.sessionKey())
			s.busyCancel()
			s.writeAbove(fmt.Sprintf("%s✓ Aborting current turn...%s\n", yellow, reset))
		}

	case "/model":
		if len(parts) > 1 {
			s.Model = parts[1]
			s.writeAbove(fmt.Sprintf("%sModel set to: %s%s\n", green, s.Model, reset))
		} else {
			s.writeAbove(fmt.Sprintf("%sCurrent model: %s%s\n", dim, s.Model, reset))
		}

	case "/reasoning", "/reason":
		if len(parts) == 1 {
			effort, ok := providers.GetReasoningEffort(s.provider)
			if !ok {
				s.writeAbove(fmt.Sprintf("%sProvider does not support reasoning control.%s\n", yellow, reset))
				return true
			}

			if effort == "" {
				effort = "provider default"
			}

			s.writeAbove(fmt.Sprintf("%sCurrent reasoning: %s%s\n", dim, effort, reset))
			return true
		}

		if len(parts) != 2 {
			s.writeAbove(fmt.Sprintf("%sUsage: /reasoning <none|low|medium|high>%s\n", yellow, reset))
			return true
		}

		effort, ok := providers.NormalizeReasoningEffort(parts[1])
		if !ok {
			s.writeAbove(fmt.Sprintf("%sInvalid reasoning level. Use none, low, medium, or high.%s\n", yellow, reset))
			return true
		}

		if !providers.SetReasoningEffort(s.provider, effort) {
			s.writeAbove(fmt.Sprintf("%sProvider does not support reasoning control.%s\n", yellow, reset))
			return true
		}

		s.writeAbove(fmt.Sprintf("%s✓ Reasoning set to: %s%s\n", green, effort, reset))

	case "/title":
		rest := strings.TrimSpace(strings.TrimPrefix(line, "/title"))
		if rest == "" {
			cur := s.agent.CurrentSessionTitle(s.sessionKey())
			if cur == "" {
				cur = "(untitled)"
			}
			s.writeAbove(fmt.Sprintf("%sUsage: /title <text> — title current session, /title <N> <text> — rename archived session N\nCurrent title: %s%s\n\n", dim, cur, reset))
			return true
		}
		if fields := strings.Fields(rest); len(fields) >= 2 {
			if n, err := strconv.Atoi(fields[0]); err == nil && n >= 1 {
				titleText := strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
				if old, ok := s.agent.SetArchivedSessionTitle(s.sessionKey(), n-1, titleText); ok {
					s.writeAbove(fmt.Sprintf("%s✓ Session %d retitled: %s (was: %s)%s\n\n", green, n, titleText, old, reset))
					return true
				}
				s.writeAbove(fmt.Sprintf("%sSession %d not found. Use /sessions to list.%s\n\n", yellow, n, reset))
				return true
			}
		}
		s.agent.SetSessionTitle(s.sessionKey(), rest)
		s.writeAbove(fmt.Sprintf("%s✓ Current session titled: %s%s\n\n", green, rest, reset))

	case "/multiline", "/multi":
		s.multiLine = !s.multiLine
		state := "off"
		if s.multiLine {
			state = "on"
		}
		s.writeAbove(fmt.Sprintf("%sMulti-line mode: %s%s\n", dim, state, reset))

	default:
		s.writeAbove(fmt.Sprintf("%sUnknown command: %s%s\n", yellow, cmd, reset))
		s.printHelp()
	}
	return true
}

// printBanner shows the startup banner.
func (s *ChatSession) printBanner() {
	wprint(s.out, "\n")
	for _, line := range formatBanner("v0.5.0", s.Model) {
		wprint(s.out, "%s\n", line)
	}
	wprint(s.out, "\n")
}

// printHelp shows available commands.
func (s *ChatSession) printHelp() {
	s.writeAbove(fmt.Sprintf("\n%sCommands:%s\n", bold, reset))
	s.writeAbove(fmt.Sprintf("  %s/new%s        Start new conversation (archives current)\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/sessions%s   List saved sessions\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/session N%s  Switch to session #N\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/title text%s  Title current session (/title N text renames #N)\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/search text%s  Search saved sessions\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/purge days%s  Delete sessions older than N days\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/stop%s       Abort the current response\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %sEsc Esc%s     Abort the current response (press Esc twice within 1s)\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/clear%s      Clear the terminal screen\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/model%s      Show or set model (/model gpt-4o)\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/reasoning%s  Show or set reasoning (none|low|medium|high)\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/multiline%s  Toggle multi-line input mode\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/exit%s       Exit chat\n\n", cyan, reset))
}

// humanizeAge formats a time as a relative age string.
func humanizeAge(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
