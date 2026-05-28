package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/local/picobot/internal/chat"
	"github.com/local/picobot/internal/config"
)

// Signal represents an external trigger received via Unix domain socket.
// Signals are action-based — they carry a named action, not freeform instructions.
type Signal struct {
	// Source identifies the system sending the signal (e.g., "agentchat-mcp", "camera-script").
	// Must match a registered MCP source or be empty for user-defined actions.
	Source string `json:"source"`

	// Action is the registered action name (e.g., "check_messages", "motion_detected").
	// This must match either:
	//   - A user-defined action from config
	//   - An MCP self-declared action (source + action pair must match)
	// Unknown actions are rejected.
	Action string `json:"action"`

	// Timestamp is Unix millis when the signal was sent.
	Timestamp int64 `json:"timestamp,omitempty"`

	// Channel is the chat channel to inject the message into (e.g., "telegram", "discord").
	// If empty, "signal" is used.
	Channel string `json:"channel,omitempty"`

	// ChatID is the specific conversation to target.
	// If empty, "default" is used.
	ChatID string `json:"chat_id,omitempty"`

	// Metadata holds optional structured data for logging/auditing only.
	// NEVER exposed to the agent or used in response text.
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// Registry tracks allowed signal actions from both user config and MCP self-declaration.
type Registry struct {
	mu       sync.RWMutex
	actions  map[string]*registeredAction // action name → details
	mcpNames map[string][]string          // mcp source → list of declared actions
}

type registeredAction struct {
	config     *config.SignalActionConfig
	mcpSource  string // empty for user-defined actions
	mcpAction  string // the action name as declared by the MCP
	response   string // response template
}

// NewRegistry creates a signal registry with user-defined actions from config.
func NewRegistry(userActions map[string]config.SignalActionConfig) *Registry {
	r := &Registry{
		actions:  make(map[string]*registeredAction),
		mcpNames: make(map[string][]string),
	}
	for name, ac := range userActions {
		resp := ac.Response
		if resp == "" {
			resp = fmt.Sprintf("Signal received: %s", name)
		}
		r.actions[name] = &registeredAction{
			config:   &ac,
			response: resp,
		}
	}
	return r
}

// RegisterMCP registers actions declared by an MCP server at startup.
func (r *Registry) RegisterMCP(source string, actions []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Remove old registrations for this source
	if oldActions, ok := r.mcpNames[source]; ok {
		for _, a := range oldActions {
			// Only remove if it was registered by this MCP source
			if entry, exists := r.actions[a]; exists && entry.mcpSource == source {
				delete(r.actions, a)
			}
		}
	}

	r.mcpNames[source] = actions
	for _, a := range actions {
		// Don't overwrite user-defined actions
		if _, exists := r.actions[a]; exists {
			log.Printf("Signal: MCP action %q from %q skipped (user-defined action takes priority)", a, source)
			continue
		}
		r.actions[a] = &registeredAction{
			mcpSource: source,
			mcpAction: a,
			response:  fmt.Sprintf("External signal from %s: action %s triggered", source, a),
		}
	}
	log.Printf("Signal: registered MCP source %q with %d actions: %s", source, len(actions), strings.Join(actions, ", "))
}

// IsAllowed checks if an action is registered (from user config or MCP).
func (r *Registry) IsAllowed(action string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.actions[action]
	return ok
}

// GetResponse returns the response template for a given action.
// Returns empty string if action is not registered.
func (r *Registry) GetResponse(action string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entry, ok := r.actions[action]; ok {
		return entry.response
	}
	return ""
}

// GetSource returns the MCP source for an action, or empty string if user-defined.
func (r *Registry) GetSource(action string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entry, ok := r.actions[action]; ok {
		return entry.mcpSource
	}
	return ""
}

// ListActions returns all registered action names.
func (r *Registry) ListActions() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.actions))
	for name := range r.actions {
		names = append(names, name)
	}
	return names
}

// Listener accepts external signals on a Unix domain socket and injects
// them as Inbound messages into the chat hub.
type Listener struct {
	socketPath string
	hub        *chat.Hub
	registry   *Registry
	mu         sync.Mutex
	listener   net.Listener
	running    bool
}

// NewListener creates a new signal listener.
func NewListener(socketPath string, hub *chat.Hub, registry *Registry) *Listener {
	return &Listener{
		socketPath: socketPath,
		hub:        hub,
		registry:   registry,
	}
}

// SocketPath returns the path the listener is configured on.
func (l *Listener) SocketPath() string {
	return l.socketPath
}

// Registry returns the signal action registry (for MCP self-registration).
func (l *Listener) Registry() *Registry {
	return l.registry
}

// Start begins listening for signals on the Unix domain socket.
// It blocks until the context is cancelled.
func (l *Listener) Start(ctx context.Context) error {
	l.mu.Lock()
	// Ensure the directory exists
	dir := filepath.Dir(l.socketPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		l.mu.Unlock()
		return fmt.Errorf("signal: failed to create socket directory %s: %w", dir, err)
	}

	// Remove stale socket file
	os.Remove(l.socketPath)

	listener, err := net.Listen("unix", l.socketPath)
	if err != nil {
		l.mu.Unlock()
		return fmt.Errorf("signal: failed to listen on %s: %w", l.socketPath, err)
	}
	l.listener = listener
	l.running = true
	l.mu.Unlock()

	// Set socket permissions to be readable/writable by owner and group
	os.Chmod(l.socketPath, 0660)

	log.Printf("Signal: listening on %s (registered actions: %s)", l.socketPath, strings.Join(l.registry.ListActions(), ", "))

	// Accept connections in a goroutine, shutdown on context cancel
	go func() {
		<-ctx.Done()
		l.mu.Lock()
		l.running = false
		if l.listener != nil {
			l.listener.Close()
		}
		l.mu.Unlock()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			l.mu.Lock()
			running := l.running
			l.mu.Unlock()
			if !running {
				return nil // shutdown
			}
			log.Printf("Signal: accept error: %v", err)
			continue
		}
		go l.handleConnection(conn)
	}
}

// handleConnection reads a signal from a Unix socket connection,
// validates the action against the registry, and injects a safe response
// into the hub. The raw signal payload is NEVER exposed to the agent.
func (l *Listener) handleConnection(conn net.Conn) {
	defer conn.Close()

	// Set a read deadline to prevent hanging connections
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	buf := make([]byte, 65536) // 64KB max signal size
	n, err := conn.Read(buf)
	if err != nil {
		log.Printf("Signal: read error: %v", err)
		return
	}

	var sig Signal
	if err := json.Unmarshal(buf[:n], &sig); err != nil {
		log.Printf("Signal: invalid JSON: %v", err)
		conn.Write([]byte(`{"status":"error","error":"invalid JSON"}`))
		return
	}

	// Validate required fields
	if sig.Action == "" {
		log.Printf("Signal: missing action field, ignoring")
		conn.Write([]byte(`{"status":"error","error":"action is required"}`))
		return
	}

	if sig.Source == "" {
		log.Printf("Signal: missing source field, ignoring")
		conn.Write([]byte(`{"status":"error","error":"source is required"}`))
		return
	}

	// Validate action against registry
	if !l.registry.IsAllowed(sig.Action) {
		log.Printf("Signal: unknown action %q from source %q, rejecting", sig.Action, sig.Source)
		conn.Write([]byte(fmt.Sprintf(`{"status":"error","error":"unknown action: %s"}`, sig.Action)))
		return
	}

	// Get the safe response template
	response := l.registry.GetResponse(sig.Action)

	// Apply defaults for routing
	channel := sig.Channel
	if channel == "" {
		channel = "signal"
	}
	chatID := sig.ChatID
	if chatID == "" {
		chatID = "default"
	}

	// Log the signal for audit purposes
	log.Printf("Signal: accepted action %q from source %q (channel=%s, chatID=%s)", sig.Action, sig.Source, channel, chatID)

	// Build the inbound message — ONLY the safe response template is injected
	// Never expose raw signal content, metadata, or any freeform text to the agent
	inbound := chat.Inbound{
		Channel:   channel,
		SenderID:  "signal:" + sig.Source,
		ChatID:    chatID,
		Content:   response,
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"signal_source": sig.Source,
			"signal_action": sig.Action,
		},
	}

	// Inject into the hub
	select {
	case l.hub.In <- inbound:
		log.Printf("Signal: injected action %q into %s:%s", sig.Action, channel, chatID)
		conn.Write([]byte(`{"status":"ok"}`))
	default:
		log.Printf("Signal: hub inbound channel full, dropping signal")
		conn.Write([]byte(`{"status":"error","error":"hub channel full"}`))
	}
}

// SendSignal is a helper that sends a signal to a Unix domain socket.
func SendSignal(socketPath string, sig Signal) error {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("signal: failed to connect to %s: %w", socketPath, err)
	}
	defer conn.Close()

	// Set timestamp if not provided
	if sig.Timestamp == 0 {
		sig.Timestamp = time.Now().UnixMilli()
	}

	data, err := json.Marshal(sig)
	if err != nil {
		return fmt.Errorf("signal: failed to marshal signal: %w", err)
	}

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("signal: failed to write: %w", err)
	}

	// Read response
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		if !strings.Contains(err.Error(), "timeout") {
			return nil
		}
		return nil
	}

	var resp map[string]string
	if err := json.Unmarshal(buf[:n], &resp); err == nil {
		if resp["status"] != "ok" {
			return fmt.Errorf("signal: server returned %s: %s", resp["status"], resp["error"])
		}
	}

	return nil
}

// DefaultSocketPath returns the default Unix socket path for the given workspace.
func DefaultSocketPath(workspace string) string {
	return filepath.Join(workspace, ".picobot", "signals.sock")
}
