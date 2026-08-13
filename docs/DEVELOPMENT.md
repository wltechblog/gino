# Development Guide

This document covers how to set up a local development environment, build, test, and publish Gino.

## What You'll Need

- [Go](https://go.dev/dl/) 1.26+ installed
- [Docker](https://www.docker.com/) installed (for container builds)

## Project Structure

```
cmd/gino/          CLI entry point (main.go)
embeds/               Embedded assets (sample skills bundled into binary)
  skills/             Sample skills extracted on onboard
internal/
  agent/              Agent loop, context, tools, skills, compaction
  brain/              Knowledge brain (SQLite + embeddings + entity graph)
  chat/               Chat message hub (Inbound / Outbound channels)
  channels/           Telegram and Discord integration
  config/             Config schema, loader, onboarding
  cron/               Cron scheduler
  heartbeat/          Periodic task checker
  mcp/                MCP client (stdio + HTTP transports)
  memory/             Memory read/write/rank
  providers/          OpenAI-compatible provider (OpenAI, OpenRouter, z.ai, Ollama, etc.)
  session/            Session manager
  signal/             External trigger listener (Unix domain socket)
docker/               Dockerfile, compose, entrypoint
docs/                 Documentation
```

## Local Development

### Clone and install dependencies

```sh
git clone https://github.com/wltechblog/gino.git
cd gino
go mod download
```

### Build the binary

```sh
go build -o gino ./cmd/gino
```

### Run locally

```sh
# First time? Run onboard to create ~/.gino config and workspace
./gino onboard

# Try a quick query
./gino agent -m "Hello!"

# Work on a repository while keeping the persistent Gino profile
./gino chat --project /path/to/repo
./gino agent --project /path/to/repo -m "What does this repo do?"

# Login to channels (Telegram, Discord)
./gino channels login

# Start the full gateway (includes channels, heartbeat, etc.)
./gino gateway
```

### Run tests

```sh
# Run all tests
go test ./...

# Run tests for a specific package
go test ./internal/brain/
go test ./internal/agent/

# Run tests with verbose output
go test -v ./...
```

### Run go vet

```sh
go vet ./...
```

### Run golangci-lint

The project uses [golangci-lint](https://golangci-lint.run/) with errcheck enabled in CI.

```sh
# Lint all packages
golangci-lint run

# Lint a specific package
golangci-lint run ./internal/agent/...
```

## Versioning

The version string is defined in `cmd/gino/main.go`:

```go
const version = "0.4.0"
```

Update this value before building a new release.

## Building for Different Platforms

### Quick builds with Make

The project ships a `Makefile` that cross-compiles all supported platforms:

```sh
# Build all targets
make build-all

# Build individual targets
make linux_amd64            # full build, Linux x86-64
make linux_arm64            # full build, Linux ARM64
make mac_arm64              # full build, macOS Apple Silicon
make linux_amd64_telegram   # Telegram-only (smaller binary)
make linux_arm64_telegram   # Telegram-only, ARM64
make mac_arm64_telegram     # Telegram-only, Apple Silicon

make clean                  # remove all built binaries
```

### Manual cross-compilation

```sh
# Linux AMD64
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o gino_linux_amd64 ./cmd/gino

# Linux ARM64
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o gino_linux_arm64 ./cmd/gino

# macOS ARM64 (Apple Silicon)
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o gino_mac_arm64 ./cmd/gino

# Windows
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o gino.exe ./cmd/gino
```

**What the flags do:**
- `CGO_ENABLED=0` → pure static binary, no libc dependency
- `-ldflags="-s -w"` → strip debug symbols

### Telegram-only builds

For deployments that only need Telegram (no Discord), use the `only_telegram` build tag for a smaller binary:

```sh
go build -tags only_telegram -o gino ./cmd/gino
```

## Docker Workflow

### Build the image

Build from the **project root** (not from inside `docker/`):

```sh
docker build -f docker/Dockerfile -t wltechblog/gino:latest .
```

#### Multi-arch builds with BuildKit

```sh
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t wltechblog/gino:latest .
```

Add `--push` to publish directly to Docker Hub or `--load` to import into your local Docker engine.

### Test it locally

```sh
docker run --rm -it \
  -e OPENAI_API_KEY="your-key" \
  -e OPENAI_API_BASE="https://openrouter.ai/api/v1" \
  -e GINO_MODEL="google/gemini-2.5-flash" \
  -e TELEGRAM_BOT_TOKEN="your-token" \
  -v /opt/gino/data:/home/gino/.gino \
  wltechblog/gino:latest
```

### Push to Docker Hub

```sh
docker build -f docker/Dockerfile -t wltechblog/gino:latest . && \
docker push wltechblog/gino:latest
```

## Environment Variables

These environment variables configure the Docker container:

### Core

| Variable | Default | Description | Required |
|---|---|---|---|
| `OPENAI_API_KEY` | *(none)* | OpenAI-compatible API key | **Yes** |
| `OPENAI_API_BASE` | `https://openrouter.ai/api/v1` | API base URL | No |
| `GINO_MODEL` | `stub-model` | LLM model to use | No |
| `GINO_MAX_TOKENS` | `8192` | Maximum tokens for LLM responses | No |
| `GINO_MAX_TOOL_ITERATIONS` | `100` | Max tool-calling iterations per request | No |

### Channels

| Variable | Default | Description | Required |
|---|---|---|---|
| `TELEGRAM_BOT_TOKEN` | *(none)* | Telegram Bot API token | No |
| `TELEGRAM_ALLOW_FROM` | *(none)* | Comma-separated Telegram user IDs | No |
| `DISCORD_BOT_TOKEN` | *(none)* | Discord Bot token | No |
| `DISCORD_ALLOW_FROM` | *(none)* | Comma-separated Discord user IDs | No |

### Brain (Knowledge System)

| Variable | Default | Description | Required |
|---|---|---|---|
| `GINO_BRAIN_ENABLED` | `true` | Enable the knowledge brain | No |
| `GINO_BRAIN_EMBEDDING_MODEL` | `nomic-embed-text` | Ollama embedding model | No |
| `OLLAMA_URL` | *(none)* | External Ollama URL (skip bundled Ollama) | No |
| `GINO_BRAIN_REMOTE_API_BASE` | *(none)* | Fallback remote embedding API base | No |
| `GINO_BRAIN_REMOTE_API_KEY` | *(none)* | Fallback remote embedding API key | No |
| `GINO_BRAIN_REMOTE_MODEL` | *(none)* | Fallback remote embedding model | No |

### Data

| Variable | Default | Description | Required |
|---|---|---|---|
| `GINO_DATA_PATH` | `/home/gino/.gino` | Data directory for persistence | No |

## Extending Gino

### Adding a new tool

Let's say you want to add a `database` tool that queries PostgreSQL:

1. **Create the file:**
   ```sh
   touch internal/agent/tools/database.go
   ```

2. **Implement the tool type:**
   ```go
   package tools

   import (
       "context"
       "fmt"
   )

   type DatabaseTool struct {
       connString string
   }

   func NewDatabaseTool(connString string) *DatabaseTool {
       return &DatabaseTool{connString: connString}
   }

   func (t *DatabaseTool) Name() string { return "database" }
   func (t *DatabaseTool) Description() string {
       return "Query a PostgreSQL database. Args: {\"sql\": \"SELECT ...\"}"
   }
   func (t *DatabaseTool) Parameters() map[string]any {
       return map[string]any{
           "type": "object",
           "properties": map[string]any{
               "sql": map[string]any{
                   "type": "string",
                   "description": "SQL query to execute",
               },
           },
           "required": []string{"sql"},
       }
   }
   func (t *DatabaseTool) Execute(ctx context.Context, args map[string]any) (string, error) {
       // Implementation here
       return "result", nil
   }
   ```

3. **Register it in the agent loop** (`internal/agent/loop.go`):
   ```go
   register("database", tools.NewDatabaseTool("postgres://..."))
   ```

### Adding a new channel

Channels implement the chat client interface. See `internal/channels/telegram.go` and `internal/channels/discord.go` for reference.

### Using build tags to exclude channels

For single-channel deployments, use build tags to strip unused channels:

```sh
# Telegram only
go build -tags only_telegram -o gino ./cmd/gino

# Discord only
go build -tags only_discord -o gino ./cmd/gino
```

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feat/my-feature`)
3. Run tests and lint: `go test ./... && golangci-lint run`
4. Commit your changes
5. Open a pull request

### Code style

- Run `go fmt` before committing
- Run `go vet` and fix all warnings
- Check error returns (CI enforces errcheck)
- Keep functions focused and small
