# How to Start Using Gino

## Prerequisites

- **Go 1.26+** installed ([download](https://go.dev/dl/))
- An **API key** for an OpenAI-compatible service:
  - [OpenRouter](https://openrouter.ai/keys) (recommended, supports many models)
  - [OpenAI](https://platform.openai.com/api-keys)
  - [z.ai](https://z.ai) Coding Plan
  - Or use a local [Ollama](https://ollama.ai) instance (no key needed)

## Step 1: Build

Gino is a single static binary with no runtime dependencies.

### Build from source

```sh
git clone https://github.com/wltechblog/gino.git
cd gino
go build -o gino ./cmd/gino
```

### Build all platforms (Makefile)

Use `make` to cross-compile all supported platforms:

```sh
make build-all
```

This produces binaries for Linux amd64/arm64 and macOS arm64.

Individual targets:

```sh
make linux_amd64        # Linux x86-64
make linux_arm64        # Linux ARM64
make mac_arm64          # macOS Apple Silicon
make linux_amd64_telegram   # Telegram-only (smaller binary)
make linux_arm64_telegram   # Telegram-only, ARM64
make clean              # remove all built binaries
```

## Step 2: Onboard

Run the onboard command to create the config and workspace:

```sh
./gino onboard
```

This creates:
- `~/.gino/config.json` — your configuration file
- `~/.gino/workspace/` — the agent's workspace with bootstrap files:
  - `SOUL.md` — agent personality and values
  - `AGENTS.md` — agent instructions and guidelines
  - `USER.md` — your profile (customize this!)
  - `TOOLS.md` — documentation of all available tools
  - `HEARTBEAT.md` — periodic tasks
  - `memory/MEMORY.md` — long-term memory
  - `skills/example/SKILL.md` — example skill

## Step 3: Configure API Key

Edit `~/.gino/config.json` and replace the placeholder API key:

```sh
nano ~/.gino/config.json
```

Change `"sk-or-v1-REPLACE_ME"` to your actual API key.

Also set your preferred model:

```json
{
  "agents": {
    "defaults": {
      "model": "google/gemini-2.5-flash"
    }
  },
  "providers": {
    "openai": {
      "apiKey": "sk-or-v1-YOUR_ACTUAL_KEY",
      "apiBase": "https://openrouter.ai/api/v1"
    }
  }
}
```

## Step 4: Customize Your Profile

Edit `~/.gino/workspace/USER.md` to fill in your name, timezone, preferences, etc. This helps the agent personalize its responses.

## Step 5: Try It!

### Single-shot query

```sh
./gino agent -m "Hello, what tools do you have?"
```

### Use a specific model

```sh
./gino agent -M "google/gemini-2.5-flash" -m "What is 2+2?"
```

### Work on a project while keeping your Gino profile

`--project` sets the active working directory (files, exec cwd, project `AGENTS.md`) while identity, memory, skills, sessions, and checkpoints stay in the configured Gino workspace. Omitting the flag keeps the previous behaviour: profile and project are the same directory.

```sh
./gino chat --project /path/to/repo
./gino agent --project /path/to/repo -m "What does this repo do?"
```

### Login to channels (Telegram, Discord)

```sh
./gino channels login
```

### Start the gateway (long-running mode)

```sh
./gino gateway
```

This starts the agent loop, heartbeat, and any enabled channels (Telegram, Discord).

## CLI Commands

| Command | Description |
|---------|-------------|
| `gino version` | Print version |
| `gino onboard` | Create default config and workspace |
| `gino channels login` | Interactively connect Telegram or Discord |
| `gino agent -m "..."` | Run a single-shot agent query |
| `gino agent -M model -m "..."` | Query with a specific model |
| `gino agent --project dir -m "..."` | Query against a project while keeping the Gino profile |
| `gino chat` | Interactive TUI chat |
| `gino chat --project dir` | Interactive chat in a project with a persistent Gino profile |
| `gino gateway` | Start long-running gateway |
| `gino memory read today` | Read today's memory notes |
| `gino memory read long` | Read long-term memory |
| `gino memory append today -c "..."` | Append to today's notes |
| `gino memory append long -c "..."` | Append to long-term memory |
| `gino memory write long -c "..."` | Overwrite long-term memory |
| `gino memory recent -days 7` | Show recent 7 days' notes |
| `gino memory rank -q "query"` | Rank memories by relevance |

## Available Tools

The agent has access to these built-in tools:

| Tool | Purpose |
|------|--------|
| `message` | Send messages to channels |
| `filesystem` | Read, write, list, edit files |
| `exec` | Run shell commands |
| `web` | Fetch web content from URLs |
| `web_search` | Search the web via DuckDuckGo (no API key needed) |
| `spawn` | Spawn background subagent |
| `cron` | Schedule cron jobs |
| `write_memory` | Persist information to memory |
| `list_memory` | List all memory files |
| `read_memory` | Read a specific memory file |
| `edit_memory` | Find and replace text in a memory file |
| `delete_memory` | Delete a daily memory file |
| `create_skill` | Create a new skill |
| `list_skills` | List available skills |
| `read_skill` | Read a skill's content |
| `delete_skill` | Delete a skill |

### MCP Server Tools

Additional tools are registered dynamically from any MCP servers listed in `mcpServers` in your `config.json`. Each tool is exposed as `mcp_{server}_{tool}`.

See [CONFIG.md](CONFIG.md#mcpservers) for the full mcpServers configuration reference.

## Setting Up Telegram (BotFather Guide)

To chat with Gino on Telegram, you need to create a bot via **@BotFather**.

### Quick setup (recommended)

Run the interactive channel login wizard:

```sh
./gino channels login
```

Select **1) Telegram**, then follow the prompts.

### Manual setup

### 1. Open BotFather

Open Telegram and search for [@BotFather](https://t.me/BotFather), or click the link directly.

### 2. Create a New Bot

Send the command:

```
/newbot
```

BotFather will ask for:
1. **Bot name** — A display name (e.g., `My Gino`)
2. **Bot username** — A unique username ending in `bot` (e.g., `my_gino_bot`)

### 3. Copy the Token

After creation, BotFather will reply with your bot token:

```
123456789:ABCdefGHIjklMNOpqrsTUVwxyz
```

### 4. Get Your Telegram User ID

Send a message to [@userinfobot](https://t.me/userinfobot) on Telegram — it will reply with your numeric user ID.

### 5. Configure Gino

Edit `~/.gino/config.json`:

```json
{
  "channels": {
    "telegram": {
      "enabled": true,
      "token": "123456789:ABCdefGHIjklMNOpqrsTUVwxyz",
      "allowFrom": ["8881234567"]
    }
  }
}
```

## Setting Up Discord

### 1. Create a Bot Application

Go to the [Discord Developer Portal](https://discord.com/developers/applications) and create a new application.

### 2. Get the Bot Token

Under **Bot** → **Token**, copy the token.

### 3. Get Your User ID

Enable Developer Mode in Discord settings (App Settings → Advanced → Developer Mode), then right-click your username and select **Copy User ID**.

### 4. Configure Gino

Edit `~/.gino/config.json`:

```json
{
  "channels": {
    "discord": {
      "enabled": true,
      "token": "MTIzNDU2789abcDEF",
      "allowFrom": ["123456789012345678"]
    }
  }
}
```

## Docker Quick Start

The fastest way to get running:

```sh
cd docker
cp .env.example .env
# Edit .env — set your API key and Telegram/Discord tokens
docker compose up -d
```

See [docker/README.md](../docker/README.md) for full Docker deployment instructions.

## Full Configuration Reference

See [CONFIG.md](CONFIG.md) for documentation of all config options including brain, signal, sandbox, web, compaction, and MCP servers.
