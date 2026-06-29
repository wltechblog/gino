package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wltechblog/gino/internal/brain"
	"github.com/wltechblog/gino/internal/agent/memory"
	"github.com/wltechblog/gino/internal/agent/tools"
	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/cron"
	"github.com/wltechblog/gino/internal/mcp"
	"github.com/wltechblog/gino/internal/providers"
	"github.com/wltechblog/gino/internal/session"
)

var rememberRE = regexp.MustCompile(`(?i)^remember(?:\s+to)?\s+(.+)$`)

// stopCommands are message prefixes that trigger immediate cancellation
// of the current turn for a session.
var stopCommands = []string{"/stop", "/cancel", "/abort"}

// trimTurnMessages trims the message chain to keep it within maxMsgs.
// It preserves: system prompt, the last assistant text response (so the LLM
// remembers what it just told the user), a small window of recent session
// history, the current user message, and the most recent tool-call exchanges.
//
// Message chain layout (built by BuildMessages + processTurn):
//
//	[0]              system prompt
//	[1..userMsgIdx)  session history (alternating user/assistant)
//	[userMsgIdx]     current user message
//	[userMsgIdx+1..] tool-call exchanges from this turn
//
// Trimming strategy:
//  1. Always keep system[0] and user[userMsgIdx].
//  2. Protect the last assistant text response (non-tool-call) so the LLM
//     remembers what it just told the user — this prevents "I forgot the list".
//  3. Reserve up to 20% of the budget for recent session history.
//  4. Fill the rest with the most recent tool exchanges from the tail.
//  5. Drop orphaned "tool" role messages at any seam (they require a
//     preceding assistant tool_calls message to be valid).
func trimTurnMessages(messages []providers.Message, userMsgIdx int, maxMsgs int) []providers.Message {
	if len(messages) <= maxMsgs {
		return messages
	}

	// Defensive: clamp userMsgIdx to valid range.
	if userMsgIdx >= len(messages) {
		userMsgIdx = len(messages) - 1
	}
	if userMsgIdx < 0 {
		userMsgIdx = 0
	}

	// Find the last assistant message with actual text content (not just tool_calls).
	// This is typically the LLM's most recent substantive response to the user
	// (e.g., "here's the list of items...").  We must keep it so the LLM doesn't
	// forget what it just said when the user references it.
	lastAssistantTextIdx := -1
	for i := userMsgIdx - 1; i >= 1; i-- {
		if messages[i].Role == "assistant" && messages[i].Content != "" {
			lastAssistantTextIdx = i
			break
		}
	}

	// Fast path: if the only messages are system + user, nothing to trim from history.
	if userMsgIdx <= 1 {
		result := make([]providers.Message, 0, maxMsgs)
		result = append(result, messages[0])
		result = append(result, messages[userMsgIdx])
		used := 2

		// Preserve last assistant text if found after userMsgIdx (shouldn't happen in fast path, but safe).
		tailBudget := maxMsgs - used
		tailStart := len(messages) - tailBudget
		if tailStart <= userMsgIdx {
			tailStart = userMsgIdx + 1
		}
		tail := messages[tailStart:]
		skip := 0
		for skip < len(tail) && tail[skip].Role == "tool" {
			skip++
		}
		result = append(result, tail[skip:]...)
		trimmed := len(messages) - len(result)
		log.Printf("Turn context: trimmed %d messages (was %d, now %d)", trimmed, len(messages), len(result))
		return result
	}

	// --- Normal path: we have session history to preserve ---

	result := make([]providers.Message, 0, maxMsgs)
	result = append(result, messages[0])         // system
	result = append(result, messages[userMsgIdx]) // user
	used := 2

	// Always preserve the last assistant text response.
	if lastAssistantTextIdx >= 0 {
		result = append(result, messages[lastAssistantTextIdx])
		used++
	}

	// Reserve 20% of remaining budget for recent session history.
	historyBudget := (maxMsgs - used) / 5
	if historyBudget < 1 {
		historyBudget = 1
	}

	// Collect recent history entries (user/assistant roles only) walking
	// backwards from just before the current user message, but skip the
	// lastAssistantTextIdx since we already preserved it.
	var historyWindow []providers.Message
	historyCount := 0
	for i := userMsgIdx - 1; i >= 1 && historyCount < historyBudget; i-- {
		if i == lastAssistantTextIdx {
			continue // already preserved
		}
		if messages[i].Role == "user" || messages[i].Role == "assistant" {
			historyWindow = append(historyWindow, messages[i])
			historyCount++
		}
	}
	// Reverse to restore chronological order.
	for l, r := 0, len(historyWindow)-1; l < r; l, r = l+1, r-1 {
		historyWindow[l], historyWindow[r] = historyWindow[r], historyWindow[l]
	}
	result = append(result, historyWindow...)
	used += len(historyWindow)

	// Fill remaining budget with the most recent tool exchanges from the tail.
	tailBudget := maxMsgs - used
	tailStart := len(messages) - tailBudget
	if tailStart <= userMsgIdx {
		tailStart = userMsgIdx + 1
	}
	tail := messages[tailStart:]

	// Skip orphaned tool results at the start of the tail (they need a
	// preceding assistant tool_calls message to be valid).
	skip := 0
	for skip < len(tail) && tail[skip].Role == "tool" {
		skip++
	}
	result = append(result, tail[skip:]...)

	trimmed := len(messages) - len(result)
	log.Printf("Turn context: trimmed %d messages (was %d, now %d, kept %d history entries, protected last assistant at idx %d)",
		trimmed, len(messages), len(result), len(historyWindow), lastAssistantTextIdx)
	return result
}

// truncateToolResult caps a tool result string to maxChars.
func truncateToolResult(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + fmt.Sprintf("\n... [truncated %d chars]", len(s)-maxChars)
}

// toolCallRecord captures a single tool invocation for session continuity.
type toolCallRecord struct {
	Name   string
	Args   map[string]interface{}
	Result string
}

// summarizeToolCalls builds a brief summary of tool calls made during a turn,
// so that the next "continue" turn can pick up where things left off instead of
// re-reading all the same files from scratch.
func summarizeToolCalls(records []toolCallRecord) string {
	if len(records) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Previous turn made %d tool call(s):", len(records)))
	for _, r := range records {
		argSummary := ""
		switch r.Name {
		case "filesystem":
			if action, ok := r.Args["action"].(string); ok {
				if path, ok := r.Args["path"].(string); ok {
					argSummary = fmt.Sprintf(" %s %s", action, path)
				}
			}
		case "exec":
			if cmd, ok := r.Args["command"].([]interface{}); ok {
				parts := make([]string, len(cmd))
				for i, c := range cmd {
					parts[i], _ = c.(string)
				}
				argSummary = " " + strings.Join(parts, " ")
			}
		default:
			b, _ := json.Marshal(r.Args)
			if len(b) > 80 {
				b = b[:80]
			}
			argSummary = " " + string(b)
		}
		resultSummary := "ok"
		if r.Result != "" {
			if len(r.Result) > 120 {
				resultSummary = r.Result[:120] + "..."
			} else {
				resultSummary = r.Result
			}
		}
		fmt.Fprintf(&sb, "\n  %s%s → %s", r.Name, argSummary, resultSummary)
	}
	sb.WriteString("]")
	return sb.String()
}

// captureToolMemory adds key tool results to short-term memory so the ranker
// has relevant context for future queries. Not all tool results are worth
// storing — we focus on high-signal tools like filesystem reads, web fetches,
// and exec outputs.
func (a *AgentLoop) captureToolMemory(toolName, result string) {
	if a.memory == nil || result == "" || strings.HasPrefix(result, "(tool error)") {
		return
	}

	// Only capture from tools that produce useful context
	switch toolName {
	case "filesystem", "web", "web_search", "exec":
		// Truncate to keep short-term memory items manageable
		text := result
		if len(text) > 500 {
			text = text[:500]
		}
		a.memory.AddShort(fmt.Sprintf("[%s] %s", toolName, text))
	}
}

// extractTurnMemory runs a background LLM call to extract facts worth remembering
// from the completed turn. It runs in a goroutine so it doesn't delay the response.
func (a *AgentLoop) extractTurnMemory(userMsg, assistantReply string, toolCalls []toolCallRecord, channel, senderID string) {
	if a.memory == nil || a.provider == nil {
		return
	}

	// Build a compact summary of what happened in this turn
	var sb strings.Builder
	sb.WriteString("User: ")
	if len(userMsg) > 500 {
		sb.WriteString(userMsg[:500] + "...")
	} else {
		sb.WriteString(userMsg)
	}
	sb.WriteString("\n\nAssistant: ")
	if len(assistantReply) > 800 {
		sb.WriteString(assistantReply[:800] + "...")
	} else {
		sb.WriteString(assistantReply)
	}
	if len(toolCalls) > 0 {
		sb.WriteString("\n\nTools used:\n")
		for i, tc := range toolCalls {
			if i >= 5 {
				sb.WriteString("- ... (more tools used)\n")
				break
			}
			resultSummary := "ok"
			if len(tc.Result) > 200 {
				resultSummary = tc.Result[:200] + "..."
			} else if tc.Result != "" {
				resultSummary = tc.Result
			}
			fmt.Fprintf(&sb, "- %s → %s\n", tc.Name, resultSummary)
		}
	}

	prompt := `You are a memory extraction system. Given a completed conversation turn, extract any facts worth remembering for future turns.

Extract ONLY:
1. User preferences, decisions, or instructions given
2. Important file paths, URLs, or identifiers discovered
3. Key results or conclusions (bug found, fix applied, config changed)
4. Project details or values that will matter later

Output one fact per line starting with "- ". If nothing is worth remembering, output "NONE". Be very concise — each fact should be a single line under 100 chars.`

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := a.provider.Chat(ctx, []providers.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: sb.String()},
	}, nil, a.model)
	if err != nil {
		log.Printf("Turn memory extraction failed: %v", err)
		return
	}

	facts := strings.TrimSpace(resp.Content)
	if facts == "NONE" || facts == "" {
		return
	}

	// Save to today's notes with a turn-extraction marker (global memory)
	entry := fmt.Sprintf("[turn-extract] %s", facts)
	if err := a.memory.AppendToday(entry); err != nil {
		log.Printf("Failed to save turn-extracted facts: %v", err)
	} else {
		log.Printf("Turn memory: extracted facts from turn (%d chars)", len(facts))
	}

	// For non-owner channels (Discord), also ingest into per-user brain source
	// so each user's memories are isolated and searchable independently.
	if a.brain != nil && senderID != "" && channel != "cli" && channel != "telegram" {
		userSource := fmt.Sprintf("user:%s:%s", channel, senderID)
		lines := strings.Split(facts, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "- ") || len(line) < 4 {
				continue
			}
			text := strings.TrimPrefix(line, "- ")
			slug := memorySlug(text)
			now := time.Now().UTC().Format("2006-01-02T15:04:05")
			slug = now + "-" + slug

			page := brain.Page{
				SourceID: userSource,
				Slug:     slug,
				Type:     "note",
				Title:    text,
				Content:  fmt.Sprintf("[%s] %s", now, text),
				Metadata: map[string]string{
					"channel":  channel,
					"sender":   senderID,
					"extracted": "true",
				},
			}
			if _, err := a.brain.IngestPage(context.Background(), page); err != nil {
				log.Printf("Failed to ingest per-user memory for %s: %v", userSource, err)
			}
		}
		log.Printf("Turn memory: ingested %d facts for user source %s", len(lines), userSource)
	}
}

// isStopCommand reports whether the message content is a stop/cancel command.
func isStopCommand(content string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(content))
	for _, cmd := range stopCommands {
		if trimmed == cmd {
			return true
		}
	}
	return false
}

// sendChannelNotification delivers a non-blocking status message back to the
// originating channel so the user can see tool progress in real time.
// It is a no-op for system channels (heartbeat, cron) that have no user-facing chat.
func sendChannelNotification(hub *chat.Hub, channel, chatID, content string) {
	if isSystemChannel(channel) {
		return
	}
	out := chat.Outbound{Channel: channel, ChatID: chatID, Content: content}
	select {
	case hub.Out <- out:
	default:
		log.Println("sendChannelNotification: outbound channel full, dropping notification")
	}
}

// isSystemChannel reports whether a channel is a background/system trigger
// (heartbeat, cron) rather than an interactive user-facing channel.
// Messages from system channels are processed statelessly: no session history
// is loaded as context and nothing is written back to disk.  This prevents the
// heartbeat session file from growing unboundedly and keeps each invocation's
// context window small.
func isSystemChannel(channel string) bool {
	switch channel {
	case "heartbeat", "cron":
		return true
	default:
		return false
	}
}

// AgentLoop is the core processing loop; it holds an LLM provider, tools, sessions and context builder.
type AgentLoop struct {
	hub                    *chat.Hub
	provider               providers.LLMProvider
	tools                  *tools.Registry
	sessions               *session.SessionManager
	checkpoints            *CheckpointManager
	context                *ContextBuilder
	memory                 *memory.MemoryStore
	brain                  *brain.Brain
	model                  string
	maxIterations          int
	maxTurnMessages        int
	maxToolResultChars     int
	running                bool
	mcpClients             []*mcp.Client
	mcpConfigs             map[string]config.MCPServerConfig
	tokenStore             *mcp.TokenStore
	enableToolActivity     bool
	enableToolCallMessages bool
	signalSocketPath       string // GINO_SIGNAL_SOCKET injected into MCP child processes
	signalListener         SignalTargetRecorder // optional: records last real channel for signal routing
	compactor              *compactor           // nil = use legacy trimTurnMessages

	// Per-session turn management for async processing and cancellation.
	mu       sync.Mutex
	active   map[string]*activeTurn // sessionKey -> active turn (nil = idle)

	// bgWG tracks background goroutines (e.g. turn memory extraction) so
	// tests can wait for them to finish before cleaning up temp dirs.
	bgWG sync.WaitGroup
}

// activeTurn tracks an in-flight turn for cancellation.
type activeTurn struct {
	cancel  context.CancelFunc
	done    chan struct{} // closed when turn completes
	stopped bool         // true if cancelled by /stop
}

// SignalTargetRecorder is implemented by signal.Listener to record the last
// real channel/chatID so that signal-triggered messages can be routed correctly.
type SignalTargetRecorder interface {
	SetLastTarget(channel, chatID string)
}

func NewAgentLoop(b *chat.Hub, provider providers.LLMProvider, model string, maxIterations int, workspace string, scheduler *cron.Scheduler, mcpServers map[string]config.MCPServerConfig, allowedDirs []string, disableTools []string, brainCfg *config.BrainConfig, homeDir string, sandbox config.SandboxConfig, signalSocketPath string, maxTurnMessages int, maxToolResultChars int, compactionCfg *config.CompactionConfig, webCfg config.WebConfig) *AgentLoop {
	if model == "" {
		model = provider.GetDefaultModel()
	}
	if workspace == "" {
		workspace = "."
	}
	reg := tools.NewRegistry()

	// Build disabled tool set for filtering
	disabled := make(map[string]bool, len(disableTools))
	for _, name := range disableTools {
		disabled[name] = true
	}
	register := func(t tools.Tool) {
		if !disabled[t.Name()] {
			reg.Register(t)
		} else {
			log.Printf("Tool %q: disabled via config", t.Name())
		}
	}

	// register default tools
	register(tools.NewMessageTool(b))

	allDirs := append([]string{workspace}, allowedDirs...)

	fsTool, err := tools.NewFilesystemTool(workspace, allDirs)
	if err != nil {
		log.Fatalf("failed to create filesystem tool: %v", err)
	}
	register(fsTool)
	register(tools.NewExecToolWithSandbox(60, workspace, allDirs, sandbox))
	register(tools.NewWebToolWithConfig(webCfg.TimeoutS, webCfg.MaxResponseBytes, webCfg.UserAgent))
	register(tools.NewWebSearchTool())
	register(tools.NewSpawnTool())
	if scheduler != nil {
		register(tools.NewCronTool(scheduler))
	}

	sm := session.NewSessionManager(workspace)

	// Restore sessions from disk on startup so conversation history survives restarts.
	if err := sm.LoadAll(); err != nil {
		log.Printf("warning: failed to load sessions from disk: %v", err)
	} else {
		log.Println("Sessions: restored from disk")
	}

	ctx := NewContextBuilder(workspace, memory.NewLLMRanker(provider, model), 5)
	mem := memory.NewMemoryStoreWithWorkspace(workspace, 100)
	// register memory tools (all share the same store instance)
	register(tools.NewWriteMemoryTool(mem))
	register(tools.NewListMemoryTool(mem))
	register(tools.NewReadMemoryTool(mem))
	register(tools.NewEditMemoryTool(mem))
	register(tools.NewDeleteMemoryTool(mem))

	// register skill management tools (share the workspace os.Root)
	skillMgr := tools.NewSkillManager(fsTool.WorkspaceRoot())
	register(tools.NewCreateSkillTool(skillMgr))
	register(tools.NewListSkillsTool(skillMgr))
	register(tools.NewReadSkillTool(skillMgr))
	register(tools.NewDeleteSkillTool(skillMgr))

	// Connect to configured MCP servers and register their tools.
	var mcpClients []*mcp.Client
	tokenStore := mcp.NewTokenStore(homeDir)
	for name, cfg := range mcpServers {
		var client *mcp.Client
		var err error
		switch {
		case cfg.Command != "":
			mcpEnv := map[string]string{}
			for k, v := range cfg.Env {
				mcpEnv[k] = v
			}
			if signalSocketPath != "" {
				mcpEnv["GINO_SIGNAL_SOCKET"] = signalSocketPath
				mcpEnv["GINO_MCP_ID"] = name
			}
			client, err = mcp.NewStdioClientWithEnv(name, cfg.Command, cfg.Args, mcpEnv)
		case cfg.URL != "":
			client, err = mcp.NewHTTPClientWithOAuth(name, cfg.URL, cfg.Headers, tokenStore)
		default:
			log.Printf("MCP server %q: no command or url configured, skipping", name)
			continue
		}
		if err != nil {
			// If OAuth is required, surface a user-friendly message
			if oauthErr, ok := err.(*mcp.ErrOAuthRequired); ok {
				log.Printf("MCP server %q: OAuth authentication required. Auth URL: %s", name, oauthErr.AuthURL)
				mcp.SetOAuthPending(name, oauthErr)
				continue
			}
			log.Printf("MCP server %q: failed to connect: %v", name, err)
			continue
		}
		mcpClients = append(mcpClients, client)
		for _, tool := range client.Tools() {
			register(tools.NewMCPTool(client, name, tool))
		}
		log.Printf("MCP server %q: registered %d tools", name, len(client.Tools()))
	}

	// Register MCP management tools (callback will be set after AgentLoop is created)
	restartTool := tools.NewMCPRestartTool()
	register(restartTool)
	listMCPTool := tools.NewMCPListTool()
	register(listMCPTool)
	authTool := tools.NewMCPAuthTool()
	register(authTool)

	// Initialize knowledge brain (optional)
	var brainInst *brain.Brain
	if brainCfg != nil && brainCfg.Enabled {
		brainInst = initBrain(homeDir, workspace, brainCfg, provider)
	}
	if brainInst != nil {
		register(tools.NewBrainSearchTool(brainInst))
		register(tools.NewBrainIngestTool(brainInst))
		register(tools.NewBrainEntityTool(brainInst))
		register(tools.NewBrainStatusTool(brainInst))
		register(tools.NewBrainMaintainTool(brainInst))
		ctx.SetBrain(brainInst)
		log.Println("Brain: initialized and tools registered")
	}

	checkpoints := NewCheckpointManager(workspace)

	if maxTurnMessages <= 0 {
		maxTurnMessages = 100
	}
	if maxToolResultChars <= 0 {
		maxToolResultChars = 8000
	}

	// Initialize LLM-based compactor if enabled, otherwise nil (falls back to legacy trim).
	var comp *compactor
	if compactionCfg != nil && compactionCfg.Enabled {
		comp = newCompactor(provider, model, compactionCfg, maxTurnMessages, newMemoryFlusher(provider, model, mem))
		log.Printf("Compaction: enabled (maxCtx=%d, reserve=%d, keepRecent=%d)",
			comp.maxContextTokens, comp.reserveTokens, comp.keepRecentTokens)
	}

	al := &AgentLoop{
		hub:                    b,
		provider:               provider,
		tools:                  reg,
		sessions:               sm,
		checkpoints:            checkpoints,
		context:                ctx,
		memory:                 mem,
		brain:                  brainInst,
		model:                  model,
		maxIterations:          maxIterations,
		maxTurnMessages:        maxTurnMessages,
		maxToolResultChars:     maxToolResultChars,
		mcpClients:             mcpClients,
		mcpConfigs:             mcpServers,
		tokenStore:             tokenStore,
		enableToolActivity:     true,
		enableToolCallMessages: false,
		active:                 make(map[string]*activeTurn),
		compactor:              comp,
	}

	// Wire the MCP management tool callbacks so they can call back into the loop
	restartTool.SetCallback(al.restartMCPServer)
	listMCPTool.SetCallback(al.listMCPServers)
	authTool.SetCallback(al)

	// Wire OAuth notifications into the context builder so pending auth is surfaced
	ctx.SetOAuthNotifier(al.ListPendingOAuth)

	log.Printf("Sandbox mode: %s", sandbox.GetMode())

	return al
}

func (a *AgentLoop) SetToolActivityIndicator(enabled bool) {
	a.enableToolActivity = enabled
}

func (a *AgentLoop) SetToolCallMessages(enabled bool) {
	a.enableToolCallMessages = enabled
}

// SetSignalSocketPath sets the path to the signal Unix socket. When set, this
// path is injected as GINO_SIGNAL_SOCKET into MCP child process environments.
func (a *AgentLoop) SetSignalSocketPath(path string) {
	a.signalSocketPath = path
}

// SetSignalListener sets the signal listener for recording last target.
func (a *AgentLoop) SetSignalListener(l SignalTargetRecorder) {
	a.signalListener = l
}

// Close shuts down all MCP server connections and the brain.
func (a *AgentLoop) Close() {
	for _, c := range a.mcpClients {
		_ = c.Close()
	}
	if a.brain != nil {
		if err := a.brain.Close(); err != nil {
			log.Printf("agent: close brain: %v", err)
		}
	}
}

// StopTurn cancels the active turn for a session key, if one is running.
// Returns true if a turn was cancelled.
func (a *AgentLoop) StopTurn(sessionKey string) bool {
	return a.cancelActiveTurn(sessionKey)
}

// DeleteSession removes a session from the session manager (including on-disk).
func (a *AgentLoop) DeleteSession(sessionKey string) {
	a.sessions.DeleteSession(sessionKey)
}

// cancelActiveTurn cancels the current turn for a session key, if one is running.
// Returns true if a turn was cancelled.
func (a *AgentLoop) cancelActiveTurn(sessionKey string) bool {
	a.mu.Lock()
	at, ok := a.active[sessionKey]
	if !ok || at == nil {
		a.mu.Unlock()
		return false
	}
	at.stopped = true
	at.cancel()
	a.mu.Unlock()

	// Wait for the goroutine to finish (with timeout so we don't hang)
	select {
	case <-at.done:
		log.Printf("Turn for %s stopped successfully", sessionKey)
		return true
	case <-time.After(5 * time.Second):
		log.Printf("Turn for %s: stop signal sent but goroutine did not exit within 5s", sessionKey)
		return true
	}
}

// restartMCPServer shuts down and reconnects a single MCP server by name.
// It returns a summary of what happened.
func (a *AgentLoop) restartMCPServer(serverName string) (string, error) {
	cfg, ok := a.mcpConfigs[serverName]
	if !ok {
		available := make([]string, 0, len(a.mcpConfigs))
		for k := range a.mcpConfigs {
			available = append(available, k)
		}
		return "", fmt.Errorf("unknown MCP server %q; available: %v", serverName, available)
	}

	// Find and close the old client
	var oldClient *mcp.Client
	for _, c := range a.mcpClients {
		if c.Name() == serverName {
			oldClient = c
			break
		}
	}

	// Unregister old MCP tools from the registry
	if oldClient != nil {
		for _, t := range oldClient.Tools() {
			toolName := fmt.Sprintf("mcp_%s_%s", serverName, t.Name)
			a.tools.Unregister(toolName)
		}
		_ = oldClient.Close()
		log.Printf("MCP server %q: closed old connection", serverName)
	}

	// Create new client
	var newClient *mcp.Client
	var err error
	switch {
	case cfg.Command != "":
		mcpEnv := map[string]string{}
		for k, v := range cfg.Env {
			mcpEnv[k] = v
		}
		if a.signalSocketPath != "" {
			mcpEnv["GINO_SIGNAL_SOCKET"] = a.signalSocketPath
			mcpEnv["GINO_MCP_ID"] = serverName
		}
		newClient, err = mcp.NewStdioClientWithEnv(serverName, cfg.Command, cfg.Args, mcpEnv)
	case cfg.URL != "":
		newClient, err = mcp.NewHTTPClientWithOAuth(serverName, cfg.URL, cfg.Headers, a.tokenStore)
	default:
		return "", fmt.Errorf("MCP server %q has no command or URL", serverName)
	}
	if err != nil {
		return "", fmt.Errorf("failed to reconnect MCP server %q: %w", serverName, err)
	}

	// Replace in the clients slice
	newClients := make([]*mcp.Client, 0, len(a.mcpClients))
	replaced := false
	for _, c := range a.mcpClients {
		if c.Name() == serverName {
			newClients = append(newClients, newClient)
			replaced = true
		} else {
			newClients = append(newClients, c)
		}
	}
	if !replaced {
		newClients = append(newClients, newClient)
	}
	a.mcpClients = newClients

	// Register new tools
	for _, t := range newClient.Tools() {
		a.tools.Register(tools.NewMCPTool(newClient, serverName, t))
	}

	toolNames := make([]string, 0, len(newClient.Tools()))
	for _, t := range newClient.Tools() {
		toolNames = append(toolNames, t.Name)
	}

	msg := fmt.Sprintf("MCP server %q restarted successfully, %d tools: %v", serverName, len(newClient.Tools()), toolNames)
	log.Println(msg)
	return msg, nil
}

// listMCPServers returns a summary of all connected MCP servers and their tools.
func (a *AgentLoop) listMCPServers() string {
	infos := make([]tools.MCPClientInfo, 0, len(a.mcpClients))
	for _, c := range a.mcpClients {
		toolNames := make([]string, 0, len(c.Tools()))
		for _, t := range c.Tools() {
			toolNames = append(toolNames, t.Name)
		}
		infos = append(infos, tools.MCPClientInfo{Name: c.Name(), Tools: toolNames})
	}
	return tools.FormatMCPServerList(infos)
}

/*** MCPAuthCallback implementation ***/

// ListPendingOAuth implements tools.MCPAuthCallback.
func (a *AgentLoop) ListPendingOAuth() map[string]string {
	result := make(map[string]string)
	for name, err := range mcp.AllPendingOAuth() {
		result[name] = err.AuthURL
	}
	return result
}

// CompleteOAuth implements tools.MCPAuthCallback.
func (a *AgentLoop) CompleteOAuth(serverName, redirectURL string) error {
	cfg, ok := a.mcpConfigs[serverName]
	if !ok {
		return fmt.Errorf("unknown MCP server %q", serverName)
	}
	if cfg.URL == "" {
		return fmt.Errorf("MCP server %q is not an HTTP server (OAuth not applicable)", serverName)
	}
	if err := mcp.CompleteAuthForServer(serverName, cfg.URL, redirectURL, cfg.Headers, a.tokenStore); err != nil {
		return err
	}
	// Clear the pending OAuth state — auth is done
	mcp.ClearOAuthPending(serverName)
	return nil
}

// ReconnectAfterAuth implements tools.MCPAuthCallback.
func (a *AgentLoop) ReconnectAfterAuth(serverName string) error {
	_, err := a.restartMCPServer(serverName)
	return err
}

// Run starts processing inbound messages. This is a blocking call until context is canceled.
//
// Messages are dispatched to per-session goroutines so that multiple sessions
// (e.g. different Telegram chats) can be processed concurrently. Each session
// gets a cancellable context, allowing /stop to abort the current turn.
func (a *AgentLoop) Run(ctx context.Context) {
	a.running = true
	log.Println("Agent loop started")

	// Recover any interrupted turns from a previous run.
	a.recoverTurns(ctx)

	for a.running {
		select {
		case <-ctx.Done():
			log.Println("Agent loop received shutdown signal")
			a.running = false
			return

		case msg, ok := <-a.hub.In:
			if !ok {
				log.Println("Inbound channel closed, stopping agent loop")
				a.running = false
				return
			}
			a.dispatchMessage(ctx, msg)


		default:
			// idle tick
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// dispatchMessage routes an inbound message to the correct handler.
// Stop commands cancel the active turn; everything else is queued for processing.
// Signal messages use a separate session namespace so they don't interrupt active turns.
func (a *AgentLoop) dispatchMessage(ctx context.Context, msg chat.Inbound) {
	sessionKey := msg.Channel + ":" + msg.ChatID

	// Determine if this is a signal-originated message.
	isSignal := isSignalMessage(msg)

	// For signals, use a separate session namespace so they don't cancel or
	// interfere with the user's active interactive turn. The signal runs in
	// its own isolated session and its results are injected back into the
	// main session after completion.
	signalSessionKey := sessionKey
	if isSignal {
		signalSessionKey = "signal:" + sessionKey
	}

	// Handle /stop — cancel the current turn for this session
	if isStopCommand(msg.Content) {
		if a.cancelActiveTurn(sessionKey) {
			sendChannelNotification(a.hub, msg.Channel, msg.ChatID, "⛔ Stopped.")
		} else {
			sendChannelNotification(a.hub, msg.Channel, msg.ChatID, "Nothing to stop.")
		}
		return
	}

	// Handle /reset — kill all Discord conversations.
	// Only applies to Discord; Telegram and other channels are unaffected.
	if strings.TrimSpace(msg.Content) == "/reset" && msg.Channel == "discord" {
		const discordPrefix = "discord:"
		cancelled := 0
		a.mu.Lock()
		for key, at := range a.active {
			if strings.HasPrefix(key, discordPrefix) && !strings.HasPrefix(key, "signal:") {
				at.stopped = true
				at.cancel()
				delete(a.active, key)
				cancelled++
			}
		}
		a.mu.Unlock()
		deleted := a.sessions.DeleteByPrefix(discordPrefix)
		sendChannelNotification(a.hub, msg.Channel, msg.ChatID, fmt.Sprintf("🗑️ Cleared all %d Discord conversations (cancelled %d active turns).", deleted, cancelled))
		return
	}

	log.Printf("Processing message from %s:%s\n", msg.Channel, msg.SenderID)

	// Quick heuristic: if user asks the agent to remember something explicitly,
	// store it in today's note and reply immediately without calling the LLM.
	trimmed := strings.TrimSpace(msg.Content)
	if matches := rememberRE.FindStringSubmatch(trimmed); len(matches) == 2 {
		note := matches[1]
		if err := a.memory.AppendToday(note); err != nil {
			log.Printf("error appending to memory: %v", err)
		}
		out := chat.Outbound{Channel: msg.Channel, ChatID: msg.ChatID, Content: "OK, I've remembered that."}
		select {
		case a.hub.Out <- out:
		default:
			log.Println("Outbound channel full, dropping message")
		}
		// Only save session for interactive channels, not system triggers.
		if !isSystemChannel(msg.Channel) {
			sess := a.sessions.GetOrCreate(sessionKey)
			sess.AddMessage("user", msg.Content)
			sess.AddMessage("assistant", "OK, I've remembered that.")
			if err := a.sessions.Save(sess); err != nil {
				log.Printf("error saving session: %v", err)
			}
		}
		return
	}

	// Set tool context (so message tool knows channel+chat)
	if mt := a.tools.Get("message"); mt != nil {
		if mtool, ok := mt.(interface{ SetContext(string, string) }); ok {
			mtool.SetContext(msg.Channel, msg.ChatID)
		}
	}
	if ct := a.tools.Get("cron"); ct != nil {
		if ctool, ok := ct.(interface{ SetContext(string, string) }); ok {
			ctool.SetContext(msg.Channel, msg.ChatID)
		}
	}

	// Record last real channel/chatID for signal routing (only for non-signal messages)
	if a.signalListener != nil && !isSystemChannel(msg.Channel) && !isSignal {
		a.signalListener.SetLastTarget(msg.Channel, msg.ChatID)
	}

	// Build messages from session, long-term memory, and recent memory.
	var sess *session.Session
	if isSystemChannel(msg.Channel) {
		sess = &session.Session{Key: signalSessionKey}
	} else {
		sess = a.sessions.GetOrCreate(signalSessionKey)
	}
	memCtx, _ := a.memory.GetMemoryContext()
	memories := a.memory.Recent(5)
	userContent := msg.Content
	if len(msg.Media) > 0 {
		userContent += "\n\n[Attached files saved to:]"
		for _, p := range msg.Media {
			userContent += "\n- " + p
		}
	}
	messages := a.context.BuildMessages(sess.GetHistory(), userContent, msg.Channel, msg.ChatID, msg.SenderID, memCtx, memories)

	// For signals, do NOT cancel the active interactive turn — run in parallel.
	// For regular user messages, cancel any existing turn (new message supersedes old one).
	if isSignal {
		// Only cancel a previous signal turn for this same signal session, not the user's turn.
		a.cancelActiveTurn(signalSessionKey)
	} else {
		a.cancelActiveTurn(sessionKey)
	}

	// Create a cancellable context for this turn
	turnCtx, turnCancel := context.WithCancel(ctx)
	at := &activeTurn{
		cancel: turnCancel,
		done:   make(chan struct{}),
	}

	a.mu.Lock()
	a.active[signalSessionKey] = at
	a.mu.Unlock()

	// Process the turn in a goroutine
	go func() {
		defer close(at.done)
		defer turnCancel()

		result := a.processTurn(turnCtx, at, signalSessionKey, msg, sess, messages)

		// Clean up active turn
		a.mu.Lock()
		delete(a.active, signalSessionKey)
		a.mu.Unlock()

		// If this was a signal turn, inject the result into the main
		// interactive session so the next user message has context about
		// what the signal produced (e.g., a task completion notification).
		if isSignal && result != "" {
			mainSess := a.sessions.GetOrCreate(sessionKey)
			source, _ := msg.Metadata["signal_source"].(string)
			action, _ := msg.Metadata["signal_action"].(string)
			notification := fmt.Sprintf("[Signal notification from %s: %s] %s", source, action, result)
			mainSess.AddMessage("system", notification)
			if err := a.sessions.Save(mainSess); err != nil {
				log.Printf("error saving main session after signal injection: %v", err)
			}
			log.Printf("Signal: injected result into main session %s (%d chars)", sessionKey, len(notification))
		}
	}()
}

// processTurn runs the agent loop for a single message: LLM calls, tool execution, etc.
// It respects the turn's context for cancellation.

// isSignalSilent checks whether the inbound message originated from a silent signal.
func isSignalSilent(msg chat.Inbound) bool {
	if msg.Metadata == nil {
		return false
	}
	silent, _ := msg.Metadata["signal_silent"].(bool)
	return silent
}

// isSignalMessage checks whether the inbound message originated from the signal
// listener (as opposed to a direct user message from Telegram/Discord/CLI).
func isSignalMessage(msg chat.Inbound) bool {
	if msg.Metadata == nil {
		return false
	}
	_, ok := msg.Metadata["signal_action"]
	return ok
}

func (a *AgentLoop) processTurn(ctx context.Context, at *activeTurn, sessionKey string, msg chat.Inbound, sess *session.Session, messages []providers.Message) string {
	iteration := 0
	finalContent := ""
	lastToolResult := ""
	toolDefs := a.tools.Definitions()

	// userMsgIdx is the index of the current user message in the messages slice.
	// BuildMessages always puts it last. We need this for trimTurnMessages.
	userMsgIdx := len(messages) - 1

	// Track tool calls for session continuity (so "continue" doesn't re-read everything).
	var toolCallLog []toolCallRecord

	for iteration < a.maxIterations {
		iteration++

		// Check for cancellation before each iteration
		select {
		case <-ctx.Done():
			if at.stopped {
				return "" // /stop already sent the reply
			}
			finalContent = "Turn cancelled."
			goto done
		default:
		}

		// Trim/compact messages to keep the context window manageable.
		// After each tool iteration the messages slice grows by 2+ messages.
		// Without trimming, 8-10 file reads exhaust most context windows.
		if len(messages) > a.maxTurnMessages {
			if a.compactor != nil && a.compactor.shouldCompact(messages) {
				// LLM-based compaction: summarize old messages, keep recent tail.
				var compactErr error
				messages, compactErr = a.compactor.compact(ctx, messages, userMsgIdx)
				if compactErr != nil {
					log.Printf("Compaction failed, falling back to trim: %v", compactErr)
					// Inject tool call summary before trimming
					if summary := summarizeToolCalls(toolCallLog); summary != "" {
						messages = append(messages, providers.Message{
							Role:    "assistant",
							Content: summary,
						})
					}
					messages = trimTurnMessages(messages, userMsgIdx, a.maxTurnMessages)
				}
			} else {
				// Legacy trim: inject tool call summary, then slice.
				if summary := summarizeToolCalls(toolCallLog); summary != "" {
					messages = append(messages, providers.Message{
						Role:    "assistant",
						Content: summary,
					})
				}
				messages = trimTurnMessages(messages, userMsgIdx, a.maxTurnMessages)
			}
			// Both compact and trim preserve system[0] and user[1]; update index.
			userMsgIdx = 1
		}

		// Checkpoint the current turn state before each LLM invocation.
		if err := a.checkpoints.Save(sessionKey, &ActiveTurn{
			Channel:        msg.Channel,
			ChatID:         msg.ChatID,
			SenderID:       msg.SenderID,
			Content:        msg.Content,
			Messages:       messages,
			Iteration:      iteration,
			LastToolResult: lastToolResult,
		}); err != nil {
			log.Printf("agent: checkpoint save: %v", err)
		}

		resp, err := a.provider.Chat(ctx, messages, toolDefs, a.model)
		if err != nil {
			// Check if it was cancelled
			select {
			case <-ctx.Done():
				if at.stopped {
					return ""
				}
				finalContent = "Turn cancelled."
				goto done
			default:
			}

			log.Printf("provider error: %v", err)
			finalContent = "Sorry, I encountered an error while processing your request."
			break
		}

		if resp.HasToolCalls {
			// append assistant message with tool_calls attached
			messages = append(messages, providers.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
			// execute each tool call and return results with "tool" role
			for _, tc := range resp.ToolCalls {
				// Check for cancellation between tool calls
				select {
				case <-ctx.Done():
					if at.stopped {
						return ""
					}
					finalContent = "Turn cancelled mid-execution."
					goto done
				default:
				}

				argsJSON, _ := json.Marshal(tc.Arguments)
				if a.enableToolCallMessages {
					sendChannelNotification(a.hub, msg.Channel, msg.ChatID,
						fmt.Sprintf("🤖 Running: %s %s", tc.Name, argsJSON))
				}

				start := time.Now()
				res, err := a.tools.Execute(ctx, tc.Name, tc.Arguments)
				elapsed := time.Since(start).Round(time.Millisecond)

				if err != nil {
					if a.enableToolCallMessages {
						sendChannelNotification(a.hub, msg.Channel, msg.ChatID,
							fmt.Sprintf("📢 %s failed (%s): %v", tc.Name, elapsed, err))
					}
					res = "(tool error) " + err.Error()
				} else {
					if a.enableToolCallMessages {
						sendChannelNotification(a.hub, msg.Channel, msg.ChatID,
							fmt.Sprintf("📢 %s done (%s)", tc.Name, elapsed))
					}
				}
				lastToolResult = res

				// Truncate large tool results before adding to the LLM message chain.
				// File reads are the primary source of context bloat — a single file
				// can be 50 KB of text, which quickly fills the context window.
				toolResultForLLM := truncateToolResult(res, a.maxToolResultChars)

				// Record for session continuity
				toolCallLog = append(toolCallLog, toolCallRecord{
					Name:   tc.Name,
					Args:   tc.Arguments,
					Result: toolResultForLLM,
				})

				// Auto-populate short-term memory with tool results so the ranker
				// has useful context for future queries.
				a.captureToolMemory(tc.Name, toolResultForLLM)

				messages = append(messages, providers.Message{Role: "tool", Content: toolResultForLLM, ToolCallID: tc.ID})
			}
			// loop again
			continue
		} else {
			finalContent = resp.Content
			break
		}
	}

done:
	// Turn completed — clear the checkpoint so it won't be recovered.
	if err := a.checkpoints.MarkCompleted(sessionKey); err != nil {
		log.Printf("agent: checkpoint mark completed: %v", err)
	}

	if finalContent == "" && lastToolResult != "" {
		finalContent = lastToolResult
	} else if finalContent == "" {
		finalContent = "I've completed processing but have no response to give."
	}

	// Save session for interactive channels only.
	// When tool calls were made, append a summary to the session copy so that a
	// follow-up "continue" message can pick up where things left off instead of
	// re-reading all the same files from scratch.
	if !isSystemChannel(msg.Channel) {
		sess.AddMessage("user", msg.Content)
		sessionContent := finalContent
		if summary := summarizeToolCalls(toolCallLog); summary != "" {
			sessionContent += "\n\n" + summary
		}
		sess.AddMessage("assistant", sessionContent)
		if err := a.sessions.Save(sess); err != nil {
			log.Printf("error saving session: %v", err)
		}
	}

	// Turn-end memory extraction: if the turn had significant activity (tool calls,
	// long exchanges), run a background LLM call to extract facts worth remembering.
	// This catches things the LLM might forget to explicitly save via write_memory.
	if len(toolCallLog) > 0 && !isSystemChannel(msg.Channel) {
		a.bgWG.Add(1)
		go func() {
			defer a.bgWG.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("extractTurnMemory panic recovered: %v", r)
				}
			}()
			a.extractTurnMemory(msg.Content, finalContent, toolCallLog, msg.Channel, msg.SenderID)
		}()
	}

	// Suppress reply for silent signals unless the agent has something substantive to say.
	// Silent signals (e.g., check_messages) process in the background but shouldn't
	// spam the channel with "no new messages" acknowledgments. The agent's response
	// is still saved to the session for context continuity.
	if isSignalSilent(msg) && iteration == 1 && lastToolResult == "" {
		log.Printf("Silent signal: suppressing reply for %s (no tool activity)", sessionKey)
		return finalContent
	}

	log.Printf("Turn done: sending reply to %s/%s (%d chars, %d iterations)", msg.Channel, msg.ChatID, len(finalContent), iteration)
	out := chat.Outbound{Channel: msg.Channel, ChatID: msg.ChatID, Content: finalContent}
	select {
	case a.hub.Out <- out:
		log.Printf("Turn done: reply queued successfully")
	default:
		log.Printf("WARNING: Outbound channel full, DROPPING reply (%d chars) for %s/%s", len(finalContent), msg.Channel, msg.ChatID)
	}
	return finalContent
}

// recoverTurns scans for any interrupted turns from a previous run and
// re-injects them into the hub for reprocessing. This is called once at
// startup before the main agent loop begins processing new messages.
func (a *AgentLoop) recoverTurns(ctx context.Context) {
	recoveries := a.checkpoints.RecoverAll()
	if len(recoveries) == 0 {
		return
	}

	log.Printf("Agent: recovering %d interrupted turn(s)", len(recoveries))
	for _, r := range recoveries {
		log.Printf("Agent: recovering turn for %s (iteration %d, %d messages, last saved with %d messages in chain)",
			r.Key, r.Turn.Iteration, len(r.Turn.Messages), len(r.Turn.Messages))

		inbound := r.ToInbound()

		if !isSystemChannel(inbound.Channel) {
			sendChannelNotification(a.hub, inbound.Channel, inbound.ChatID,
				"🔄 Recovering from restart — reprocessing your last message...")
		}

		select {
		case a.hub.In <- inbound:
			log.Printf("Agent: re-injected recovered message for %s", r.Key)
		default:
			log.Printf("Agent: inbound channel full, could not recover %s", r.Key)
		}
	}
}

// ProcessDirect sends a message directly to the provider and returns the response.
// It supports tool calling - if the model requests tools, they will be executed.
func (a *AgentLoop) ProcessDirect(content string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Set tool context so message/cron tools know the originating channel,
	// matching what Run() does for hub-based messages.
	if mt := a.tools.Get("message"); mt != nil {
		if mtool, ok := mt.(interface{ SetContext(string, string) }); ok {
			mtool.SetContext("cli", "direct")
		}
	}
	if ct := a.tools.Get("cron"); ct != nil {
		if ctool, ok := ct.(interface{ SetContext(string, string) }); ok {
			ctool.SetContext("cli", "direct")
		}
	}

	// Build full context (bootstrap files, skills, memory) just like the main loop
	memCtx, _ := a.memory.GetMemoryContext()
	memories := a.memory.Recent(5)
	messages := a.context.BuildMessages(nil, content, "cli", "direct", "", memCtx, memories)

	// Support tool calling iterations (similar to main loop)
	var lastToolResult string
	userMsgIdx := len(messages) - 1
	for iteration := 0; iteration < a.maxIterations; iteration++ {
		// Trim/compact messages to keep context manageable
		if len(messages) > a.maxTurnMessages {
			if a.compactor != nil && a.compactor.shouldCompact(messages) {
				var compactErr error
				messages, compactErr = a.compactor.compact(ctx, messages, userMsgIdx)
				if compactErr != nil {
					log.Printf("Compaction failed, falling back to trim: %v", compactErr)
					messages = trimTurnMessages(messages, userMsgIdx, a.maxTurnMessages)
				}
			} else {
				messages = trimTurnMessages(messages, userMsgIdx, a.maxTurnMessages)
			}
			userMsgIdx = 1
		}

		resp, err := a.provider.Chat(ctx, messages, a.tools.Definitions(), a.model)
		if err != nil {
			return "", err
		}

		if !resp.HasToolCalls {
			if resp.Content != "" {
				return resp.Content, nil
			}
			if lastToolResult != "" {
				return lastToolResult, nil
			}
			return resp.Content, nil
		}

		messages = append(messages, providers.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			result, err := a.tools.Execute(ctx, tc.Name, tc.Arguments)
			if err != nil {
				result = "(tool error) " + err.Error()
			}
			lastToolResult = result
			messages = append(messages, providers.Message{Role: "tool", Content: truncateToolResult(result, a.maxToolResultChars), ToolCallID: tc.ID})
		}
	}

	return "Max iterations reached without final response", nil
}

// initBrain initializes the knowledge brain subsystem.
// Tries Ollama first, falls back to remote API, then FTS5-only mode.
// memorySlug creates a URL-safe slug from text for use as a brain page slug.
func memorySlug(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	result := b.String()
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	if len(result) > 80 {
		result = result[:80]
	}
	return strings.Trim(result, "-")
}

func initBrain(homeDir, workspace string, cfg *config.BrainConfig, provider providers.LLMProvider) *brain.Brain {
	dbPath := filepath.Join(homeDir, "brain.db")

	var embedder brain.EmbeddingProvider

	ollamaURL := cfg.OllamaURL
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	resp, err := http.Get(ollamaURL + "/api/tags")
	if err == nil && resp.StatusCode == 200 {
		resp.Body.Close()
		model := cfg.EmbeddingModel
		if model == "" {
			model = "nomic-embed-text"
		}
		embedder = brain.NewOllamaProvider(brain.OllamaConfig{
			BaseURL: ollamaURL,
			Model:   model,
		})
		log.Printf("Brain: using Ollama (%s) at %s", model, ollamaURL)
	} else if cfg.RemoteAPIBase != "" && cfg.RemoteAPIKey != "" {
		model := cfg.RemoteModel
		if model == "" {
			model = "text-embedding-3-small"
		}
		embedder = brain.NewRemoteAPIProvider(brain.RemoteAPIConfig{
			BaseURL: cfg.RemoteAPIBase,
			APIKey:  cfg.RemoteAPIKey,
			Model:   model,
		})
		log.Printf("Brain: using remote API (%s)", model)
	} else {
		log.Println("Brain: no embedding provider available, running in FTS5-only mode")
	}

	opts := brain.DefaultOptions()
	if cfg.EmbeddingModel != "" {
		opts.EmbeddingModel = cfg.EmbeddingModel
	}
	if cfg.EmbeddingDims > 0 {
		opts.EmbeddingDims = cfg.EmbeddingDims
	}

	brainInst, err := brain.Init(dbPath, embedder, opts)
	if err != nil {
		log.Printf("Brain: failed to initialize: %v", err)
		return nil
	}

	stats, _ := brainInst.Stats(context.Background())
	if stats.Pages == 0 {
		memDir := filepath.Join(workspace, "memory")
		if info, err := os.Stat(memDir); err == nil && info.IsDir() {
			imported, err := brainInst.ImportMemories(context.Background(), memDir)
			if err != nil {
				log.Printf("Brain: memory import failed: %v", err)
			} else {
				log.Printf("Brain: imported %d existing memory files", imported)
			}
		}
	}

	return brainInst
}
