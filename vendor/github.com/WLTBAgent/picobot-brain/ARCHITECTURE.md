# Picobot Brain — Architecture Document

A Go-native, gbrain-inspired memory system designed for Picobot agents running on Raspberry Pi and other constrained devices.

## Design Principles

1. **Single binary, zero dependencies** — SQLite (modernc.org/sqlite, pure Go, no CGO) + embedded vector search
2. **Runs on a Pi** — target ≤64MB RAM for the brain subsystem, binary <5MB
3. **Picobot-native** — integrates as `internal/brain/` inside Picobot, not a separate daemon
4. **Offline-first** — all search and retrieval works without LLM calls. Embeddings are the only outbound traffic (optional local models via Ollama)
5. **Progressive complexity** — keyword search works day one, vector search with a single API key, knowledge graph is opt-in

## What We're Taking from gbrain

| gbrain Feature | Our Adaptation |
|---|---|
| Hybrid search (vector + keyword + RRF) | SQLite FTS5 + sqlite-vec + Reciprocal Rank Fusion |
| Self-wiring knowledge graph | Entity extraction → typed edges, stored in SQLite |
| Multi-source tenancy | Scoped "sources" in SQLite (wiki, conversations, imported) |
| Nightly maintenance cycle (`gbrain dream`) | `brain maintain` — backfill embeddings, extract entities, prune orphans |
| Content-hash dedup | Hash-on-write prevents duplicate ingestion |
| Source-aware ranking | Boost curated content over raw dumps |
| Eval harness | BrainBench-style regression tests built into `go test` |

## What We're NOT Taking

- **PGLite/WASM** — SQLite instead (pure Go, zero install, works on ARM)
- **TypeScript/Bun** — Pure Go
- **OAuth 2.1 HTTP server** — Picobot already has channel auth; brain is internal
- **Admin dashboard** — CLI + Picobot tools only
- **35 scaffolded skills** — Picobot has its own skill system
- **108 cron jobs** — Picobot has its own cron/heartbeat

## Storage: SQLite

```
~/.picobot/brain.db
```

Pure Go via `modernc.org/sqlite` (already used by Picobot's WhatsApp). No CGO, cross-compiles cleanly for ARM.

### Extensions
- **FTS5** — built into SQLite, full-text search with BM25 ranking
- **sqlite-vec** — vector similarity search (HNSW-like) as a SQLite extension. Pure C, compiles with CGO — BUT we can also ship prebuilt `.so` for Pi, or use a pure-Go brute-force KNN for small brains (<100K items)
- **Fallback path** — if sqlite-vec is unavailable, fall back to brute-force cosine similarity in Go (fine for <50K vectors on a Pi)

### Schema

```sql
-- Sources: logical partitions (conversations, notes, imported, etc.)
CREATE TABLE sources (
    id          TEXT PRIMARY KEY,          -- 'default', 'conversations', 'imported'
    name        TEXT NOT NULL UNIQUE,
    local_path  TEXT,                       -- optional filesystem root
    config      TEXT NOT NULL DEFAULT '{}', -- JSON
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Pages: the core content unit
CREATE TABLE pages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id     TEXT NOT NULL DEFAULT 'default' REFERENCES sources(id),
    slug          TEXT NOT NULL,             -- e.g., '2026-05-18', 'people/alice'
    type          TEXT NOT NULL DEFAULT 'note', -- note, person, company, concept, conversation
    title         TEXT NOT NULL DEFAULT '',
    content       TEXT NOT NULL DEFAULT '',
    content_hash  TEXT,                       -- SHA-256 for dedup
    metadata      TEXT NOT NULL DEFAULT '{}', -- JSON frontmatter
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(source_id, slug)
);

-- FTS5 virtual table for full-text search
CREATE VIRTUAL TABLE pages_fts USING fts5(
    title, content, metadata,
    content='pages', content_rowid='id'
);

-- Embeddings: one vector per page
CREATE TABLE embeddings (
    page_id     INTEGER PRIMARY KEY REFERENCES pages(id) ON DELETE CASCADE,
    model       TEXT NOT NULL,              -- e.g., 'text-embedding-3-small'
    vector      BLOB NOT NULL,              -- float32 array, serialized
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Entities: extracted people, companies, concepts
CREATE TABLE entities (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id   TEXT NOT NULL DEFAULT 'default' REFERENCES sources(id),
    name        TEXT NOT NULL,
    type        TEXT NOT NULL DEFAULT 'person', -- person, company, concept, place
    slug        TEXT NOT NULL UNIQUE,        -- e.g., 'people/alice-smith'
    page_id     INTEGER REFERENCES pages(id),   -- optional detail page
    metadata    TEXT NOT NULL DEFAULT '{}',
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Edges: typed relationships between entities
CREATE TABLE edges (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id     INTEGER NOT NULL REFERENCES entities(id),
    to_id       INTEGER NOT NULL REFERENCES entities(id),
    type        TEXT NOT NULL,               -- works_at, attended, invested_in, founded, mentions
    source_page INTEGER REFERENCES pages(id), -- where we learned this
    confidence  REAL NOT NULL DEFAULT 1.0,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(from_id, to_id, type, source_page)
);

-- Ingest log: track what's been imported
CREATE TABLE ingest_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id   TEXT NOT NULL REFERENCES sources(id),
    path        TEXT NOT NULL,               -- file path or origin
    content_hash TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'ok',  -- ok, skipped, error
    ingested_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(source_id, content_hash)
);
```

## Package Layout

```
internal/brain/
├── brain.go           // Brain struct, init, close
├── store.go           // SQLite CRUD for pages, sources, ingest log
├── search.go          // Hybrid search: FTS5 + vector + RRF
├── search_fts.go      // FTS5-specific queries
├── search_vector.go   // Vector similarity (sqlite-vec or brute-force)
├── search_rrf.go      // Reciprocal Rank Fusion
├── embed.go           // Embedding client (OpenAI-compatible API)
├── entity.go          // Entity extraction (regex + optional LLM)
├── graph.go           // Knowledge graph queries (traversal, neighbors)
├── ingest.go          // File import (markdown dirs, conversations)
├── maintain.go        // Nightly maintenance cycle
├── schema.go          // DDL + migrations
├── vector_fallback.go // Pure Go brute-force KNN (no CGO)
└── brain_test.go
```

## Core API

```go
package brain

// Init opens/creates the brain database at the given path.
func Init(dbPath string) (*Brain, error)

// --- Ingest ---

// IngestPage writes a page and triggers FTS index + optional embedding.
func (b *Brain) IngestPage(ctx context.Context, page Page) (int64, error)

// IngestDir walks a directory of markdown files and imports new/changed ones.
func (b *Brain) IngestDir(ctx context.Context, sourceID, dirPath string) (imported int, err error)

// IngestConversation stores a conversation transcript as a page.
func (b *Brain) IngestConversation(ctx context.Context, conv Conversation) (int64, error)

// --- Search ---

// SearchResult is a ranked page with score and snippet.
type SearchResult struct {
    PageID    int64
    Slug      string
    Title     string
    Snippet   string
    Score     float64
    Source    string
}

// Search performs hybrid search (FTS5 + vector + RRF).
func (b *Brain) Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error)

// SearchOpts configures a search query.
type SearchOpts struct {
    Limit       int      // max results (default 10)
    Sources     []string // restrict to these sources
    Types       []string // restrict to these page types
    MinScore    float64  // minimum RRF score threshold
    VectorOnly  bool     // skip FTS, vector search only
    KeywordOnly bool     // skip vector, FTS only
}

// --- Knowledge Graph ---

// Entity is a person, company, concept, etc.
type Entity struct {
    ID       int64
    Name     string
    Type     string
    Slug     string
    Metadata map[string]any
}

// Edge is a typed relationship.
type Edge struct {
    FromID     int64
    ToID       int64
    Type       string
    SourcePage int64
    Confidence float64
}

// ExtractEntities parses a page for entity references and creates/updates edges.
// Uses regex patterns first (email, @mention, [[wikilink]], capital patterns).
// Optionally uses LLM for deeper extraction.
func (b *Brain) ExtractEntities(ctx context.Context, pageID int64, useLLM bool) error

// GraphNeighbors returns entities connected to the given entity.
func (b *Brain) GraphNeighbors(ctx context.Context, entityID int64, depth int) ([]Entity, []Edge, error)

// --- Maintenance ---

// Maintain runs the maintenance cycle:
//   1. Backfill missing embeddings
//   2. Extract entities from unprocessed pages
//   3. Rebuild stale FTS entries
//   4. Prune orphaned entities/edges
//   5. Deduplicate pages by content_hash
func (b *Brain) Maintain(ctx context.Context) (*MaintainReport, error)
```

## Hybrid Search Pipeline

This is the core value prop from gbrain, adapted for SQLite:

```
Query "what did Alice and I discuss last quarter?"
         │
         ├───── FTS5 (BM25) ──────┐
         │     fast keyword match  │
         │                         │
         ├───── Vector KNN ────────┤
         │     semantic similarity │
         │                         │
         └───── RRF Fusion ────────┘
               merge & re-rank
               (source-aware boost)
                    │
                    ▼
            Top-K SearchResults
```

### Reciprocal Rank Fusion

```
score(page) = Σ (1 / (k + rank_i))   for each method i
```

Where `k = 60` (standard RRF constant). Simple, effective, no weights to tune.

### Source-Aware Boosting

Pages from curated sources (notes, people pages) get a 1.5x multiplier over raw dumps (conversation transcripts, imported articles). Configurable per source.

## Embedding Strategy

**Default:** Call an OpenAI-compatible embedding API (`text-embedding-3-small` or equivalent).

**Pi-friendly options:**
1. **Remote API** — OpenAI, Voyage, etc. One API call per page, batched during maintenance
2. **Ollama local** — `nomic-embed-text` (274MB) runs on Pi 5, zero network traffic
3. **No embeddings** — fall back to FTS5-only search (still better than Picobot's current keyword ranker)

Embeddings are computed lazily — pages are searchable via FTS5 immediately, vectors backfilled by `Maintain()`.

## Entity Extraction (Zero LLM Path)

The self-wiring graph is gbrain's killer feature. We replicate it without requiring LLM calls:

1. **Wikilinks** — `[[Alice Smith]]` → entity link, type inferred from context
2. **@mentions** — `@alice` → person entity
3. **Email addresses** — person entity
4. **Markdown headers** — `## People`, `## Companies` sections → entity type hints
5. **Pattern matching** — capital-letter sequences near role words ("CEO of X", "works at Y")
6. **Frontmatter** — YAML `people: [Alice, Bob]` → explicit entities

Optional LLM extraction for richer results when an API key is configured.

## Integration with Picobot

### As Internal Package

The brain lives inside Picobot at `internal/brain/`. The Picobot agent loop gets two new capabilities:

1. **Automatic context enrichment** — before each LLM call, Picobot queries the brain for relevant context and injects it into the system prompt
2. **New tools** — `brain_search`, `brain_ingest`, `brain_graph` exposed as Picobot tools

### New Picobot Tools

```
brain_search     — hybrid search across all sources
brain_ingest     — import a file or directory into the brain
brain_entity     — look up entity and its relationships
brain_maintain   — trigger maintenance cycle
brain_status     — show brain stats (pages, entities, edges)
```

### Context Injection

In `internal/agent/context.go`, add a brain query step:

```go
// After loading memory/sessions/skills, before LLM call:
if brain != nil {
    results, _ := brain.Search(ctx, userMessage, brain.SearchOpts{Limit: 5})
    // Inject as "Relevant Brain Context" section in system prompt
}
```

### Backward Compatibility

If `brain.db` doesn't exist, Picobot works exactly as before with the flat-file memory system. Brain is opt-in.

## Memory Comparison

| Capability | Picobot Now | Picobot + Brain |
|---|---|---|
| Daily notes | ✅ flat files | ✅ flat files + SQLite |
| Long-term memory | ✅ MEMORY.md | ✅ MEMORY.md + structured pages |
| Search | keyword overlap | FTS5 + vector + RRF |
| Knowledge graph | ❌ | ✅ entities + typed edges |
| Dedup | ❌ | ✅ content-hash |
| Semantic search | ❌ | ✅ embeddings |
| Entity lookup | ❌ | ✅ "who works at X?" |
| Multi-source | ❌ | ✅ scoped sources |
| Runs on Pi | ✅ | ✅ (same binary, +brain.db) |

## Build & Runtime Targets

| Target | RAM Budget | Notes |
|---|---|---|
| Raspberry Pi 5 (8GB) | ≤64MB for brain | Ollama embeddings possible |
| Raspberry Pi 4 (4GB) | ≤32MB for brain | Remote embeddings only |
| Pi Zero 2W (512MB) | ≤16MB for brain | FTS5-only mode, no vectors |
| VPS (1GB) | ≤64MB | All features |

## Phased Implementation

### Phase 1: Foundation (Week 1-2)
- `internal/brain/` package with SQLite store
- Schema + migrations
- Page CRUD + FTS5 search
- `brain_search` tool for Picobot
- **Deliverable:** Hybrid keyword search that beats current SimpleRanker

### Phase 2: Vectors (Week 3-4)
- Embedding client (OpenAI-compatible)
- Vector storage + brute-force KNN
- RRF fusion of FTS5 + vector results
- Content-hash dedup on ingest
- **Deliverable:** Semantic search that finds related content across daily notes

### Phase 3: Knowledge Graph (Week 5-6)
- Entity extraction (regex/wikilink patterns)
- Edge creation + graph storage
- Graph-augmented search (boost results connected to query entities)
- `brain_entity` tool
- **Deliverable:** "Who works at X?" queries work without LLM

### Phase 4: Maintenance & Polish (Week 7-8)
- Nightly maintenance cycle (embeddings backfill, entity extraction, orphan pruning)
- `brain_maintain` tool + Picobot heartbeat integration
- Source-aware ranking
- Multi-directory import
- sqlite-vec integration (optional, for larger brains)
- **Deliverable:** Self-maintaining brain that improves overnight

### Phase 5: LLM Extraction (Optional)
- Optional LLM-based entity extraction for richer graph
- Conversation summarization → page creation
- Contradiction detection across pages

## Dependencies

```go
require (
    modernc.org/sqlite      // pure Go SQLite (already in Picobot)
    github.com/mattn/go-sqlite3  // alternative: CGO SQLite with extensions
)
```

**Embeddings:** Uses Picobot's existing OpenAI-compatible provider. No new HTTP clients needed.

**sqlite-vec:** Optional. Can be loaded as a SQLite extension at runtime if the `.so` is present, otherwise falls back to pure-Go KNN.

## Why This Works on a Pi

1. **SQLite is tiny** — the database for 100K pages would be ~2-5GB on disk, but queries use minimal RAM
2. **FTS5 is fast** — BM25 ranking in SQLite is highly optimized, sub-millisecond for typical queries
3. **Vector search scales** — brute-force KNN on 50K vectors (1536-dim) takes ~50ms on a Pi 5. For larger brains, sqlite-vec with HNSW indexing drops this to ~5ms
4. **No running services** — it's a library, not a daemon. Zero overhead when not querying
5. **Lazy embeddings** — compute them during maintenance, not during search. Search itself is always local

## Open Questions

1. **sqlite-vec on ARM** — need to verify compilation for `linux/arm64`. Fallback is brute-force KNN which is fine for <100K items
2. **Embedding model choice** — `text-embedding-3-small` (1536-dim) vs `nomic-embed-text` (768-dim, runs locally on Pi 5). Smaller vectors = faster search
3. **Migration from flat files** — should `brain init` auto-import existing `memory/*.md` files?
4. **Picobot upstream PR** — do we keep this as WLTBAgent-only or propose to louisho5?
