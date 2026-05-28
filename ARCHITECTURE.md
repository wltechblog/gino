# Picobot Architecture — Deep Dive

## Project Overview
- **Language**: Go 1.26.3
- **Module**: `github.com/local/picobot`
- **Version**: 0.2.1
- **Binary size**: ~12-22MB (single binary, static compile)
- **RAM**: ~10MB idle
- **License**: MIT

## Directory Structure
```
cmd/picobot/main.go         — CLI entry point (Cobra)
internal/
  agent/
    loop.go                 — Core agent loop (AgentLoop)
    context.go              — ContextBuilder (system prompt assembly)
    tools/                  — All tool implementations
      registry.go           — Tool registry (Thread-safe map)
      exec.go               — Shell execution (array-only, sandboxed)
      filesystem.go         — File ops using os.Root (Go 1.24+ kernel sandbox)
      web.go                — HTTP fetch
      web_search.go         — DuckDuckGo Instant Answer API
      message.go            — Send messages to channels via Hub
      spawn.go              — Background subagent (stub)
      cron.go               — Schedule reminders via cron.Scheduler
      memory.go             — read/list/edit/delete memory tools
      write_memory.go       — write_memory + content filtering
      skill.go              — CRUD for skills (SkillManager via os.Root)
      brain.go              — 5 brain tools (search, ingest, entity, status, maintain)
      mcp.go                — MCP tool wrapper
      mcp_manage.go         — mcp_restart + mcp_list tools
    memory/
      store.go              — File-based memory (MEMORY.md + daily YYYY-MM-DD.md)
      ranker.go             — SimpleRanker (keyword overlap)
      llm_ranker.go         — LLM-based ranking with SimpleRanker fallback
    skills/
      loader.go             — Loads SKILL.md files with YAML frontmatter
  channels/
    telegram.go             — Telegram via gotgbot
    discord.go              — Discord via discordgo
    slack.go                — Slack via slack-go/socket mode
    whatsapp.go             — WhatsApp via whatsmeow
    helpers.go              — Shared channel helpers
    stub_*.go               — Build-tag stubs for single-channel builds
  chat/
    chat.go                 — Hub (Inbound/Outbound channels + subscriber router)
  config/
    schema.go               — Config structs (JSON)
    loader.go               — LoadConfig + env overrides
    onboard.go              — Default config, workspace init, bootstrap files
  cron/
    scheduler.go            — In-memory cron (1s ticker, fire callback)
  heartbeat/
    service.go              — Periodic checker → Hub
  mcp/
    client.go               — MCP client (stdio + HTTP transports, JSON-RPC 2.0)
  providers/
    provider.go             — LLMProvider interface, Message/ToolDefinition/ToolCall types
    openai.go               — OpenAI-compatible provider (chat completions)
    factory.go              — NewProviderFromConfig
    stub.go                 — Stub provider fallback
  session/
    manager.go              — Session persistence (JSON files, max 50 messages)
embeds/
  embeds.go                 — Embedded skills FS
```

## Core Flow

### 1. Gateway Startup (cmd/picobot/main.go gatewayCmd)
1. Load config from ~/.picobot/config.json + env overrides
2. Create chat.Hub (buffered inbound/outbound channels)
3. Create cron.Scheduler with fire callback → Hub.In
4. Create AgentLoop (registers all tools, MCP clients, brain)
5. go ag.Run(ctx) — agent loop goroutine
6. go scheduler.Start(ctx.Done()) — cron ticker
7. Start periodic checker → Hub
8. Start enabled channels (Telegram, Discord, Slack, WhatsApp)
9. hub.StartRouter(ctx) — dispatch outbound to channel subscribers
10. Wait for SIGINT/SIGTERM

### 2. Agent Loop (internal/agent/loop.go)
- Reads from hub.In channel
- "remember X" shortcut → writes to today's memory, skips LLM
- System channels (periodic checker, cron) → stateless, no session persistence
- Interactive channels → load session history, build context
- Tool call loop: max maxToolIterations (default 100) iterations
  - Call provider.Chat() with messages + tool definitions
  - If tool calls → execute each, append tool results, loop
  - If no tool calls → final response, send to hub.Out
- Session saved after each interaction (max 50 messages trimmed)

### 3. Context Building (internal/agent/context.go)
System prompt assembled from:
1. "You are Picobot" base
2. Bootstrap files: SOUL.md, AGENTS.md, USER.md, TOOLS.md
3. Channel/workspace context
4. Memory tool instruction
5. Loaded skills (skills/*/SKILL.md)
6. File-based memory (long-term MEMORY.md + today's notes)
7. Top-K ranked memories (LLM ranker or keyword fallback)
8. Brain context (semantic search results)

Then replay session history + current user message.

### 4. Tool System (internal/agent/tools/)
- Tool interface: Name(), Description(), Parameters(), Execute()
- Registry: thread-safe map, 64KB max result (truncates + dumps to /tmp)
- Tools can be disabled via disableTools config
- MCP tools registered as mcp_{server}_{tool}

### 5. Provider (internal/providers/)
- LLMProvider interface: Chat(ctx, messages, tools, model) → LLMResponse
- Only OpenAI-compatible provider (works with OpenRouter, Ollama, etc.)
- Supports tool calls (function calling format)

### 6. Memory System (internal/agent/memory/)
- File-based: workspace/memory/MEMORY.md (long-term) + YYYY-MM-DD.md (daily)
- In-memory store with short/long term items + keyword search
- LLM ranker: sends memories to LLM for relevance ranking with tool-call support

### 7. Brain (picobot-brain external dependency)
- Optional SQLite-backed knowledge system
- Hybrid search: FTS5 + vector + Reciprocal Rank Fusion
- Knowledge graph with auto-extracted entities
- Auto-imports existing memories on first run
- Embedding providers: Ollama (local) or remote API

### 8. MCP (internal/mcp/)
- JSON-RPC 2.0 client
- Stdio transport (spawn process, stdin/stdout)
- HTTP transport (Streamable HTTP with SSE)
- Session management (Mcp-Session-Id header)

### 9. Channels
- All use chat.Hub for message routing
- Build-tag stubs for single-channel compilation
- AllowFrom/AllowUsers filtering
- WhatsApp uses whatsmeow with persistent DB

### 10. Cron (internal/cron/)
- In-memory, 1-second ticker
- One-time and recurring jobs
- Fire callback → routes message back through agent loop
- Jobs not persisted to disk (lost on restart)

### 11. Session (internal/session/)
- JSON file per conversation (workspace/sessions/{channel}:{chatID}.json)
- Max 50 messages (trimmed on save)
- Loaded on each message for history replay

## Security Features
- exec tool: array-only commands, blocks dangerous programs (rm, sudo, dd, etc.)
- filesystem tool: uses os.Root (Go 1.24+) for kernel-level path containment
- write_memory: filters unwanted content to prevent pollution
- allowedDirs config for exec/filesystem sandboxing
- disableTools config to remove specific tools
- Tool results truncated at 64KB

## Build Tags
- only_telegram — strips Discord, Slack, WhatsApp
- only_discord — strips others
- only_slack — strips others
- lite — no WhatsApp

## Key Dependencies
- github.com/WLTBAgent/picobot-brain — knowledge brain (SQLite + embeddings)
- github.com/bwmarrin/discordgo — Discord
- github.com/slack-go/slack — Slack
- go.mau.fi/whatsmeow — WhatsApp
- github.com/spf13/cobra — CLI
- modernc.org/sqlite — pure Go SQLite
