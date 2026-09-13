# Configuration Reference

Gino is configured via `~/.gino/config.json`. Run `gino onboard` to generate the default config.

## Full Default Config

```json
{
  "agents": {
    "defaults": {
      "workspace": "~/.gino/workspace",
      "model": "stub-model",
      "maxTokens": 8192,
      "temperature": 0.7,
      "maxToolIterations": 100,
      "heartbeatIntervalS": 60,
      "requestTimeoutS": 60,
      "enableToolActivityIndicator": true,
      "allowedDirs": [],
      "disableTools": [],
      "sandbox": {
        "mode": "strict"
      },
      "web": {
        "timeoutS": 30,
        "maxResponseBytes": 1048576,
        "userAgent": "GinoAI https://github.com/wltechblog/gino"
      }
    }
  },
  "mcpServers": {},
  "channels": {
    "telegram": {
      "enabled": false,
      "token": "",
      "allowFrom": []
    },
    "discord": {
      "enabled": false,
      "token": "",
      "allowFrom": []
    }
  },
  "providers": {
    "openai": {
      "apiKey": "sk-or-v1-REPLACE_ME",
      "apiBase": "https://openrouter.ai/api/v1"
    }
  },
  "brain": {
    "enabled": false
  },
  "signal": {
    "enabled": false
  }
}
```

---

## agents.defaults

Agent behavior settings.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `workspace` | string | `~/.gino/workspace` | Path to the agent's workspace directory. Contains bootstrap files, memory, and skills. |
| `model` | string | `stub-model` | Default LLM model to use. Set to a real model like `google/gemini-2.5-flash`. Can be overridden with the `-M` flag. |
| `reasoningEffort` | string | *(none)* | Reasoning effort sent as `reasoning_effort`. Validated against the provider's `reasoningLevels` (provider config takes precedence). |
| `maxTokens` | int | `8192` | Maximum tokens for LLM responses. |
| `temperature` | float | `0.7` | LLM temperature (0.0 = deterministic, 1.0 = creative). |
| `maxToolIterations` | int | `100` | Maximum number of tool-calling iterations per request. Prevents infinite loops. |
| `heartbeatIntervalS` | int | `60` | How often (in seconds) the heartbeat checks `HEARTBEAT.md` for periodic tasks. Only used in gateway mode. |
| `requestTimeoutS` | int | `60` | HTTP timeout in seconds for each LLM API request. Increase for slow models or poor network conditions. |
| `enableToolActivityIndicator` | bool | `true` | When `true`, sends interim `🤖 Running` / `📢 done` messages to the chat channel as tools are called. Set to `false` for IoT or headless deployments where only the final response should be delivered. |
| `allowedDirs` | string[] | `[]` | Additional directories the filesystem and exec tools can access beyond the workspace. |
| `disableTools` | string[] | `[]` | List of tool names to disable (e.g., `["exec", "web"]`). |
| `maxTurnMessages` | int | `0` | Maximum number of messages retained in a single turn before trimming. 0 = no limit. |
| `maxToolResultChars` | int | `0` | Maximum characters of a tool result sent to the LLM. 0 = no limit. |

### agents.defaults.sandbox

Controls the exec tool's security level.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | `"strict"` | Security mode: `"strict"`, `"permissive"`, or `"yolo"`. |

**Modes:**

| Mode | Commands | Absolute paths | Blacklist |
|------|----------|---------------|-----------|
| `strict` | Array-only (`["ls", "-la"]`) | ❌ Blocked | Full blacklist (rm -rf, sudo, etc.) |
| `permissive` | Array-only | ✅ Allowed | Dangerous only (dd, mkfs, shutdown) |
| `yolo` | String or array | ✅ Allowed | ❌ None |

### agents.defaults.web

Configures the built-in web fetch tool.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `timeoutS` | int | `30` | Maximum time in seconds for an HTTP request. |
| `maxResponseBytes` | int | `1048576` (1 MB) | Maximum response body size to read. |
| `userAgent` | string | `"GinoAI https://github.com/wltechblog/gino"` | User-Agent header sent with requests. |

The web tool only fetches text-based content (HTML, JSON, XML, plain text). Binary content types are rejected. Only `http://` and `https://` schemes are allowed.

### agents.defaults.compaction

Optional LLM-based context compaction. When enabled, older messages are summarized by the LLM instead of silently dropped.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Turn on LLM-based compaction. |
| `maxContextTokens` | int | `128000` | Estimated context window size in tokens. Compaction fires when usage approaches this. |
| `reserveTokens` | int | `16384` | Token budget reserved for the summarization prompt and response. |
| `keepRecentTokens` | int | `20000` | Tokens of recent messages to keep intact (not summarized). |
| `maxSummaryTokens` | int | `4000` | Cap on summary length to prevent unbounded growth. |

---

## providers

LLM provider configuration. Gino uses an OpenAI-compatible API provider.

### providers.openai

Connect to any OpenAI-compatible API service (OpenAI, OpenRouter, z.ai, Ollama, etc.).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `apiKey` | string | *(required)* | Your API key. Get OpenRouter keys at https://openrouter.ai/keys |
| `apiBase` | string | `https://openrouter.ai/api/v1` | API base URL. See examples below. |
| `reasoningEffort` | string | *(none)* | Value sent as the `reasoning_effort` request parameter for reasoning models. Must be in `reasoningLevels`. |
| `reasoningLevels` | string[] | `["none","minimal","low","medium","high"]` | Allowed vocabulary for `reasoningEffort`. Levels vary by model family — set this to match your model (e.g. gpt-5: `["minimal","low","medium","high"]`). Invalid values are rejected, never remapped. |

**Common API base URLs:**

| Service | API Base |
|---------|----------|
| OpenRouter | `https://openrouter.ai/api/v1` |
| OpenAI | `https://api.openai.com/v1` |
| z.ai Coding Plan | `https://api.z.ai/api/coding/paas/v4` |
| Local Ollama | `http://localhost:11434/v1` |

```json
{
  "providers": {
    "openai": {
      "apiKey": "sk-or-v1-your-key-here",
      "apiBase": "https://openrouter.ai/api/v1"
    }
  }
}
```

**Reasoning models** — the `reasoning_effort` parameter:

```json
{
  "providers": {
    "openai": {
      "apiKey": "sk-or-v1-your-key-here",
      "apiBase": "https://openrouter.ai/api/v1",
      "reasoningEffort": "high",
      "reasoningLevels": ["none", "minimal", "low", "medium", "high"]
    }
  }
}
```

`reasoningLevels` is the validation vocabulary — levels are **not consistent between model families**, so the operator declares what the target model actually accepts:

| Model family | reasoningLevels |
|--------------|-----------------|
| OpenAI o-series | `["low","medium","high"]` |
| OpenAI gpt-5 | `["minimal","low","medium","high"]` |
| GLM-5.3 / 5.3-FLASH (z.ai) | `["low","high","max"]` — only these three; anything else is a hard API error. `max` is the default |
| GLM-5.2 (z.ai) | `["none","minimal","low","medium","high","xhigh","max"]` — server remaps `low`/`medium`→`high`, `xhigh`→`max`; `none`/`minimal` stop thinking |
| GLM-5.1 and older | `["none"]` — no `reasoning_effort` support; control via `thinking.type` |
| Ollama thinking models | `["none"]` (`none` disables thinking) |

Note: z.ai's **Coding Plan** endpoint (`/api/coding/paas/v4`) is the exception — it accepts the broad vocabulary (`none`…`max`) and remaps server-side per model (`none`/`minimal`/`low`→`low`, `medium`/`high`→`high`, `xhigh`/`max`→`max` on GLM-5.3). The direct API endpoint (`/api/paas/v4`) does **not** — it hard-errors on out-of-vocabulary values.

`agents.defaults.reasoningEffort` applies the same parameter at the agent level (provider config wins; validated against the provider's `reasoningLevels`). Runtime: `gino chat -R <level>`, `gino agent -R <level>`, `/reasoning <level>` in the TUI. Env: `GINO_REASONING_EFFORT` + `GINO_REASONING_LEVELS` (comma-separated).

Each fallback entry also accepts `reasoningEffort` + its own `reasoningLevels` — the fallback chain may target a different model family than the primary, so vocabularies are per-provider.

### Provider Fallback

If no valid provider is configured, Gino uses a **Stub** provider (echoes back your message, for testing).

---

## mcpServers

Connect external [MCP (Model Context Protocol)](https://modelcontextprotocol.io) servers to give the agent additional tools. Each entry is a named server that exposes one or more tools, which are registered automatically at startup under the name `mcp_{server}_{tool}`.

Two transports are supported:

| Transport | When to use | Required fields |
|-----------|-------------|------------------|
| **Stdio** | Local process (npx, uvx, binary, docker) | `command` + `args` |
| **HTTP** | Remote or hosted MCP server | `url` (+ optional `headers`) |

### Stdio transport (command + args)

Gino spawns the process and communicates over stdin/stdout.

```json
{
  "mcpServers": {
    "via-npx": {
      "command": "npx",
      "args": ["-y", "@some/mcp-server"]
    }
  }
}
```

**Common patterns:**

```json
{
  "mcpServers": {
    "via-npx": {
      "command": "npx",
      "args": ["-y", "@some/mcp-server"]
    },
    "via-uvx": {
      "command": "uvx",
      "args": ["some-mcp-server"]
    },
    "via-binary": {
      "command": "/usr/local/bin/my-mcp-server",
      "args": ["--some-flag"]
    },
    "via-docker": {
      "command": "docker",
      "args": ["run", "--rm", "-i", "mcp/some-image"]
    }
  }
}
```

> **Docker note:** Always include `-i` (interactive) in the `args`. Without it, Docker closes stdin immediately and the MCP handshake fails.

### HTTP transport (url + headers)

For MCP servers accessible over HTTP (Streamable HTTP or SSE).

```json
{
  "mcpServers": {
    "via-remote": {
      "url": "https://mcp.example.com/mcp",
      "headers": {
        "Authorization": "Bearer YOUR_TOKEN"
      }
    }
  }
}
```

### MCPServerConfig fields

| Field | Type | Description |
|-------|------|-------------|
| `command` | string | Executable to spawn (stdio transport). |
| `args` | string[] | Arguments passed to the command. |
| `url` | string | HTTP endpoint (HTTP transport). |
| `headers` | object | HTTP headers to attach to every request. |
| `env` | object | Additional environment variables for the child process (stdio only). |

Only one transport is used per server: if both `command` and `url` are set, `command` takes precedence.

### Tool naming

Each MCP tool is registered as `mcp_{server}_{tool}`. For example, a server named `via-npx` exposing a tool `some-action` becomes `mcp_via-npx_some-action`.

### Startup behaviour

- Servers are connected when the agent starts.
- If a server fails to connect, Gino **logs the error and continues** — other servers and built-in tools are unaffected.
- All MCP connections are cleanly shut down when the gateway exits.

---

## channels

Chat channel integrations. Currently supports **Telegram** and **Discord**.

### channels.telegram

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Set to `true` to start the Telegram bot. |
| `token` | string | `""` | Your Telegram Bot token from [@BotFather](https://t.me/BotFather). |
| `allowFrom` | string[] | `[]` | List of allowed Telegram user IDs. Empty = allow all. |

```json
{
  "channels": {
    "telegram": {
      "enabled": true,
      "token": "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11",
      "allowFrom": ["8881234567"]
    }
  }
}
```

### channels.discord

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Set to `true` to start the Discord bot. |
| `token` | string | `""` | Your Discord Bot token from the [Developer Portal](https://discord.com/developers/applications). |
| `allowFrom` | string[] | `[]` | List of allowed Discord user IDs. Empty = allow all. |

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

---

## brain

Optional knowledge brain — a SQLite-backed store with hybrid search (full-text + vector embeddings). When enabled, the agent can ingest documents, search them semantically, and build a knowledge graph of entities and relationships.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Turn on the knowledge brain. |
| `embeddingModel` | string | `"nomic-embed-text"` | Ollama model used for generating embeddings. |
| `embeddingDims` | int | `768` | Dimensionality of the embedding vectors. Must match the model. |
| `ollamaBaseURL` | string | `"http://localhost:11434"` | Ollama server URL for local embeddings. |
| `remoteApiBase` | string | `""` | Fallback remote API base URL (if Ollama is unavailable). |
| `remoteApiKey` | string | `""` | Fallback remote API key. |
| `remoteModel` | string | `""` | Fallback remote model name. |

```json
{
  "brain": {
    "enabled": true,
    "embeddingModel": "nomic-embed-text",
    "ollamaBaseURL": "http://localhost:11434"
  }
}
```

---

## signal

External trigger system. When enabled, Gino listens on a Unix domain socket for signals from external sources (MCP servers, scripts, IoT devices) that can wake the agent and inject messages.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Turn on the signal listener. |
| `socketPath` | string | `{workspace}/.gino/signals.sock` | Unix domain socket path. |
| `defaultChannel` | string | `""` | Fallback channel for signals that don't specify one. |
| `defaultChatID` | string | `""` | Fallback chat ID for signals that don't specify one. |

### Signal routing

When multiple sessions are live (e.g. several Discord threads), signals route by this priority:

1. **Explicit** — `channel`/`chat_id` fields in the signal payload itself
2. **Per-source binding** — the session that most recently called a tool on the originating MCP server; gino stamps `channel`/`chat_id` into every tools/call request's `_meta`, and records the binding after each successful MCP tool call (persisted in `~/.gino/signal_routes.json`, survives restarts)
3. **Last known** — the channel/chatID of the most recent non-signal message
4. **Config defaults** — `defaultChannel`/`defaultChatID`

The per-source binding fixes multi-session misrouting: a trigger armed from thread A routes back to thread A even if thread B messaged more recently. Cooperating servers can echo the `_meta` origin in their signal payloads for exact routing (priority 1); without cooperation the binding (priority 2) applies.

### Signal actions

User-defined actions that external sources can send. The key is the action name, the value describes the safe response template injected into the agent.

| Field | Type | Description |
|-------|------|-------------|
| `description` | string | Human-readable description of the signal. |
| `response` | string | Message injected into the agent. Supports `{{.Source}}` and `{{.Timestamp}}` template variables. |
| `silent` | bool | When true, agent processes the signal but only replies if it has something useful to report. |

```json
{
  "signal": {
    "enabled": true,
    "defaultChannel": "telegram",
    "defaultChatID": "8881234567",
    "actions": {
      "motion_detected": {
        "description": "Motion sensor triggered",
        "response": "Motion was detected by {{.Source}} at {{.Timestamp}}. Check the cameras.",
        "silent": false
      },
      "check_messages": {
        "description": "Periodic message check",
        "response": "Check for any new messages.",
        "silent": true
      }
    }
  }
}
```

MCP servers can also self-declare their own signal actions at startup.
