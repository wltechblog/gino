<p align="center">
  <img src="docs/logo.png" alt="Picobot" width="250" height="150">
  <h1 align="center">Picobot</h1>
  <p align="center"><strong>Your AI agent. On your hardware. Under your control.</strong></p>
  <p align="center">
    <img src="https://img.shields.io/badge/binary-~12MB-brightgreen" alt="Binary Size">
    <img src="https://img.shields.io/badge/RAM-~10MB-orange" alt="Memory Usage">
    <img src="https://img.shields.io/badge/built_with-Go-00ADD8?logo=go" alt="Go">
    <img src="https://img.shields.io/badge/license-MIT-yellow" alt="License">
  </p>
</p>

Picobot is a self-hosted AI agent written in Go. One binary, zero dependencies, runs on a Raspberry Pi or a $5 VPS. It talks to any OpenAI-compatible LLM (OpenRouter, Ollama, OpenAI, etc.) and connects to Telegram, Discord, Slack, or WhatsApp.

This is the [WLTechBlog](https://youtube.com/@wltechblog) fork with a built-in knowledge brain, single-channel builds, and a focus on privacy-first operation.

---

## Why Picobot over OpenClaw?

Picobot takes direct inspiration from [OpenClaw](https://github.com/openclaw/openclaw) — same concepts (tools, skills, memory, heartbeats, cron) — but built for people who want to own their infrastructure instead of renting it.

| | Picobot | OpenClaw |
|---|---|---|
| **Runtime** | Single Go binary (~12MB) | Node.js (~200MB+) |
| **RAM** | ~10MB idle | ~200MB–1GB |
| **Cold start** | Instant | 5–30 seconds |
| **Raspberry Pi** | First-class citizen | Painful on ARM |
| **Language** | Go (static, cross-compiles) | JavaScript/TypeScript |
| **License** | MIT | MIT |
| **Memory system** | File-based + optional SQLite brain | File-based |
| **Semantic search** | Built-in (FTS5 + vector + RRF) | External tooling |
| **Knowledge graph** | Built-in (auto-extracted entities) | Not included |

If you're running a Pi, a small VPS, or just want an agent that starts instantly and sips RAM — Picobot is it.

---

## Quick Start

### From Source

```sh
git clone https://github.com/WLTBAgent/picobot.git
cd picobot
make build                    # full build with all channels (~22MB)
./picobot onboard             # creates ~/.picobot with config + workspace
```

Edit `~/.picobot/config.json` with your API key and channel tokens, then:

```sh
./picobot gateway             # starts the agent with all enabled channels
```

### Docker

```sh
docker run -d --name picobot \
  -e OPENAI_API_KEY="your-key" \
  -e OPENAI_API_BASE="https://openrouter.ai/api/v1" \
  -v ./picobot-data:/home/picobot/.picobot \
  louisho5/picobot:latest
```

### Single-Command Build Variants

```sh
make build              # all channels (~22MB)
make build-telegram     # Telegram only (~12MB)
make build-discord      # Discord only (~13MB)
make build-slack        # Slack only (~13MB)
make build-lite         # no WhatsApp (~14MB)
```

### Cross-Compile

```sh
# For a Raspberry Pi
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o picobot ./cmd/picobot

# For a Linux VPS
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o picobot ./cmd/picobot
```

Works on any Linux with 256MB RAM. Copy the binary and run.

---

## The Knowledge Brain

Picobot includes an optional SQLite-backed knowledge system ([picobot-brain](https://github.com/WLTBAgent/picobot-brain)) that gives your agent real memory — not just flat files.

**What it adds:**
- **Hybrid search** — FTS5 keyword + vector semantic similarity, merged via Reciprocal Rank Fusion
- **Knowledge graph** — auto-extracted entities and relationships from `[[wikilinks]]`, `@mentions`, and text patterns
- **Automatic context** — before every LLM call, the brain searches for relevant context and injects it into the system prompt
- **Content dedup** — SHA-256 hashing prevents importing the same content twice

**It's optional and backward-compatible.** If you don't enable it, Picobot works exactly as before with its file-based memory system.

### Enabling the Brain

Add to `~/.picobot/config.json`:

```json
{
  "brain": {
    "enabled": true,
    "embeddingModel": "nomic-embed-text"
  }
}
```

On first run, the brain auto-imports everything in `~/.picobot/workspace/memory/` — your existing daily notes and MEMORY.md become searchable instantly.

### Setting Up Embeddings

For semantic search you need an embedding model. The easiest path is [Ollama](https://ollama.com):

```sh
# Install Ollama (one line)
curl -fsSL https://ollama.com/install.sh | sh

# Pull the embedding model (~274MB, runs on Pi 5)
ollama pull nomic-embed-text
```

Or use Docker:

```sh
docker run -d --name ollama -p 11434:11434 ollama/ollama
docker exec ollama ollama pull nomic-embed-text
```

Picobot auto-detects Ollama at `localhost:11434`. No additional config needed.

See [picobot-brain docs](https://github.com/WLTBAgent/picobot-brain/blob/main/docs/OLLAMA_SETUP.md) for cloud API fallback, Pi Zero setup, and troubleshooting.

### Brain Tools

When enabled, the agent gets five new tools:

| Tool | What it does |
|------|-------------|
| `brain_search` | Hybrid search across all ingested content |
| `brain_ingest` | Import a file or directory into the brain |
| `brain_entity` | Look up an entity and its relationships |
| `brain_status` | Show brain statistics (pages, entities, embeddings) |
| `brain_maintain` | Backfill embeddings, extract entities, prune orphans |

### No Ollama? No Problem

The brain runs in **FTS5-only mode** without any embedding provider. You still get keyword search with BM25 ranking — better than the default memory system. Just enable the brain without configuring any embedding provider:

```json
{
  "brain": {
    "enabled": true,
    "embeddingDims": 0
  }
}
```

---

## Configuration

Picobot uses a single JSON config at `~/.picobot/config.json`:

```json
{
  "agents": {
    "defaults": {
      "model": "google/gemini-2.5-flash",
      "maxTokens": 8192,
      "temperature": 0.7,
      "maxToolIterations": 200,
      "heartbeatIntervalS": 60,
      "workspace": "",
      "allowedDirs": ["/home/user/projects", "/tmp"]
    }
  },
  "providers": {
    "openai": {
      "apiKey": "sk-or-v1-YOUR_KEY",
      "apiBase": "https://openrouter.ai/api/v1"
    }
  },
  "brain": {
    "enabled": true,
    "embeddingModel": "nomic-embed-text",
    "ollamaUrl": "http://localhost:11434"
  },
  "channels": {
    "telegram": {
      "enabled": true,
      "token": "YOUR_TELEGRAM_BOT_TOKEN",
      "allowFrom": ["YOUR_TELEGRAM_USER_ID"]
    },
    "discord": {
      "enabled": false,
      "token": "",
      "allowFrom": []
    },
    "slack": {
      "enabled": false,
      "appToken": "",
      "botToken": "",
      "allowUsers": [],
      "allowChannels": []
    },
    "whatsapp": {
      "enabled": false,
      "allowFrom": []
    }
  },
  "mcpServers": {}
}
```

### Key Config Fields

| Field | Description |
|-------|-------------|
| `agents.defaults.model` | LLM model identifier (provider-specific) |
| `agents.defaults.maxTokens` | Max response tokens |
| `agents.defaults.maxToolIterations` | Max tool call loops per message |
| `agents.defaults.heartbeatIntervalS` | Heartbeat check interval in seconds |
| `agents.defaults.allowedDirs` | Directories the exec tool can access |
| `providers.openai.apiKey` | API key for the LLM provider |
| `providers.openai.apiBase` | API base URL (OpenRouter, Ollama, etc.) |
| `brain.enabled` | Enable the knowledge brain |
| `brain.embeddingModel` | Ollama model name for embeddings |
| `brain.ollamaUrl` | Ollama server URL (default: `http://localhost:11434`) |
| `brain.remoteApiBase` | Fallback remote API URL for embeddings |
| `brain.remoteApiKey` | Fallback remote API key |
| `brain.remoteModel` | Fallback remote embedding model name |
| `mcpServers` | Map of MCP server configs (see [CONFIG.md](docs/CONFIG.md)) |

Supports any **OpenAI-compatible API**: OpenAI, OpenRouter, Ollama, Groq, Together, etc.

---

## Built-in Tools

| Tool | What it does |
|------|-------------|
| `filesystem` | Read, write, list files in workspace |
| `exec` | Run shell commands (sandboxed to allowedDirs) |
| `web` | Fetch web pages and APIs |
| `web_search` | Search the web via DuckDuckGo |
| `message` | Send messages to channels |
| `spawn` | Launch background subagents |
| `cron` | Schedule recurring tasks |
| `write_memory` | Write to daily or long-term memory |
| `read_memory` | Read a memory file |
| `edit_memory` | Find and replace in a memory file |
| `list_memory` | List all memory files |
| `delete_memory` | Delete a daily memory file |
| `create_skill` | Create a skill from markdown |
| `read_skill` | Read a skill's content |
| `list_skills` | List available skills |
| `delete_skill` | Remove a skill |

Plus 5 brain tools when the knowledge brain is enabled (see above).

**MCP Servers:** extend with any [Model Context Protocol](https://modelcontextprotocol.io) server — a binary, `npx`, `uvx`, `docker run`, or HTTP endpoint. Tools register automatically as `mcp_{server}_{tool}`. See [CONFIG.md](docs/CONFIG.md#mcpservers).

---

## Skills System

Teach your agent new tricks. Skills are markdown files in `~/.picobot/workspace/skills/`:

```
You: "Create a skill for checking weather using curl wttr.in"
Agent: Created skill "weather" — I'll use it from now on.
```

The agent creates them via the `create_skill` tool or you can write them manually. They're loaded into the system prompt automatically.

---

## CLI Reference

```
picobot version                        # print version
picobot onboard                        # create config + workspace
picobot --home /path onboard           # use custom home directory
picobot agent -m "..."                 # one-shot query
picobot agent -M model -m "..."        # query with specific model
picobot channels login                 # interactive channel setup
picobot gateway                        # start long-running agent
picobot memory read today|long         # read memory
picobot memory append today|long -c "" # append to memory
picobot memory write long -c ""        # overwrite long-term memory
picobot memory recent --days N         # recent N days
picobot memory rank -q "query"         # semantic memory search
```

Multiple instances with `--home`:

```sh
picobot --home /opt/bot1 onboard
picobot --home /opt/bot1 gateway &

picobot --home /opt/bot2 onboard
picobot --home /opt/bot2 gateway &
```

---

## Project Structure

```
cmd/picobot/          CLI entry point
internal/
  agent/              Agent loop, context builder, tools, skills
    memory/           File-based memory store + ranking
    tools/            All tool implementations (including brain)
  channels/           Telegram, Discord, Slack, WhatsApp
  chat/               Chat message hub
  config/             Config schema, loader, onboarding
  cron/               Cron scheduler
  heartbeat/          Periodic task checker
  mcp/                MCP client (stdio + HTTP)
  providers/          OpenAI-compatible LLM provider
  session/            Session manager
docker/               Dockerfile, compose, entrypoint
```

---

## Running on a Raspberry Pi

Picobot is designed for constrained hardware. Build for ARM:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -tags only_telegram -o picobot ./cmd/picobot
```

The `only_telegram` build tag strips Discord, Slack, and WhatsApp — drops the binary from ~22MB to ~12MB.

For the knowledge brain, run Ollama locally:

```sh
curl -fsSL https://ollama.com/install.sh | sh
ollama pull nomic-embed-text
```

nomic-embed-text uses ~300MB RAM on a Pi 5. If that's too much, enable the brain without embeddings (FTS5-only) — it still works great.

---

## Docs

- [HOW_TO_START.md](docs/HOW_TO_START.md) — step-by-step getting started guide
- [CONFIG.md](docs/CONFIG.md) — full configuration reference
- [DEVELOPMENT.md](docs/DEVELOPMENT.md) — development, testing, and Docker publishing
- [docker/README.md](docker/README.md) — Docker deployment guide

---

## License

MIT — use it however you want.
