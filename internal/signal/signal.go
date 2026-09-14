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

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/mcp"
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
	// If empty, the last known channel or default is used.
	Channel string `json:"channel,omitempty"`

	// ChatID is the specific conversation to target.
	// If empty, the last known chatID or default is used.
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
	config    *config.SignalActionConfig
	mcpSource string // empty for user-defined actions
	mcpAction string // the action name as declared by the MCP
	response  string // response template
	silent    bool   // suppress channel reply
	// hasExplicitSilent distinguishes MCP declarations that carried an
	// explicit silent flag from those that didn't (the latter default to
	// silent so MCP wake-ups never spam the channel).
	hasExplicitSilent bool
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
			silent:   ac.Silent,
		}

	}
	return r
}

// SignalDeclaration is the rich form of an MCP server's signal action
// declaration (type alias of mcp.SignalAction — signal imports mcp, mcp has
// no internal imports, so no cycle).
type SignalDeclaration = mcp.SignalAction

// RegisterMCP registers plain action names for an MCP source (compat).
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

// RegisterMCPDecls registers rich signal declarations from an MCP server
// (captured from its initialize result). Re-registering a source replaces
// its previous declarations. User-defined actions always take priority —
// an MCP declaration that collides with a config action is skipped.
func (r *Registry) RegisterMCPDecls(source string, decls []SignalDeclaration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	names := make([]string, 0, len(decls))
	valid := decls[:0:0]
	for _, d := range decls {
		if d.Name == "" {
			continue
		}
		valid = append(valid, d)
		names = append(names, d.Name)
	}

	// Remove old registrations for this source
	if oldActions, ok := r.mcpNames[source]; ok {
		for _, a := range oldActions {
			// Only remove if it was registered by this MCP source
			if entry, exists := r.actions[a]; exists && entry.mcpSource == source {
				delete(r.actions, a)
			}
		}
	}

	r.mcpNames[source] = names
	for _, d := range valid {
		// Don't overwrite user-defined actions
		if entry, exists := r.actions[d.Name]; exists && entry.mcpSource == "" {
			log.Printf("Signal: MCP action %q from %q skipped (user-defined action takes priority)", d.Name, source)
			continue
		}
		resp := d.Response
		if resp == "" {
			resp = fmt.Sprintf("External signal from %s: action %s triggered", source, d.Name)
		}
		desc := d.Description
		r.actions[d.Name] = &registeredAction{
			mcpSource:         source,
			mcpAction:         d.Name,
			response:          resp,
			silent:            d.Silent,
			hasExplicitSilent: d.Silent,
		}
		if desc != "" {
			log.Printf("Signal: registered %q from %q — %s", d.Name, source, desc)
		}
	}
	if len(names) > 0 {
		log.Printf("Signal: registered MCP source %q with %d actions: %s", source, len(names), strings.Join(names, ", "))
	}
}

// IsAllowed checks if an action is registered (from user config or MCP)
// AND, for MCP-declared actions, that the sender's source matches the
// declaring server (server A cannot fire server B's action).
func (r *Registry) IsAllowed(action, source string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.actions[action]
	if !ok {
		return false
	}
	if entry.mcpSource != "" && entry.mcpSource != source {
		return false
	}
	return true
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

// GetSilentResponse returns the silent flag for an action (MCP-declared
// actions default to silent so they never spam the channel; config
// actions use their explicit setting).
func (r *Registry) GetSilentResponse(action string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entry, ok := r.actions[action]; ok {
		if entry.mcpSource != "" && !entry.hasExplicitSilent {
			return true
		}
		return entry.silent
	}
	return false
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

// UnregisterMCP removes all declarations from an MCP source (server
// disconnected or removed at runtime). User-defined actions untouched.
func (r *Registry) UnregisterMCP(source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if actions, ok := r.mcpNames[source]; ok {
		for _, a := range actions {
			if entry, exists := r.actions[a]; exists && entry.mcpSource == source {
				delete(r.actions, a)
			}
		}
		delete(r.mcpNames, source)
		log.Printf("Signal: unregistered MCP source %q (%d actions)", source, len(actions))
	}
}

// IsSilent reports whether a signal action should suppress channel replies.
func (r *Registry) IsSilent(action string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entry, ok := r.actions[action]; ok {
		return entry.silent
	}
	return false
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

	// Configured defaults for signal routing (from SignalConfig.DefaultChannel/DefaultChatID)
	defaultChannel string
	defaultChatID  string

	// Last known real channel/chatID for routing signals
	// when the signal doesn't specify a target.
	lastMu     sync.RWMutex
	lastChan   string
	lastChatID string

	// Per-source routing bindings: MCP source name → (channel, chatID) of
	// the session that most recently called a tool on that server. A session
	// that arms a trigger via a tools/call does so on a specific server, so
	// signals from that server should return to that session, not to
	// whichever chat messaged most recently. Populated by the agent loop
	// after successful MCP tool calls.
	sourceMu      sync.RWMutex
	sourceTargets map[string]sourceTarget

	// persistPath, when set, survives source bindings across restarts.
	persistPath string
}

// sourceTarget is one per-source routing binding.
type sourceTarget struct {
	Channel string `json:"channel"`
	ChatID  string `json:"chat_id"`
}

// NewListener creates a new signal listener.
func NewListener(socketPath string, hub *chat.Hub, registry *Registry, defaultChannel, defaultChatID string) *Listener {
	return &Listener{
		socketPath:     socketPath,
		hub:            hub,
		registry:       registry,
		defaultChannel: defaultChannel,
		defaultChatID:  defaultChatID,
		sourceTargets:  map[string]sourceTarget{},
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

// SetLastTarget records the most recent real channel/chatID pair.
// Called by the agent loop whenever it processes a non-signal message.
func (l *Listener) SetLastTarget(channel, chatID string) {
	l.lastMu.Lock()
	defer l.lastMu.Unlock()
	l.lastChan = channel
	l.lastChatID = chatID
}

// getLastTarget returns the most recent real channel/chatID pair.
func (l *Listener) getLastTarget() (string, string) {
	l.lastMu.RLock()
	defer l.lastMu.RUnlock()
	return l.lastChan, l.lastChatID
}

// BindSource records that the session at (channel, chatID) most recently
// called a tool on the named MCP server. Signals originating from that
// server route to this session before falling back to last-known/default.
func (l *Listener) BindSource(source, channel, chatID string) {
	if source == "" || channel == "" || chatID == "" {
		return
	}
	l.sourceMu.Lock()
	defer l.sourceMu.Unlock()
	l.sourceTargets[source] = sourceTarget{Channel: channel, ChatID: chatID}
	l.persistLocked()
}

// UnbindSource removes a source binding (e.g., when its MCP server is removed).
func (l *Listener) UnbindSource(source string) {
	l.sourceMu.Lock()
	defer l.sourceMu.Unlock()
	if _, ok := l.sourceTargets[source]; !ok {
		return
	}
	delete(l.sourceTargets, source)
	l.persistLocked()
}

// getSourceTarget returns the bound target for a source, if any.
func (l *Listener) getSourceTarget(source string) (string, string, bool) {
	l.sourceMu.RLock()
	defer l.sourceMu.RUnlock()
	t, ok := l.sourceTargets[source]
	return t.Channel, t.ChatID, ok
}

// SetPersistencePath enables disk persistence for per-source routing
// bindings at the given path (JSON, atomic write). Follows the same pattern
// as cron_jobs.json / background_jobs.json.
func (l *Listener) SetPersistencePath(path string) {
	l.sourceMu.Lock()
	defer l.sourceMu.Unlock()
	// No immediate persist: at startup the in-memory map may be empty and
	// must not clobber the file before loadBindings() reads it. Writes
	// happen on mutation only.
	l.persistPath = path
}

// persistLocked writes bindings to disk; caller must hold sourceMu.
func (l *Listener) persistLocked() {
	if l.persistPath == "" {
		return
	}
	data, err := json.Marshal(l.sourceTargets)
	if err != nil {
		return
	}
	tmp := l.persistPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, l.persistPath)
}

// loadBindings restores persisted bindings from disk. Called at startup.
func (l *Listener) loadBindings() {
	if l.persistPath == "" {
		return
	}
	data, err := os.ReadFile(l.persistPath)
	if err != nil {
		return // missing file = fresh start
	}
	var m map[string]sourceTarget
	if err := json.Unmarshal(data, &m); err != nil {
		return // corrupt = fresh start
	}
	l.sourceMu.Lock()
	defer l.sourceMu.Unlock()
	for k, v := range m {
		if k != "" && v.Channel != "" && v.ChatID != "" {
			l.sourceTargets[k] = v
		}
	}
}

// Start begins listening for signals on the Unix domain socket.
// It blocks until the context is cancelled.
// Bind creates the Unix domain socket and starts listening without yet
// accepting connections. It is safe to call before any MCP child process
// is spawned: the socket file exists the moment Bind returns, so bridges
// receiving GINO_SIGNAL_SOCKET in their environment can dial immediately
// instead of racing the gateway's slower construction path.
func (l *Listener) Bind() error {
	// Restore per-source routing bindings persisted by a previous run.
	l.loadBindings()

	l.mu.Lock()
	// Ensure the directory exists
	dir := filepath.Dir(l.socketPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		l.mu.Unlock()
		return fmt.Errorf("signal: failed to create socket directory %s: %w", dir, err)
	}

	// Remove stale socket file
	if err := os.Remove(l.socketPath); err != nil && !os.IsNotExist(err) {
		log.Printf("Signal: remove stale socket: %v", err)
	}

	listener, err := net.Listen("unix", l.socketPath)
	if err != nil {
		l.mu.Unlock()
		return fmt.Errorf("signal: failed to listen on %s: %w", l.socketPath, err)
	}
	l.listener = listener
	l.running = true
	l.mu.Unlock()

	// Set socket permissions to be readable/writable by owner and group
	if err := os.Chmod(l.socketPath, 0660); err != nil {
		log.Printf("Signal: chmod socket: %v", err)
	}

	log.Printf("Signal: listening on %s (registered actions: %s, default: %s:%s)", l.socketPath, strings.Join(l.registry.ListActions(), ", "), l.defaultChannel, l.defaultChatID)
	return nil
}

// Start begins listening for signals on the Unix domain socket.
// It blocks until the context is cancelled. Deprecated in favor of
// Bind + Serve for callers that must create the socket before spawning
// MCP children.
func (l *Listener) Start(ctx context.Context) error {
	if err := l.Bind(); err != nil {
		return err
	}
	return l.Serve(ctx)
}

// Serve accepts connections on an already-bound listener. It blocks until
// the context is cancelled.
func (l *Listener) Serve(ctx context.Context) error {
	listener := l.listener
	if listener == nil {
		return fmt.Errorf("signal: Serve called before Bind")
	}

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
	if !l.registry.IsAllowed(sig.Action, sig.Source) {
		log.Printf("Signal: unknown action %q from source %q, rejecting", sig.Action, sig.Source)
		conn.Write([]byte(fmt.Sprintf(`{"status":"error","error":"unknown action: %s"}`, sig.Action)))
		return
	}

	// Get the safe response template
	response := l.registry.GetResponse(sig.Action)

	// Resolve channel/chatID with full fallback chain:
	// 1. Explicit values from signal
	// 2. Per-source binding: the session that most recently called a tool
	//    on the originating MCP server (the session that armed the trigger)
	// 3. Last known real channel/chatID (from previous non-signal messages)
	// 4. Config defaults (SignalConfig.DefaultChannel/DefaultChatID)
	channel := sig.Channel
	chatID := sig.ChatID
	if channel == "" || chatID == "" {
		if srcChan, srcID, ok := l.getSourceTarget(sig.Source); ok {
			if channel == "" {
				channel = srcChan
			}
			if chatID == "" {
				chatID = srcID
			}
		}
	}
	if channel == "" || chatID == "" {
		lastChan, lastID := l.getLastTarget()
		if channel == "" {
			channel = lastChan
		}
		if chatID == "" {
			chatID = lastID
		}
	}
	if channel == "" {
		channel = l.defaultChannel
	}
	if chatID == "" {
		chatID = l.defaultChatID
	}

	// Final fallback — should rarely happen
	if channel == "" {
		channel = "signal"
	}
	if chatID == "" {
		chatID = "default"
	}

	// Log the signal for audit purposes
	log.Printf("Signal: accepted action %q from source %q → routing to %s:%s (signal had channel=%q, chatID=%q, default=%s:%s)", sig.Action, sig.Source, channel, chatID, sig.Channel, sig.ChatID, l.defaultChannel, l.defaultChatID)

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
			"signal_silent": l.registry.IsSilent(sig.Action),
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
	return filepath.Join(workspace, ".gino", "signals.sock")
}
