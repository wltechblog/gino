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

// render redraws the current input line.
func (rl *Readline) render() {
	// Clear line, move to start, print prompt + buffer.
	wprint(rl.out, "\r\033[K%s%s", rl.prompt, string(rl.buf))
	// Move cursor to correct position.
	total := len(rl.buf)
	if rl.cursor < total {
		// Move back by (total - cursor) characters.
		back := total - rl.cursor
		wprint(rl.out, "\033[%dD", back)
	}
}

// clearLine clears the input line (for output above it).
func (rl *Readline) clearLine() {
	wprint(rl.out, "\r\033[K")
}

// readEsc reads a 2-byte escape sequence from the byte channel.
// Returns the two bytes, or zero values if the channel closed.
func (rl *Readline) readEsc() (byte, byte) {
	b1, ok := <-rl.bytes
	if !ok {
		return 0, 0
	}
	b2, ok := <-rl.bytes
	if !ok {
		return b1, 0
	}
	return b1, b2
}

// ReadLine blocks until the user enters a complete line (Enter key).
// Handles arrow keys, backspace, Ctrl+A/E/W/K, up/down history.
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
				rl.buf = append(rl.buf[:rl.cursor-1], rl.buf[rl.cursor:]...)
				rl.cursor--
				rl.render()
			}

		case c == 0x1B: // Escape sequence
			b1, b2 := rl.readEsc()
			if b1 == '[' {
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
						rl.cursor++
						rl.render()
					}
				case 'D': // Left — move cursor left
					if rl.cursor > 0 {
						rl.cursor--
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
						rl.buf = append(rl.buf[:rl.cursor], rl.buf[rl.cursor+1:]...)
						rl.render()
					}
				}
			}

		case c >= 0x20 && c < 0x7F: // Printable ASCII
			rl.buf = append(rl.buf[:rl.cursor], append([]byte{c}, rl.buf[rl.cursor:]...)...)
			rl.cursor++
			rl.render()

		default:
			// Ignore other control characters.
		}
	}
}

// ─── ChatSession ────────────────────────────────────────────────

// ChatSession holds the state for a terminal chat session.
type ChatSession struct {
	hub      *chat.Hub
	agent    *agent.AgentLoop
	provider providers.LLMProvider
	cfg      config.Config
	Model    string // exported so main.go can override with -M flag
	homeDir  string
	ws       string

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
}

// New creates a new TUI chat session.
func New(cfg config.Config, provider providers.LLMProvider, homeDir, ws string) *ChatSession {
	return &ChatSession{
		cfg:      cfg,
		provider: provider,
		homeDir:  homeDir,
		ws:       ws,
		out:      os.Stdout,
		chatID:   "tui-" + fmt.Sprintf("%d", time.Now().UnixNano()),
	}
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

// Run starts the interactive chat loop.
func (s *ChatSession) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Set up hub and agent loop — same as gateway but CLI-only.
	s.hub = chat.NewHub(100)

	maxIter := s.cfg.Agents.Defaults.MaxToolIterations
	if maxIter <= 0 {
		maxIter = 100
	}

	s.agent = agent.NewAgentLoop(
		s.hub, s.provider, s.Model, maxIter, s.ws,
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
	)
	defer s.agent.Close()

	cliOut := s.hub.Subscribe("cli")

	s.hub.StartRouter(ctx)

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

	s.hub.In <- msg

	// Mark busy.
	waitCtx, waitCancel := context.WithCancel(context.Background())
	s.busy = true
	s.busyCancel = waitCancel
	defer func() {
		s.busy = false
		s.busyCancel = nil
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

	// Buffer for /stop detection — chars read during busy mode.
	// These are NOT consumed by ReadLine (which reads from the same channel).
	// Since we own the channel during sendMessage, any bytes we read here
	// are intentionally consumed.
	var lineBuf []byte

	for {
		select {
		case <-ctx.Done():
			stopSpinner()
			return

		case <-waitCtx.Done():
			stopSpinner()
			return

		case c, ok := <-s.rawBytes:
			if !ok {
				stopSpinner()
				return
			}
			if c == '\n' || c == '\r' {
				cmd := strings.TrimSpace(string(lineBuf))
				lineBuf = lineBuf[:0]
				if cmd == "/stop" || cmd == "/abort" || cmd == "/cancel" {
					s.agent.StopTurn(s.sessionKey())
					stopSpinner()
					// Drain remaining messages.
					drainTimer := time.NewTimer(2 * time.Second)
				drainLoop:
					for {
						select {
						case out, ok := <-cliOut:
							if !ok || !isActivityNotification(out.Content) {
								break drainLoop
							}
						case <-drainTimer.C:
							break drainLoop
						}
					}
					drainTimer.Stop()
					s.writeAbove(fmt.Sprintf("%s✓ Aborted.%s\n", yellow, reset))
					return
				}
			} else {
				lineBuf = append(lineBuf, c)
			}

		case out, ok := <-cliOut:
			if !ok {
				stopSpinner()
				return
			}

			if isActivityNotification(out.Content) {
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

		case <-time.After(5 * time.Minute):
			stopSpinner()
			s.writeAbove(fmt.Sprintf("%stimeout waiting for response%s\n", red, reset))
			return
		}
	}
}

// isActivityNotification returns true if the content is a tool activity message
// rather than a final response.
func isActivityNotification(content string) bool {
	prefixes := []string{"⏳", "🤖", "📢", "⛔", "🔄", "🗑️"}
	for _, p := range prefixes {
		if strings.HasPrefix(content, p) {
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
		s.agent.DeleteSession(s.sessionKey())
		s.chatID = "tui-" + fmt.Sprintf("%d", time.Now().UnixNano())
		s.writeAbove("\033[2J\033[H") // clear screen too for a clean look
		s.printBanner()
		s.writeAbove(fmt.Sprintf("%s✓ New conversation started%s\n\n", green, reset))

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
	wprint(s.out, "\n%s╔══════════════════════════════════════════╗\n", cyan)
	wprint(s.out, "║  🤖 Gino Chat %sv0.5.0%s                     ║\n", dim, cyan)
	wprint(s.out, "║  Model: %-34s║\n", truncForBox(s.Model, 34)+" ")
	wprint(s.out, "║  Type %s/help%s for commands               ║\n", bold, cyan)
	wprint(s.out, "%s╚══════════════════════════════════════════╝\n%s\n\n", cyan, reset)
}

// printHelp shows available commands.
func (s *ChatSession) printHelp() {
	s.writeAbove(fmt.Sprintf("\n%sCommands:%s\n", bold, reset))
	s.writeAbove(fmt.Sprintf("  %s/new%s       Start a new conversation (clears history)\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/stop%s      Abort the current response\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/clear%s     Clear the terminal screen\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/model%s     Show or set model (/model gpt-4o)\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/multiline%s Toggle multi-line input mode\n", cyan, reset))
	s.writeAbove(fmt.Sprintf("  %s/exit%s      Exit chat\n\n", cyan, reset))
}

// truncForBox truncates a string to fit in a box of the given width.
func truncForBox(str string, max int) string {
	if len(str) > max {
		return str[:max]
	}
	return str
}
