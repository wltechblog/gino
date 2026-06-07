package brain

import (
	"context"
	"time"
)

// Brain is the core knowledge brain — a SQLite-backed store with hybrid search
// and an optional knowledge graph. It has zero dependencies on any specific agent
// framework and is designed to be embedded as a library.
type Brain struct {
	db       DB
	embedder EmbeddingProvider
	opts     Options
}

// Options configures brain behavior.
type Options struct {
	// EmbeddingModel is the model identifier sent to the embedding provider.
	// Default: "nomic-embed-text"
	EmbeddingModel string

	// EmbeddingDims is the dimensionality of the embedding vectors.
	// Default: 768 (nomic-embed-text). Set to 0 to disable vector search.
	EmbeddingDims int

	// DefaultSourceID is the source for pages ingested without an explicit source.
	// Default: "default"
	DefaultSourceID string
}

// DefaultOptions returns sensible defaults for a Pi-scale brain.
func DefaultOptions() Options {
	return Options{
		EmbeddingModel:  "nomic-embed-text",
		EmbeddingDims:   768,
		DefaultSourceID: "default",
	}
}

// Page is the core content unit in the brain.
type Page struct {
	ID          int64             `json:"id"`
	SourceID    string            `json:"source_id"`
	Slug        string            `json:"slug"`
	Type        string            `json:"type"`        // note, person, company, concept, conversation
	Title       string            `json:"title"`
	Content     string            `json:"content"`
	ContentHash string            `json:"content_hash"`
	Metadata    map[string]string `json:"metadata"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Source is a logical partition within the brain.
type Source struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	LocalPath string            `json:"local_path,omitempty"`
	Config    map[string]string `json:"config,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// Entity is a person, company, concept, or place extracted from pages.
type Entity struct {
	ID       int64             `json:"id"`
	SourceID string            `json:"source_id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"` // person, company, concept, place
	Slug     string            `json:"slug"`
	PageID   *int64            `json:"page_id,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Edge is a typed relationship between two entities.
type Edge struct {
	ID         int64   `json:"id"`
	FromID     int64   `json:"from_id"`
	ToID       int64   `json:"to_id"`
	Type       string  `json:"type"` // works_at, attended, invested_in, founded, mentions
	SourcePage int64   `json:"source_page"`
	Confidence float64 `json:"confidence"`
}

// SearchResult is a ranked page returned by search.
type SearchResult struct {
	PageID  int64   `json:"page_id"`
	Slug    string  `json:"slug"`
	Title   string  `json:"title"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"`
	Source  string  `json:"source"`
	Type    string  `json:"type"`
}

// SearchOpts configures a search query.
type SearchOpts struct {
	Limit       int      `json:"limit,omitempty"`        // max results (default 10)
	Sources     []string `json:"sources,omitempty"`      // restrict to these source IDs
	Types       []string `json:"types,omitempty"`         // restrict to these page types
	MinScore    float64  `json:"min_score,omitempty"`     // minimum RRF score
	VectorOnly  bool     `json:"vector_only,omitempty"`   // skip FTS, vector search only
	KeywordOnly bool     `json:"keyword_only,omitempty"`  // skip vector, FTS only
}

// MaintainReport summarizes what a maintenance cycle did.
type MaintainReport struct {
	EmbeddingsBackfilled int `json:"embeddings_backfilled"`
	EntitiesExtracted    int `json:"entities_extracted"`
	FTSRebuilt           int `json:"fts_rebuilt"`
	OrphansPruned        int `json:"orphans_pruned"`
	DuplicatesMerged     int `json:"duplicates_merged"`
	ElapsedMs            int `json:"elapsed_ms"`
}

// EmbeddingProvider is the interface for computing text embeddings.
// Implementations can call a remote API, use a local Ollama instance,
// or do anything else. The brain module does not depend on any specific provider.
type EmbeddingProvider interface {
	// Embed computes the embedding vector for the given text.
	// Returns a float32 slice of length `dims` (matching Options.EmbeddingDims).
	Embed(ctx context.Context, text string) ([]float32, error)

	// EmbedBatch computes embeddings for multiple texts in one call.
	// Implementations may batch API calls internally.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

	// ModelName returns the model identifier.
	ModelName() string
}

// DB is the interface for database operations. This allows testing with mocks.
type DB interface {
	Exec(query string, args ...any) (Result, error)
	Query(query string, args ...any) (Rows, error)
	QueryRow(query string, args ...any) Row
	Close() error
}

// Result is the interface for a database exec result.
type Result interface {
	LastInsertId() (int64, error)
	RowsAffected() (int64, error)
}

// Rows is the interface for database query result rows.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

// Row is the interface for a single database row.
type Row interface {
	Scan(dest ...any) error
}

// Init opens or creates a brain database at the given path.
// If embedder is nil, vector search is disabled (FTS5-only mode).
func Init(dbPath string, embedder EmbeddingProvider, opts Options) (*Brain, error) {
	if opts.DefaultSourceID == "" {
		opts.DefaultSourceID = "default"
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, err
	}
	// Seed default source
	db.Exec(`INSERT INTO sources (id, name, config) VALUES (?, ?, '{}')
		ON CONFLICT(id) DO NOTHING`, opts.DefaultSourceID, opts.DefaultSourceID)

	return &Brain{db: db, embedder: embedder, opts: opts}, nil
}

// Close closes the underlying database connection.
func (b *Brain) Close() error {
	return b.db.Close()
}
