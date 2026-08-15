package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/wltechblog/gino/internal/brain"
)

// ─── brain_search ────

// BrainSearchTool searches the knowledge brain for relevant information.
type BrainSearchTool struct {
	brain *brain.Brain
}

func NewBrainSearchTool(b *brain.Brain) *BrainSearchTool {
	return &BrainSearchTool{brain: b}
}

func (t *BrainSearchTool) Name() string { return "brain_search" }
func (t *BrainSearchTool) Description() string {
	return "Search the knowledge brain for relevant information using hybrid search (keyword + semantic). Use this to find information across all ingested notes, conversations, and documents."
}
func (t *BrainSearchTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "Search query — natural language or keywords",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of results (default 10)",
			},
		},
		"required": []string{"query"},
	}
}

func (t *BrainSearchTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	if t.brain == nil {
		return "", fmt.Errorf("brain is not initialized")
	}
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("brain_search: 'query' argument required")
	}
	limit := 10
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}

	results, err := t.brain.Search(ctx, query, brain.SearchOpts{Limit: limit})
	if err != nil {
		return "", fmt.Errorf("brain search failed: %w", err)
	}
	if len(results) == 0 {
		return "No results found in brain.", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d results:\n\n", len(results))
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. [%s] %s (score: %.2f, source: %s)\n", i+1, r.Type, r.Title, r.Score, r.Source)
		if r.Snippet != "" {
			// Indent snippet
			for _, line := range strings.Split(r.Snippet, "\n") {
				fmt.Fprintf(&sb, "   %s\n", line)
			}
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String()), nil
}

// ─── brain_ingest ────

// BrainIngestTool imports files or directories into the knowledge brain.
type BrainIngestTool struct {
	brain *brain.Brain
}

func NewBrainIngestTool(b *brain.Brain) *BrainIngestTool {
	return &BrainIngestTool{brain: b}
}

func (t *BrainIngestTool) Name() string { return "brain_ingest" }
func (t *BrainIngestTool) Description() string {
	return "Import a file or directory of markdown/text files into the knowledge brain. Imported content becomes searchable via brain_search."
}
func (t *BrainIngestTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File or directory path to import",
			},
			"source_id": map[string]interface{}{
				"type":        "string",
				"description": "Source identifier (default: 'default')",
			},
		},
		"required": []string{"path"},
	}
}

func (t *BrainIngestTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	if t.brain == nil {
		return "", fmt.Errorf("brain is not initialized")
	}
	path, _ := args["path"].(string)
	if path == "" {
		return "", fmt.Errorf("brain_ingest: 'path' argument required")
	}
	sourceID, _ := args["source_id"].(string)

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("path error: %w", err)
	}

	if info.IsDir() {
		n, err := t.brain.IngestDir(ctx, sourceID, path)
		if err != nil {
			return "", fmt.Errorf("ingest directory failed: %w", err)
		}
		return fmt.Sprintf("Imported %d new pages from %s", n, path), nil
	}

	_, err = t.brain.IngestFile(ctx, sourceID, path)
	if err != nil {
		return "", fmt.Errorf("ingest file failed: %w", err)
	}
	return fmt.Sprintf("Imported 1 page from %s", path), nil
}

// ─── brain_entity ────

// BrainEntityTool looks up entities and their relationships in the knowledge graph.
type BrainEntityTool struct {
	brain *brain.Brain
}

func NewBrainEntityTool(b *brain.Brain) *BrainEntityTool {
	return &BrainEntityTool{brain: b}
}

func (t *BrainEntityTool) Name() string { return "brain_entity" }
func (t *BrainEntityTool) Description() string {
	return "Look up an entity (person, company, concept) and its relationships in the knowledge graph. Use this to answer questions like 'who works at X?' or 'what is Person Y connected to?'"
}
func (t *BrainEntityTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"name": map[string]interface{}{
				"type":        "string",
				"description": "Entity name to search for",
			},
			"type": map[string]interface{}{
				"type":        "string",
				"description": "Entity type filter: person, company, concept, place",
			},
			"depth": map[string]interface{}{
				"type":        "integer",
				"description": "Graph traversal depth (default 1, max 3)",
			},
		},
		"required": []string{"name"},
	}
}

func (t *BrainEntityTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	if t.brain == nil {
		return "", fmt.Errorf("brain is not initialized")
	}
	name, _ := args["name"].(string)
	if name == "" {
		return "", fmt.Errorf("brain_entity: 'name' argument required")
	}
	eType, _ := args["type"].(string)
	depth := 1
	if d, ok := args["depth"].(float64); ok {
		depth = int(d)
	}

	entities, err := t.brain.FindEntities(ctx, name, eType, 5)
	if err != nil || len(entities) == 0 {
		return fmt.Sprintf("No entities found matching %q.", name), nil
	}

	var sb strings.Builder
	for _, e := range entities {
		fmt.Fprintf(&sb, "## %s (%s)\nSlug: %s\n", e.Name, e.Type, e.Slug)

		neighbors, edges, _ := t.brain.GraphNeighbors(ctx, e.ID, depth)
		if len(edges) > 0 {
			sb.WriteString("\nRelationships:\n")
			for _, edge := range edges {
				for _, n := range neighbors {
					if n.ID == edge.ToID {
						fmt.Fprintf(&sb, "  - %s → %s (%s)\n", e.Name, n.Name, edge.Type)
					} else if n.ID == edge.FromID {
						fmt.Fprintf(&sb, "  - %s → %s (%s)\n", n.Name, e.Name, edge.Type)
					}
				}
			}
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String()), nil
}

// ─── brain_status ────

// BrainStatusTool shows brain statistics.
type BrainStatusTool struct {
	brain *brain.Brain
}

func NewBrainStatusTool(b *brain.Brain) *BrainStatusTool {
	return &BrainStatusTool{brain: b}
}

func (t *BrainStatusTool) Name() string        { return "brain_status" }
func (t *BrainStatusTool) Description() string { return "Show knowledge brain statistics (pages, entities, embeddings, etc.)" }
func (t *BrainStatusTool) Parameters() map[string]interface{} { return nil }

func (t *BrainStatusTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	if t.brain == nil {
		return "Brain is not initialized.", nil
	}
	stats, err := t.brain.Stats(ctx)
	if err != nil {
		return "", fmt.Errorf("brain stats failed: %w", err)
	}
	return fmt.Sprintf("🧠 Brain: %d pages, %d entities, %d edges, %d embeddings, %d sources",
		stats.Pages, stats.Entities, stats.Edges, stats.Embeddings, stats.Sources), nil
}

// ─── brain_maintain ────

// BrainMaintainTool triggers brain maintenance.
type BrainMaintainTool struct {
	brain *brain.Brain
}

func NewBrainMaintainTool(b *brain.Brain) *BrainMaintainTool {
	return &BrainMaintainTool{brain: b}
}

func (t *BrainMaintainTool) Name() string        { return "brain_maintain" }
func (t *BrainMaintainTool) Description() string { return "Run brain maintenance: backfill missing embeddings, extract entities, prune orphaned data" }
func (t *BrainMaintainTool) Parameters() map[string]interface{} { return nil }

func (t *BrainMaintainTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	if t.brain == nil {
		return "Brain is not initialized.", nil
	}
	report, err := t.brain.Maintain(ctx)
	if err != nil {
		return "", fmt.Errorf("brain maintain failed: %w", err)
	}
	return fmt.Sprintf("🧠 Maintenance complete in %dms: %d embeddings backfilled, %d entities extracted, %d FTS rebuilt, %d orphans pruned",
		report.ElapsedMs, report.EmbeddingsBackfilled, report.EntitiesExtracted, report.FTSRebuilt, report.OrphansPruned), nil
}
