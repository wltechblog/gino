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
	"time"

	brain "github.com/WLTBAgent/picobot-brain"
	"github.com/local/picobot/internal/agent/memory"
	"github.com/local/picobot/internal/agent/tools"
	"github.com/local/picobot/internal/chat"
	"github.com/local/picobot/internal/config"
	"github.com/local/picobot/internal/cron"
	"github.com/local/picobot/internal/mcp"
	"github.com/local/picobot/internal/providers"
	"github.com/local/picobot/internal/session"
)

var rememberRE = regexp.MustCompile(`(?i)^remember(?:\s+to)?\s+(.+)$`)

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
	hub                *chat.Hub
	provider           providers.LLMProvider
	tools              *tools.Registry
	sessions           *session.SessionManager
	checkpoints        *CheckpointManager
	context            *ContextBuilder
	memory             *memory.MemoryStore
	brain              *brain.Brain
	model              string
	maxIterations      int
	running            bool
	mcpClients             []*mcp.Client
	mcpConfigs             map[string]config.MCPServerConfig
	enableToolActivity     bool
	enableToolCallMessages bool
	signalSocketPath       string // PICOBOT_SIGNAL_SOCKET injected into MCP child processes
}

// NewAgentLoop creates a new AgentLoop with the given provider.
func NewAgentLoop(b *chat.Hub, provider providers.LLMProvider, model string, maxIterations int, workspace string, scheduler *cron.Scheduler, mcpServers map[string]config.MCPServerConfig, allowedDirs []string, disableTools []string, brainCfg *config.BrainConfig, homeDir string, sandbox config.SandboxConfig, signalSocketPath string) *AgentLoop {
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
	register(tools.NewWebTool())
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
	for name, cfg := range mcpServers {
		var client *mcp.Client
		var err error
		switch {
		case cfg.Command != "":
			mcpEnv := map[string]string{}
			if signalSocketPath != "" {
				mcpEnv["PICOBOT_SIGNAL_SOCKET"] = signalSocketPath
				mcpEnv["PICOBOT_MCP_ID"] = name
			}
			client, err = mcp.NewStdioClientWithEnv(name, cfg.Command, cfg.Args, mcpEnv)
		case cfg.URL != "":
			client, err = mcp.NewHTTPClient(name, cfg.URL, cfg.Headers)
		default:
			log.Printf("MCP server %q: no command or url configured, skipping", name)
			continue
		}
		if err != nil {
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

	al := &AgentLoop{hub: b, provider: provider, tools: reg, sessions: sm, checkpoints: checkpoints, context: ctx, memory: mem, brain: brainInst, model: model, maxIterations: maxIterations, mcpClients: mcpClients, mcpConfigs: mcpServers, enableToolActivity: true, enableToolCallMessages: false}

	// Wire the MCP management tool callbacks so they can call back into the loop
	restartTool.SetCallback(al.restartMCPServer)
	listMCPTool.SetCallback(al.listMCPServers)

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
// path is injected as PICOBOT_SIGNAL_SOCKET into MCP child process environments.
func (a *AgentLoop) SetSignalSocketPath(path string) {
	a.signalSocketPath = path
}

// Close shuts down all MCP server connections and the brain.
func (a *AgentLoop) Close() {
	for _, c := range a.mcpClients {
		_ = c.Close()
	}
	if a.brain != nil {
		a.brain.Close()
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
		if a.signalSocketPath != "" {
			mcpEnv["PICOBOT_SIGNAL_SOCKET"] = a.signalSocketPath
			mcpEnv["PICOBOT_MCP_ID"] = serverName
		}
		newClient, err = mcp.NewStdioClientWithEnv(serverName, cfg.Command, cfg.Args, mcpEnv)
	case cfg.URL != "":
		newClient, err = mcp.NewHTTPClient(serverName, cfg.URL, cfg.Headers)
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

// Run starts processing inbound messages. This is a blocking call until context is canceled.
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

			log.Printf("Processing message from %s:%s\n", msg.Channel, msg.SenderID)

			// Quick heuristic: if user asks the agent to remember something explicitly,
			// store it in today's note and reply immediately without calling the LLM.
			trimmed := strings.TrimSpace(msg.Content)
			rememberRe := rememberRE
			if matches := rememberRe.FindStringSubmatch(trimmed); len(matches) == 2 {
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
					sess := a.sessions.GetOrCreate(msg.Channel + ":" + msg.ChatID)
					sess.AddMessage("user", msg.Content)
					sess.AddMessage("assistant", "OK, I've remembered that.")
					if err := a.sessions.Save(sess); err != nil {
						log.Printf("error saving session: %v", err)
					}
				}
				continue
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

			// Build messages from session, long-term memory, and recent memory.
			// System channels (heartbeat, cron) get a blank ephemeral session so
			// their history never accumulates and bloats the context window.
			var sess *session.Session
			if isSystemChannel(msg.Channel) {
				sess = &session.Session{Key: msg.Channel + ":" + msg.ChatID}
			} else {
				sess = a.sessions.GetOrCreate(msg.Channel + ":" + msg.ChatID)
			}
			// get file-backed memory context (long-term + today)
			memCtx, _ := a.memory.GetMemoryContext()
			memories := a.memory.Recent(5)
			userContent := msg.Content
			if len(msg.Media) > 0 {
				userContent += "\n\n[Attached files saved to:]"
				for _, p := range msg.Media {
					userContent += "\n- " + p
				}
			}
			messages := a.context.BuildMessages(sess.GetHistory(), userContent, msg.Channel, msg.ChatID, memCtx, memories)

			sessionKey := msg.Channel + ":" + msg.ChatID

			iteration := 0
			finalContent := ""
			lastToolResult := ""
			toolDefs := a.tools.Definitions()
			for iteration < a.maxIterations {
				iteration++

				// Checkpoint the current turn state before each LLM invocation.
				// This ensures we can recover if the process is restarted mid-turn.
				a.checkpoints.Save(sessionKey, &ActiveTurn{
					Channel:        msg.Channel,
					ChatID:         msg.ChatID,
					SenderID:       msg.SenderID,
					Content:        msg.Content,
					Messages:       messages,
					Iteration:      iteration,
					LastToolResult: lastToolResult,
				})

				resp, err := a.provider.Chat(ctx, messages, toolDefs, a.model)
				if err != nil {
					log.Printf("provider error: %v", err)
					finalContent = "Sorry, I encountered an error while processing your request."
					break
				}

				if resp.HasToolCalls {
					// append assistant message with tool_calls attached
					messages = append(messages, providers.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
					// execute each tool call and return results with "tool" role
					for _, tc := range resp.ToolCalls {
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
						messages = append(messages, providers.Message{Role: "tool", Content: res, ToolCallID: tc.ID})
					}
					// loop again
					continue
				} else {
					finalContent = resp.Content
					break
				}
			}

			// Turn completed — clear the checkpoint so it won't be recovered.
			a.checkpoints.MarkCompleted(sessionKey)

			if finalContent == "" && lastToolResult != "" {
				finalContent = lastToolResult
			} else if finalContent == "" {
				finalContent = "I've completed processing but have no response to give."
			}

			// Save session for interactive channels only.
			// System channels (heartbeat, cron) are stateless triggers — their
			// history must not be persisted, otherwise the file grows unboundedly.
			if !isSystemChannel(msg.Channel) {
				sess.AddMessage("user", msg.Content)
				sess.AddMessage("assistant", finalContent)
				if err := a.sessions.Save(sess); err != nil {
					log.Printf("error saving session: %v", err)
				}
			}

			out := chat.Outbound{Channel: msg.Channel, ChatID: msg.ChatID, Content: finalContent}
			select {
			case a.hub.Out <- out:
			default:
				log.Println("Outbound channel full, dropping message")
			}
		default:
			// idle tick
			time.Sleep(100 * time.Millisecond)
		}
	}
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
	messages := a.context.BuildMessages(nil, content, "cli", "direct", memCtx, memories)

	// Support tool calling iterations (similar to main loop)
	var lastToolResult string
	for iteration := 0; iteration < a.maxIterations; iteration++ {
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
			messages = append(messages, providers.Message{Role: "tool", Content: result, ToolCallID: tc.ID})
		}
	}

	return "Max iterations reached without final response", nil
}

// initBrain initializes the knowledge brain subsystem.
// Tries Ollama first, falls back to remote API, then FTS5-only mode.
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
