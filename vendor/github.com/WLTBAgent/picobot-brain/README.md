# picobot-brain

Go-native knowledge brain for AI agents — hybrid RAG search, self-wiring knowledge graph, zero dependencies.

Designed for [Picobot](https://github.com/WLTBAgent/picobot) but usable by any Go project.

## Features

- **Hybrid Search** — FTS5 (BM25) + vector similarity, merged via Reciprocal Rank Fusion
- **Knowledge Graph** — auto-extracted entities and typed edges from `[[wikilinks]]`, `@mentions`, and relationship patterns
- **SQLite-native** — pure Go via modernc.org/sqlite, no CGO, runs on Raspberry Pi
- **Embeddings** — local (Ollama/nomic-embed-text) or remote (OpenAI-compatible) APIs
- **Content dedup** — SHA-256 content hashing prevents duplicate ingestion
- **Graceful degradation** — FTS5-only mode when no embedding provider is available

## Quick Start

```go
import brain "github.com/WLTBAgent/picobot-brain"

// Init with local Ollama embeddings
embedder := brain.NewOllamaProvider(brain.OllamaConfig{Model: "nomic-embed-text"})
b, _ := brain.Init("brain.db", embedder, brain.DefaultOptions())

// Ingest pages
b.IngestPage(ctx, brain.Page{
    Slug: "notes/hello", Title: "Hello",
    Content: "Notes about Go and Raspberry Pi",
})

// Import existing markdown directory
b.IngestDir(ctx, "notes", "/path/to/notes/")

// Search
results, _ := b.Search(ctx, "Raspberry Pi Go", brain.SearchOpts{Limit: 5})

// Maintain (backfill embeddings, extract entities, prune orphans)
b.Maintain(ctx)
```

## No Embeddings? No Problem

```go
// FTS5-only mode — still better than keyword matching alone
b, _ := brain.Init("brain.db", nil, brain.DefaultOptions())
```

## Embedding Providers

| Provider | Model | Size | Quality (MTEB) | Runs on Pi? |
|----------|-------|------|----------------|-------------|
| Ollama local | nomic-embed-text | 274MB | 62.39 | Pi 5 |
| Ollama local | granite-embedding:30m | 60MB | ~50 | Pi Zero 2W |
| Remote API | text-embedding-3-small | — | 62.3 | N/A |

## Setup

See [docs/OLLAMA_SETUP.md](docs/OLLAMA_SETUP.md) for Ollama installation (Docker, native, Pi, cloud fallback).

## Picobot Integration

Add to `~/.picobot/config.json`:

```json
{
  "brain": {
    "enabled": true,
    "embeddingModel": "nomic-embed-text"
  }
}
```

This gives Picobot five new tools: `brain_search`, `brain_ingest`, `brain_entity`, `brain_status`, and `brain_maintain`. Brain context is automatically injected into every LLM call.

On first run, existing `memory/` files are auto-imported into the brain.

## License

MIT
