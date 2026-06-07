package brain

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// IngestPage writes a page to the brain. If a page with the same (source_id, slug)
// exists, it is updated. Content-hash dedup prevents redundant embedding work.
func (b *Brain) IngestPage(ctx context.Context, page Page) (int64, error) {
	hash := contentHash(page.Content)
	page.ContentHash = hash

	if page.SourceID == "" {
		page.SourceID = b.opts.DefaultSourceID
	}
	if page.Type == "" {
		page.Type = "note"
	}

	// Auto-create source if it doesn't exist (prevents FK violation)
	b.db.Exec(`INSERT OR IGNORE INTO sources (id, name) VALUES (?, ?)`, page.SourceID, page.SourceID)

	// Upsert: insert or update by (source_id, slug)
	now := time.Now().UTC().Format(time.RFC3339)
	metaJSON, _ := json.Marshal(page.Metadata)

	res, err := b.db.Exec(`
		INSERT INTO pages (source_id, slug, type, title, content, content_hash, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_id, slug) DO UPDATE SET
			title = excluded.title,
			content = excluded.content,
			content_hash = excluded.content_hash,
			type = excluded.type,
			metadata = excluded.metadata,
			updated_at = excluded.updated_at`,
		page.SourceID, page.Slug, page.Type, page.Title, page.Content, hash, string(metaJSON), now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("ingest page: %w", err)
	}

	pageID, _ := res.LastInsertId()
	// For ON CONFLICT DO UPDATE, LastInsertId may be 0 on some SQLite drivers
	// Check if we need to look up the existing ID
	if pageID == 0 {
		err := b.db.QueryRow(`SELECT id FROM pages WHERE source_id = ? AND slug = ?`,
			page.SourceID, page.Slug).Scan(&pageID)
		if err != nil {
			return 0, fmt.Errorf("ingest page (lookup): %w", err)
		}
	}

	// Async embedding: fire and forget if embedder is configured
	if b.embedder != nil && b.opts.EmbeddingDims > 0 {
		go b.backfillEmbedding(pageID, page.Content)
	}

	return pageID, nil
}

// GetPage retrieves a page by source and slug.
func (b *Brain) GetPage(ctx context.Context, sourceID, slug string) (*Page, error) {
	var p Page
	var metaJSON string
	err := b.db.QueryRow(`
		SELECT id, source_id, slug, type, title, content, content_hash, metadata, created_at, updated_at
		FROM pages WHERE source_id = ? AND slug = ?`, sourceID, slug,
	).Scan(&p.ID, &p.SourceID, &p.Slug, &p.Type, &p.Title, &p.Content, &p.ContentHash, &metaJSON, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(metaJSON), &p.Metadata)
	return &p, nil
}

// GetPageByID retrieves a page by ID.
func (b *Brain) GetPageByID(ctx context.Context, id int64) (*Page, error) {
	var p Page
	var metaJSON string
	err := b.db.QueryRow(`
		SELECT id, source_id, slug, type, title, content, content_hash, metadata, created_at, updated_at
		FROM pages WHERE id = ?`, id,
	).Scan(&p.ID, &p.SourceID, &p.Slug, &p.Type, &p.Title, &p.Content, &p.ContentHash, &metaJSON, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(metaJSON), &p.Metadata)
	return &p, nil
}

// DeletePage removes a page and its associated embeddings.
func (b *Brain) DeletePage(ctx context.Context, id int64) error {
	_, err := b.db.Exec(`DELETE FROM pages WHERE id = ?`, id)
	return err
}

// ListPages returns pages matching the given filters.
func (b *Brain) ListPages(ctx context.Context, sourceID string, pageType string, limit int, offset int) ([]Page, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, source_id, slug, type, title, content_hash, metadata, created_at, updated_at
		FROM pages WHERE 1=1`
	args := []any{}
	if sourceID != "" {
		query += ` AND source_id = ?`
		args = append(args, sourceID)
	}
	if pageType != "" {
		query += ` AND type = ?`
		args = append(args, pageType)
	}
	query += ` ORDER BY updated_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := b.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pages []Page
	for rows.Next() {
		var p Page
		var metaJSON string
		if err := rows.Scan(&p.ID, &p.SourceID, &p.Slug, &p.Type, &p.Title, &p.ContentHash, &metaJSON, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(metaJSON), &p.Metadata)
		pages = append(pages, p)
	}
	return pages, rows.Err()
}

// PageCount returns the total number of pages, optionally filtered by source.
func (b *Brain) PageCount(ctx context.Context, sourceID string) (int, error) {
	query := `SELECT COUNT(*) FROM pages`
	args := []any{}
	if sourceID != "" {
		query += ` WHERE source_id = ?`
		args = append(args, sourceID)
	}
	var count int
	err := b.db.QueryRow(query, args...).Scan(&count)
	return count, err
}

// AddSource creates a new source partition.
func (b *Brain) AddSource(ctx context.Context, id, name, localPath string) error {
	_, err := b.db.Exec(`INSERT INTO sources (id, name, local_path) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, local_path = excluded.local_path`,
		id, name, localPath)
	return err
}

// HasContentHash checks if content with the given hash has already been ingested.
func (b *Brain) HasContentHash(ctx context.Context, sourceID, hash string) (bool, error) {
	var count int
	err := b.db.QueryRow(`SELECT COUNT(*) FROM ingest_log WHERE source_id = ? AND content_hash = ?`,
		sourceID, hash).Scan(&count)
	return count > 0, err
}

// RecordIngest logs a successful import.
func (b *Brain) RecordIngest(ctx context.Context, sourceID, path, hash, status string) error {
	_, err := b.db.Exec(`INSERT OR IGNORE INTO ingest_log (source_id, path, content_hash, status) VALUES (?, ?, ?, ?)`,
		sourceID, path, hash, status)
	return err
}

// Stats returns brain statistics.
type Stats struct {
	Pages     int `json:"pages"`
	Entities  int `json:"entities"`
	Edges     int `json:"edges"`
	Sources   int `json:"sources"`
	Embeddings int `json:"embeddings"`
}

// Stats returns current brain statistics.
func (b *Brain) Stats(ctx context.Context) (*Stats, error) {
	s := &Stats{}
	b.db.QueryRow(`SELECT COUNT(*) FROM pages`).Scan(&s.Pages)
	b.db.QueryRow(`SELECT COUNT(*) FROM entities`).Scan(&s.Entities)
	b.db.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&s.Edges)
	b.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&s.Sources)
	b.db.QueryRow(`SELECT COUNT(*) FROM embeddings`).Scan(&s.Embeddings)
	return s, nil
}

// contentHash computes SHA-256 of content for dedup.
func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h[:])
}

// slugify converts text to a URL-safe slug.
func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "/", "-")
	// Remove non-alphanumeric chars except hyphens
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	result := b.String()
	// Collapse multiple hyphens
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	return strings.Trim(result, "-")
}

// backfillEmbedding computes and stores an embedding for a page.
func (b *Brain) backfillEmbedding(pageID int64, content string) {
	ctx := context.Background()
	vec, err := b.embedder.Embed(ctx, content)
	if err != nil || len(vec) == 0 {
		return
	}
	blob := vectorToBlob(vec)
	now := time.Now().UTC().Format(time.RFC3339)
	b.db.Exec(`INSERT INTO embeddings (page_id, model, vector, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(page_id) DO UPDATE SET model = excluded.model, vector = excluded.vector, updated_at = excluded.updated_at`,
		pageID, b.embedder.ModelName(), blob, now)
}
