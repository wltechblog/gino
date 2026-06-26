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
type Readline struct {
	fd       int
	out      io.Writer
	prompt   string
	history  []string
	histIdx  int // index into history for navigation; len(history) = "new entry"

	// Current buffer state.
	buf     []byte
	cursor  int // byte offset within buf
}

// NewReadline creates a readline instance and sets the terminal to raw mode.
func NewReadline(fd int, out io.Writer, prompt string) (*Readline, error) {
	if _, err := makeRaw(fd); err != nil {
		return nil, fmt.Errorf("set raw mode: %w", err)
	}
	rl := &Readline{
		fd:     fd,
		out:    out,
		prompt: prompt,
	}
	rl.histIdx = 0
	rl.render()
	return rl, nil
}

// Restore restores the terminal to its original state.
func (rl *Readline) Restore(orig *termios) {
	restoreTerm(rl.fd, orig)
}

// SetPrompt changes the prompt string.
func (rl *Readline) SetPrompt(p string) {
	rl.prompt = p
}

// render redraws the current input line.
func (rl *Readline) render() {
	// Clear line, move to start, print prompt + buffer.
	fmt.Fprintf(rl.out, "\r\033[K%s%s", rl.prompt, string(rl.buf))
	// Move cursor to correct position.
	total := len(rl.buf)
	if rl.cursor < total {
		// Move back by (total - cursor) characters.
		back := total - rl.cursor
		fmt.Fprintf(rl.out, "\033[%dD", back)
	}
}

// clearLine clears the input line (for output above it).
func (rl *Readline) clearLine() {
	fmt.Fprint(rl.out, "\r\033[K")
}

// ReadLine blocks until the user enters a complete line (Enter key).
// Handles arrow keys, backspace, Ctrl+A/E/W/K, up/down history.
func (rl *Readline) ReadLine() (string, error) {
	buf := make([]byte, 1)
	escBuf := make([]byte, 2)

	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return "", io.ErrUnexpectedEOF
		}
		c := buf[0]

		switch {
		case c == '\n' || c == '\r':			line := string(rl.buf)
			rl.buf = nil
			rl.cursor = 0
			fmt.Fprintln(rl.out) // move to next line
			if strings.TrimSpace(line) != "" {
				rl.history = append(rl.history, line)
			}
			rl.histIdx = len(rl.history)
			rl.buf = nil
			rl.cursor = 0
			return line, nil

		case c == 0x01: // Ctrl+A — move to start
			rl.cursor = 0
			rl.render()

		case c == 0x03: // Ctrl+C — cancel current line
			rl.buf = nil
			rl.cursor = 0
			fmt.Fprintf(rl.out, "\r\033[K^C\n")
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
			// Read the next two bytes for arrow keys.
			n2, err := os.Stdin.Read(escBuf)
			if err != nil || n2 < 2 {
				continue
			}
			if escBuf[0] == '[' {
				switch escBuf[1] {
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
					var tilde [1]byte
					if _, err := os.Stdin.Read(tilde[:]); err != nil {
						continue
					}
					if tilde[0] == '~' && rl.cursor < len(rl.buf) {
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
	model    string
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
// then re-renders the input prompt + buffer.
func (s *ChatSession) writeAbove(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rl != nil {
		s.rl.clearLine()
	}
	fmt.Fprint(s.out, text)
	if s.rl != nil {
		s.rl.render()
	}
}

// Run starts the interactive chat loop. Blocks until the user exits.
func (s *ChatSession) Run(modelOverride string) error {
	s.model = modelOverride
	if s.model == "" && s.cfg.Agents.Defaults.Model != "" {
		s.model = s.cfg.Agents.Defaults.Model
	}
	if s.model == "" {
		s.model = s.provider.GetDefaultModel()
	}

	// Set up hub and agent loop — same as gateway but CLI-only.
	s.hub = chat.NewHub(100)

	maxIter := s.cfg.Agents.Defaults.MaxToolIterations
	if maxIter <= 0 {
		maxIter = 100
	}

	s.agent = agent.NewAgentLoop(
		s.hub, s.provider, s.model, maxIter, s.ws,
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

	if s.cfg.Agents.Defaults.EnableToolActivityIndicator != nil {
		s.agent.SetToolActivityIndicator(*s.cfg.Agents.Defaults.EnableToolActivityIndicator)
	}
	if s.cfg.Agents.Defaults.EnableToolCallMessages != nil {
		s.agent.SetToolCallMessages(*s.cfg.Agents.Defaults.EnableToolCallMessages)
	}

	// Subscribe to the "cli" channel for outbound messages.
	cliOut := s.hub.Subscribe("cli")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start agent loop.
	go s.agent.Run(ctx)

	// Start router.
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
		fmt.Fprintln(s.out)
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

	prompt := fmt.Sprintf("%syou%s ❯ ", cyan+bold, reset)
	s.rl, err = NewReadline(syscall.Stdin, s.out, prompt)
	if err != nil {
		return err
	}

	for {
		line, err := s.rl.ReadLine()
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Handle slash commands.
		if strings.HasPrefix(line, "/") {
			if !s.handleCommand(line) {
				break // /exit
			}
			continue
		}

		// Send message to agent and wait for response.
		s.sendMessage(ctx, cliOut, line)
	}

	return nil
}

// sendMessage sends a message to the agent loop and waits for the response.
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

	// Channel for user interrupt (/stop typed during response).
	stopCh := make(chan struct{}, 1)
	go func() {
		for {
			line, err := s.rl.ReadLine()
			if err != nil {
				return
			}
			cmd := strings.TrimSpace(line)
			if cmd == "/stop" || cmd == "/abort" || cmd == "/cancel" {
				select {
				case stopCh <- struct{}{}:
				default:
				}
				return
			}
			// Ignore other input while busy.
			if cmd != "" {
				s.writeAbove(fmt.Sprintf("%s(queued: %s)%s\n", gray, cmd, reset))
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			stopSpinner()
			return

		case <-waitCtx.Done():
			stopSpinner()
			return

		case <-stopCh:
			s.agent.StopTurn(s.sessionKey())
			stopSpinner()
			// Drain remaining messages.
			drainTimer := time.NewTimer(2 * time.Second)
			defer drainTimer.Stop()
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
			s.writeAbove(fmt.Sprintf("%s✓ Aborted.%s\n", yellow, reset))
			return

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

// spinner prints an animated spinner above the input line.
func (s *ChatSession) spinner(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	chars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	i := 0
	for {
		select {
		case <-ctx.Done():
			// Clear the spinner line.
			s.mu.Lock()
			s.rl.clearLine()
			s.rl.render()
			s.mu.Unlock()
			return
		case <-time.After(100 * time.Millisecond):
			s.mu.Lock()
			// Clear current line, print spinner, re-render input below.
			s.rl.clearLine()
			fmt.Fprintf(s.out, "%s%s thinking...%s", gray, chars[i%len(chars)], reset)
			// Move to next line, render input, then move back up.
			fmt.Fprintf(s.out, "\n%s%s%s\033[A", s.rl.prompt, string(s.rl.buf), reset)
			// Position cursor correctly in input.
			total := len(s.rl.buf)
			if s.rl.cursor < total {
				back := total - s.rl.cursor
				fmt.Fprintf(s.out, "\033[%dD", back)
			}
			// Move back up to spinner line.
			fmt.Fprint(s.out, "\033[A\r")
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
			s.model = parts[1]
			s.writeAbove(fmt.Sprintf("%sModel set to: %s%s\n", green, s.model, reset))
		} else {
			s.writeAbove(fmt.Sprintf("%sCurrent model: %s%s\n", dim, s.model, reset))
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
	fmt.Fprintf(s.out, "\n%s╔══════════════════════════════════════════╗\n", cyan)
	fmt.Fprintf(s.out, "║  🤖 Gino Chat %sv0.5.0%s                     ║\n", dim, cyan)
	fmt.Fprintf(s.out, "║  Model: %-34s║\n", truncForBox(s.model, 34)+" ")
	fmt.Fprintf(s.out, "║  Type %s/help%s for commands               ║\n", bold, cyan)
	fmt.Fprintf(s.out, "%s╚══════════════════════════════════════════╝\n%s\n\n", cyan, reset)
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
