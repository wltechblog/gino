package brain

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IngestDir walks a directory of markdown files and imports new/changed ones.
// Returns the count of newly imported pages.
func (b *Brain) IngestDir(ctx context.Context, sourceID, dirPath string) (int, error) {
	if sourceID == "" {
		sourceID = b.opts.DefaultSourceID
	}

	imported := 0
	err := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Only process markdown and text files
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".md" && ext != ".txt" && ext != ".markdown" {
			return nil
		}

		// Read file
		content, err := os.ReadFile(path)
		if err != nil {
			return nil // skip unreadable files
		}

		text := string(content)
		hash := contentHash(text)

		// Skip if already ingested with same hash
		already, _ := b.HasContentHash(ctx, sourceID, hash)
		if already {
			return nil
		}

		// Derive slug from relative path
		relPath, _ := filepath.Rel(dirPath, path)
		slug := pathToSlug(relPath)

		// Parse title from first heading or filename
		title := extractTitle(text, filepath.Base(path))

		// Detect type from path
		pageType := detectType(relPath)

		// Parse any YAML frontmatter
		metadata := parseFrontmatter(text)

		page := Page{
			SourceID: sourceID,
			Slug:     slug,
			Type:     pageType,
			Title:    title,
			Content:  text,
			Metadata: metadata,
		}

		_, err = b.IngestPage(ctx, page)
		if err != nil {
			return nil // skip pages that fail to ingest
		}

		if err := b.RecordIngest(ctx, sourceID, path, hash, "ok"); err != nil {
			log.Printf("brain: record ingest: %v", err)
		}
		imported++
		return nil
	})
	return imported, err
}

// IngestFile imports a single file into the brain.
func (b *Brain) IngestFile(ctx context.Context, sourceID, filePath string) (int64, error) {
	if sourceID == "" {
		sourceID = b.opts.DefaultSourceID
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return 0, fmt.Errorf("read file: %w", err)
	}

	text := string(content)
	title := extractTitle(text, filepath.Base(filePath))
	pageType := detectType(filePath)
	metadata := parseFrontmatter(text)

	page := Page{
		SourceID: sourceID,
		Slug:     pathToSlug(filepath.Base(filePath)),
		Type:     pageType,
		Title:    title,
		Content:  text,
		Metadata: metadata,
	}

	pageID, err := b.IngestPage(ctx, page)
	if err != nil {
		return 0, err
	}

	hash := contentHash(text)
	if err := b.RecordIngest(ctx, sourceID, filePath, hash, "ok"); err != nil {
		log.Printf("brain: record ingest: %v", err)
	}
	return pageID, nil
}

// ImportMemories imports Gino's existing memory directory into the brain.
// This is the "initial import" feature — scans ~/.gino/workspace/memory/
// and creates brain pages from daily notes and MEMORY.md.
func (b *Brain) ImportMemories(ctx context.Context, memoryDir string) (int, error) {
	// Create a "memories" source if it doesn't exist
	if err := b.AddSource(ctx, "memories", "Gino Memories", memoryDir); err != nil {
		log.Printf("brain: add source: %v", err)
	}

	imported, err := b.IngestDir(ctx, "memories", memoryDir)
	if err != nil {
		return imported, err
	}

	// Also import MEMORY.md as a special long-term page
	longTerm := filepath.Join(memoryDir, "MEMORY.md")
	if data, err := os.ReadFile(longTerm); err == nil {
		text := string(data)
		hash := contentHash(text)
		already, _ := b.HasContentHash(ctx, "memories", hash)
		if !already {
			page := Page{
				SourceID: "memories",
				Slug:     "long-term-memory",
				Type:     "longterm",
				Title:    "Long-Term Memory (MEMORY.md)",
				Content:  text,
				Metadata: map[string]string{"source": "MEMORY.md"},
			}
			if _, err := b.IngestPage(ctx, page); err != nil {
				log.Printf("brain: ingest long-term memory: %v", err)
			} else {
				if err := b.RecordIngest(ctx, "memories", longTerm, hash, "ok"); err != nil {
					log.Printf("brain: record ingest: %v", err)
				}
				imported++
			}
		}
	}

	return imported, nil
}

// Maintain runs the maintenance cycle: backfill embeddings, extract entities, prune orphans.
func (b *Brain) Maintain(ctx context.Context) (*MaintainReport, error) {
	start := time.Now()
	report := &MaintainReport{}

	// Phase 1: Backfill missing embeddings.
	// Drain the rows fully and Close() them BEFORE any write: sqliteRows holds
	// the store RLock until Close, so calling db.Exec (write lock) with the
	// rows still open deadlocks.
	if b.embedder != nil && b.opts.EmbeddingDims > 0 {
		type pendingPage struct {
			ID      int64
			Content string
		}
		var pending []pendingPage
		rows, err := b.db.Query(`
			SELECT p.id, p.content FROM pages p
			LEFT JOIN embeddings e ON e.page_id = p.id
			WHERE e.page_id IS NULL AND p.content != ''
			LIMIT 100`)
		if err == nil {
			for rows.Next() {
				var pp pendingPage
				if rows.Scan(&pp.ID, &pp.Content) == nil {
					pending = append(pending, pp)
				}
			}
			_ = rows.Close()

			// Batch embed (rows are closed; writes below are safe)
			if len(pending) > 0 {
				texts := make([]string, len(pending))
				for i, pp := range pending {
					texts[i] = pp.Content
				}
				vecs, err := b.embedder.EmbedBatch(ctx, texts)
				if err == nil {
					for i, vec := range vecs {
						if len(vec) > 0 {
							blob := vectorToBlob(vec)
							now := time.Now().UTC().Format(time.RFC3339)
							if _, err := b.db.Exec(`INSERT INTO embeddings (page_id, model, vector, updated_at) VALUES (?, ?, ?, ?)
								ON CONFLICT(page_id) DO UPDATE SET vector = excluded.vector, updated_at = excluded.updated_at`,
								pending[i].ID, b.embedder.ModelName(), blob, now); err != nil {
								log.Printf("brain: backfill embedding: %v", err)
							}
							report.EmbeddingsBackfilled++
						}
					}
				}
			}
		}
	}

	// Phase 2: Extract entities from unprocessed pages.
	// The page IDs must be fully drained and the rows Closed BEFORE calling
	// ExtractEntities: it writes via db.Exec (write lock) while the open rows
	// hold the RLock — executing writes with rows open deadlocks the store.
	pageIDs := []int64{}
	entityRows, err := b.db.Query(`
		SELECT p.id FROM pages p
		WHERE p.content != ''
		ORDER BY p.id`)
	if err == nil {
		for entityRows.Next() {
			var pid int64
			if entityRows.Scan(&pid) == nil {
				pageIDs = append(pageIDs, pid)
			}
		}
		_ = entityRows.Close()
	}

	for _, pid := range pageIDs {
		n, err := b.ExtractEntities(ctx, pid)
		if err == nil {
			report.EntitiesExtracted += n
		}
	}

	// Phase 3: Rebuild stale FTS entries
	rows, err := b.db.Query(`SELECT COUNT(*) FROM pages WHERE id NOT IN (SELECT rowid FROM pages_fts)`)
	if err == nil {
		if rows.Next() {
			_ = rows.Scan(&report.FTSRebuilt)
		}
		_ = rows.Close()
	}

	// Phase 4: Prune orphaned entities
	res, err := b.db.Exec(`DELETE FROM entities WHERE page_id IS NOT NULL AND page_id NOT IN (SELECT id FROM pages)`)
	if err == nil && res != nil {
		affected, _ := res.RowsAffected()
		report.OrphansPruned += int(affected)
	}
	res, err = b.db.Exec(`DELETE FROM edges WHERE source_page NOT IN (SELECT id FROM pages)`)
	if err == nil && res != nil {
		affected, _ := res.RowsAffected()
		report.OrphansPruned += int(affected)
	}

	report.ElapsedMs = int(time.Since(start).Milliseconds())
	return report, nil
}

// --- Helper functions ---

func pathToSlug(relPath string) string {
	s := strings.TrimSuffix(relPath, filepath.Ext(relPath))
	s = strings.ReplaceAll(s, string(filepath.Separator), "/")
	s = strings.ToLower(s)
	return strings.ReplaceAll(s, " ", "-")
}

func extractTitle(content, filename string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimPrefix(line, "# ")
		}
	}
	// Fall back to filename without extension
	name := filename
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")
	return name
}

func detectType(relPath string) string {
	relPath = strings.ToLower(relPath)
	parts := strings.Split(relPath, string(filepath.Separator))
	for _, part := range parts {
		switch part {
		case "people":
			return "person"
		case "companies":
			return "company"
		case "conversations", "chats":
			return "conversation"
		case "concepts", "ideas":
			return "concept"
		case "meetings":
			return "meeting"
		}
	}
	// Check if it looks like a date file (YYYY-MM-DD.md)
	if len(parts) > 0 {
		base := parts[len(parts)-1]
		if len(base) >= 10 {
			_, err := time.Parse("2006-01-02", base[:10])
			if err == nil {
				return "daily"
			}
		}
	}
	return "note"
}

func parseFrontmatter(content string) map[string]string {
	metadata := map[string]string{}
	if !strings.HasPrefix(content, "---") {
		return metadata
	}
	end := strings.Index(content[3:], "---")
	if end < 0 {
		return metadata
	}
	fm := content[3 : end+3]
	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, "\"'")
			metadata[key] = val
		}
	}
	return metadata
}


