<p align="center">
  <img src="docs/logo.png" alt="Gino" width="250">
  <h1 align="center">Gino</h1>
  <p align="center"><strong>Your AI agent. On your hardware. Under your control.</strong></p>
  <p align="center">
    <img src="https://img.shields.io/badge/binary-~12MB-brightgreen" alt="Binary Size">
    <img src="https://img.shields.io/badge/RAM-~10MB-orange" alt="Memory Usage">
    <img src="https://img.shields.io/badge/built_with-Go-00ADD8?logo=go" alt="Go">
    <img src="https://img.shields.io/badge/license-MIT-yellow" alt="License">
  </p>
</p>

Gino is a self-hosted AI agent written in Go. One binary, minimal dependencies, runs on a Raspberry Pi or a $5 VPS. It connects to any OpenAI-compatible LLM (OpenRouter, OpenAI, z.ai, Ollama, etc.) and works with Telegram and Discord.

The built-in knowledge brain uses **Ollama** for local embeddings — automatically bundled in the Docker image, so it works out of the box with zero configuration. Prefer to use your own infrastructure? Point Gino at any external Ollama instance, or disable the brain entirely. Your choice, your hardware.

---

## Why Gino?

**Tiny footprint.** The entire agent is a single ~12MB binary. Idle RAM usage is around 10MB. Cold start is instant — no runtime to spin up, no garbage to collect.

**Advanced memory system.** Gino doesn't just remember things — it understands them. A built-in SQLite knowledge brain provides hybrid search (FTS5 keyword + vector semantic similarity via Reciprocal Rank Fusion), an auto-extracted knowledge graph, and automatic context injection before every LLM call.

**Signal system.** Gino's unique Unix-domain-socket signal system lets external scripts, cron jobs, MCP servers, and IoT devices trigger pre-registered actions. Your camera detects motion? Send a signal. A build finishes? Send a signal. The agent wakes, processes, and responds on the right channel — without exposing freeform prompt injection.

**Supply chain security.** Gino minimizes its dependency surface and vendors everything. No npm-style transitive dependency trees. The full source you're running sits in `vendor/` — auditable, reproducible, and immune to upstream package tampering.

**Fast Docker/Podman deployment.** A single container includes both Gino and Ollama. Copy `.env.example`, set your API key and bot token, `docker compose up -d`. That's it.

**Built for real hardware.** Cross-compiles to any platform Go supports. First-class ARM64 support for Raspberry Pi. No Node.js, no Python, no 500MB container layers.

---

## Quick Start

### Docker (recommended)

```sh
git clone https://github.com/wltechblog/gino.git
cd gino/docker
cp .env.example .env
# Edit .env — at minimum set OPENAI_API_KEY and a channel token
docker compose up -d
```

The container bundles Ollama for embeddings when the brain is enabled. See [`docker/README.md`](docker/README.md) for all options.

### From Source

```sh
git clone https://github.com/wltechblog/gino.git
cd gino
make build
./gino onboard          # creates ~/.gino with config + workspace
```

Edit `~/.gino/config.json` with your API key and channel tokens, then:

```sh
./gino gateway
```

### Cross-Compile

```sh
# Raspberry Pi (ARM64)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o gino ./cmd/gino

# Linux VPS (AMD64)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o gino ./cmd/gino
```

Works on any Linux with 256MB RAM. Copy the binary and run.

---

## The Knowledge Brain

Gino includes a SQLite-backed knowledge system that gives your agent real memory.

**Hybrid search** — FTS5 full-text search and vector semantic similarity are merged via Reciprocal Rank Fusion (RRF). You get keyword exactness and semantic understanding in every query.

**Knowledge graph** — Entities and relationships are auto-extracted from `[[wikilinks]]`, `@mentions`, and natural language patterns. The agent can answer "who works at X?" or "what is Y connected to?".

**Automatic context** — Before every LLM call, the brain searches for relevant context and injects it into the system prompt. The agent has the right information at the right time, automatically.

**Content dedup** — SHA-256 hashing prevents importing the same content twice.

### Enabling the Brain

Add to `~/.gino/config.json`:

```json
{
  "brain": {
    "enabled": true,
    "embeddingModel": "nomic-embed-text"
  }
}
```

Or in Docker, set `GINO_BRAIN_ENABLED=true` in your `.env`.

The brain needs an embedding model. With bundled Ollama, it works out of the box. For native Ollama:

```sh
curl -fsSL https://ollama.com/install.sh | sh
ollama pull nomic-embed-text
```

### Brain Tools

When enabled, the agent gets these tools:

| Tool | What it does |
|------|-------------|
| `brain_search` | Hybrid search across all ingested content |
| `brain_ingest` | Import files or directories into the knowledge base |
| `brain_entity` | Look up entities and their relationships in the knowledge graph |
| `brain_status` | Show brain statistics (pages, entities, embeddings) |
| `brain_maintain` | Backfill missing embeddings, prune orphaned data |

---

## Signal System

Gino can be triggered by external systems via a Unix domain socket. Signals are **action-based** — they carry a registered action name, not freeform instructions. This means external scripts can wake the agent safely without prompt injection risk.

### How it works

1. Register actions in `config.json` (or let MCP servers self-declare them)
2. External scripts send a JSON signal to the socket
3. The agent wakes, injects a safe pre-defined response, and processes it

```json
{
  "signals": {
    "actions": {
      "check_messages": {
        "response": "Check your messages and summarize anything important.",
        "silent": false
      },
      "motion_detected": {
        "response": "Motion was detected by the security camera. Check the feed.",
        "channel": "telegram",
        "chatId": "123456789"
      }
    }
  }
}
```

```sh
# Send a signal from any script
echo '{"action":"motion_detected"}' | socat - UNIX-CONNECT:~/.gino/signal.sock
```

MCP servers can self-declare actions at startup via the protocol handshake — no manual registration needed.

---

## MCP Support

Gino supports [Model Context Protocol](https://modelcontextprotocol.io) servers. Add them to your config:

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/path"]
    }
  }
}
```

MCP tools are automatically discovered and made available to the agent. MCP servers can also self-declare signal actions for the signal system.

---

## Tools

Gino includes a built-in tool set:

| Tool | Description |
|------|-------------|
| `filesystem` | Read, write, edit, and list files in the workspace |
| `exec` | Execute shell commands |
| `web` | Fetch web content (with timeout, size limit, and content-type filtering) |
| `message` | Send messages to the current channel |
| `write_memory` / `read_memory` | Persist and recall information |
| `cron` | Schedule one-time or recurring tasks |
| `spawn` | Launch background subagents |

---

## Skills

Skills are reusable knowledge modules stored in `~/.gino/workspace/skills/`. Each skill is a markdown file with instructions, examples, and procedures. The agent loads them automatically and uses them when relevant.

Create a skill:

```sh
# Gino can create skills for you — just ask!
# Or create manually:
mkdir -p ~/.gino/workspace/skills/deploy
cat > ~/.gino/workspace/skills/deploy/SKILL.md << 'EOF'
# Deploy Skill

## Deploy to Production
1. Run tests: `make test`
2. Build: `make build`
3. Deploy: `./deploy.sh production`
EOF
```

---

## Memory

Gino has a layered memory system:

- **Daily notes** (`memory/YYYY-MM-DD.md`) — ephemeral context for today
- **Long-term memory** (`memory/MEMORY.md`) — durable facts and preferences
- **Knowledge brain** (`brain.db`) — searchable, indexed, entity-aware

The agent automatically extracts important facts from conversations and saves them to memory. It checks existing memory before writing to avoid duplicates, and can edit or correct specific facts.

---

## Channels

| Channel | Type | Status | Notes |
|---------|------|--------|-------|
| **Telegram** | Bot API | ✅ Stable | MarkdownV2 formatting with automatic reserved-character escaping |
| **Discord** | Bot (discordgo) | ✅ Stable | |
| **CLI** | stdin/stdout | ✅ Built-in | |

---

## Configuration

All config lives in `~/.gino/config.json`:

```json
{
  "providers": {
    "openai": {
      "apiKey": "your-key",
      "baseURL": "https://openrouter.ai/api/v1"
    }
  },
  "agents": {
    "defaults": {
      "model": "google/gemini-2.5-flash",
      "maxTokens": 8192,
      "maxToolIterations": 100
    }
  },
  "channels": {
    "telegram": {
      "token": "your-bot-token",
      "allowFrom": ["your-user-id"]
    },
    "discord": {
      "token": "your-bot-token",
      "allowFrom": ["your-user-id"]
    }
  },
  "brain": {
    "enabled": true,
    "embeddingModel": "nomic-embed-text"
  }
}
```

Run `./gino onboard` to generate a starter config interactively.

---

## Environment Variables

All config values can be overridden via environment variables (useful for Docker):

| Variable | Description |
|----------|-------------|
| `GINO_MODEL` | LLM model identifier |
| `GINO_MAX_TOKENS` | Max response tokens |
| `GINO_MAX_TOOL_ITERATIONS` | Max tool call loops per message |
| `GINO_BRAIN_ENABLED` | Enable the knowledge brain |
| `GINO_BRAIN_EMBEDDING_MODEL` | Ollama embedding model name |
| `GINO_HOME` | Home directory path |
| `GINO_SIGNAL_SOCKET` | Signal socket path |
| `GINO_WEB_TIMEOUT_S` | Web tool timeout (default: 30) |
| `GINO_WEB_MAX_RESPONSE_BYTES` | Web tool response size limit (default: 1MB) |
| `GINO_WEB_USER_AGENT` | Web tool User-Agent string |
| `OPENAI_API_KEY` | LLM provider API key |
| `OPENAI_API_BASE` | LLM provider base URL |
| `TELEGRAM_BOT_TOKEN` | Telegram bot token |
| `TELEGRAM_ALLOW_FROM` | Comma-separated Telegram user IDs |
| `DISCORD_BOT_TOKEN` | Discord bot token |
| `DISCORD_ALLOW_FROM` | Comma-separated Discord user IDs |

---

## License

MIT
