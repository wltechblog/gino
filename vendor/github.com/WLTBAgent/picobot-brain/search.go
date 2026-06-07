package brain

import (
	"context"
	"encoding/binary"
	"math"
	"sort"
	"strings"
	"unicode"
)

// rankedResult is an internal intermediate result used by the search pipeline.
// Not exported — consumers use SearchResult.
type rankedResult struct {
	PageID int64
	Slug   string
	Title  string
	Score  float64
	Source string
	Type   string
}

// sanitizeFTSQuery cleans a raw query string for safe use with SQLite FTS5 MATCH.
// FTS5 has special syntax for ?, !, ", *, (, ), AND, OR, NOT, etc.
// We strip all special characters and return a clean token-based query.
func sanitizeFTSQuery(query string) string {
	// Remove characters that are special in FTS5 syntax
	special := "?!\")(*:^+-~{}|"
	for _, ch := range special {
		query = strings.ReplaceAll(query, string(ch), " ")
	}

	// Split into tokens, filter empty
	fields := strings.Fields(query)

	// Filter out FTS5 operators that could cause issues
	ftsOperators := map[string]bool{
		"AND": true, "OR": true, "NOT": true,
		"NEAR": true, "COLUMN": true,
	}

	var clean []string
	for _, f := range fields {
		if ftsOperators[strings.ToUpper(f)] {
			continue
		}
		// Skip tokens that are purely non-alphanumeric
		if !hasLetterOrDigit(f) {
			continue
		}
		clean = append(clean, f)
	}

	if len(clean) == 0 {
		return ""
	}

	return strings.Join(clean, " OR ")
}

func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// Search performs hybrid search combining FTS5 keyword search and vector similarity,
// merged via Reciprocal Rank Fusion (RRF).
func (b *Brain) Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	if opts.Limit <= 0 {
		opts.Limit = 10
	}

	var ftsResults []rankedResult
	var vecResults []rankedResult
	var err error

	// FTS5 keyword search
	if !opts.VectorOnly {
		ftsResults, err = b.searchFTS(ctx, query, opts)
		if err != nil {
			return nil, err
		}
	}

	// Vector similarity search
	if !opts.KeywordOnly && b.embedder != nil && b.opts.EmbeddingDims > 0 {
		vecResults, err = b.searchVector(ctx, query, opts)
		if err != nil {
			// Vector search failure shouldn't block results — fall back to FTS only
			vecResults = nil
		}
	}

	// If only one method produced results, return those
	if len(ftsResults) == 0 && len(vecResults) == 0 {
		return []SearchResult{}, nil
	}
	if len(ftsResults) == 0 {
		return b.rankToResults(vecResults, opts.Limit), nil
	}
	if len(vecResults) == 0 {
		return b.rankToResults(ftsResults, opts.Limit), nil
	}

	// Reciprocal Rank Fusion
	merged := b.rrfFuse(ftsResults, vecResults)
	return b.rankToResults(merged, opts.Limit), nil
}

// searchFTS performs FTS5 full-text search with BM25 ranking.
func (b *Brain) searchFTS(ctx context.Context, query string, opts SearchOpts) ([]rankedResult, error) {
	// Sanitize query for FTS5
	cleanQuery := sanitizeFTSQuery(query)
	if cleanQuery == "" {
		return nil, nil
	}

	// Build query with source/type filters
	args := []any{cleanQuery, opts.Limit * 3} // overfetch for filtering

	rows, err := b.db.Query(`
		SELECT p.id, p.slug, p.title, p.source_id, p.type,
			bm25(pages_fts) AS score
		FROM pages_fts fts
		JOIN pages p ON p.id = fts.rowid
		WHERE pages_fts MATCH ?
		ORDER BY score DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []rankedResult
	for rows.Next() {
		var r rankedResult
		if err := rows.Scan(&r.PageID, &r.Slug, &r.Title, &r.Source, &r.Type, &r.Score); err != nil {
			continue
		}
		// Apply source filter
		if len(opts.Sources) > 0 && !contains(opts.Sources, r.Source) {
			continue
		}
		// Apply type filter
		if len(opts.Types) > 0 && !contains(opts.Types, r.Type) {
			continue
		}
		results = append(results, r)
	}

	// Trim to limit
	if len(results) > opts.Limit {
		results = results[:opts.Limit]
	}
	return results, rows.Err()
}

// searchVector performs brute-force KNN vector similarity search.
func (b *Brain) searchVector(ctx context.Context, query string, opts SearchOpts) ([]rankedResult, error) {
	if b.embedder == nil {
		return nil, nil
	}

	// Compute query embedding
	queryVec, err := b.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}

	// Load all stored vectors and compute cosine similarity
	// For large brains, this should be replaced with sqlite-vec or an HNSW index
	rows, err := b.db.Query(`
		SELECT e.page_id, e.vector, p.slug, p.title, p.source_id, p.type
		FROM embeddings e
		JOIN pages p ON p.id = e.page_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type vecEntry struct {
		PageID int64
		Slug   string
		Title  string
		Source string
		Type   string
		Score  float64
	}

	entries := make([]vecEntry, 0, 64)
	for rows.Next() {
		var pageID int64
		var vecBlob []byte
		var slug, title, source, ptype string
		if err := rows.Scan(&pageID, &vecBlob, &slug, &title, &source, &ptype); err != nil {
			continue
		}
		// Apply filters before computing similarity
		if len(opts.Sources) > 0 && !contains(opts.Sources, source) {
			continue
		}
		if len(opts.Types) > 0 && !contains(opts.Types, ptype) {
			continue
		}

		vec := blobToVector(vecBlob)
		score := cosineSimilarity(queryVec, vec)
		entries = append(entries, vecEntry{
			PageID: pageID, Slug: slug, Title: title,
			Source: source, Type: ptype, Score: score,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Sort by score descending
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Score > entries[j].Score
	})

	// Convert to rankedResult
	limit := opts.Limit * 3
	if limit > len(entries) {
		limit = len(entries)
	}
	results := make([]rankedResult, 0, limit)
	for i := 0; i < limit; i++ {
		results = append(results, rankedResult{
			PageID: entries[i].PageID,
			Slug:   entries[i].Slug,
			Title:  entries[i].Title,
			Score:  entries[i].Score,
			Source: entries[i].Source,
			Type:   entries[i].Type,
		})
	}
	return results, nil
}

// rrfFuse merges results from FTS and vector search using Reciprocal Rank Fusion.
// k=60 is the standard RRF constant.
func (b *Brain) rrfFuse(fts, vec []rankedResult) []rankedResult {
	const k = 60.0

	// Build score map: pageID -> accumulated RRF score
	scores := make(map[int64]float64)
	pageInfo := make(map[int64]rankedResult)

	// FTS ranks (rank 0 = best)
	for i, r := range fts {
		scores[r.PageID] += 1.0 / (k + float64(i) + 1)
		pageInfo[r.PageID] = r
	}

	// Vector ranks
	for i, r := range vec {
		scores[r.PageID] += 1.0 / (k + float64(i) + 1)
		if _, exists := pageInfo[r.PageID]; !exists {
			pageInfo[r.PageID] = r
		}
	}

	// Sort by fused score
	type fused struct {
		PageID int64
		Score  float64
	}
	fusedResults := make([]fused, 0, len(scores))
	for pid, score := range scores {
		fusedResults = append(fusedResults, fused{PageID: pid, Score: score})
	}
	sort.Slice(fusedResults, func(i, j int) bool {
		return fusedResults[i].Score > fusedResults[j].Score
	})

	// Build final results
	results := make([]rankedResult, 0, len(fusedResults))
	for _, f := range fusedResults {
		r := pageInfo[f.PageID]
		r.Score = f.Score
		results = append(results, r)
	}
	return results
}

func (b *Brain) rankToResults(ranked []rankedResult, limit int) []SearchResult {
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	results := make([]SearchResult, 0, len(ranked))
	for _, r := range ranked {
		// Get snippet from content
		snippet := ""
		page, err := b.GetPageByID(context.Background(), r.PageID)
		if err == nil && len(page.Content) > 200 {
			snippet = page.Content[:200] + "..."
		} else if err == nil {
			snippet = page.Content
		}
		results = append(results, SearchResult{
			PageID:  r.PageID,
			Slug:    r.Slug,
			Title:   r.Title,
			Snippet: snippet,
			Score:   r.Score,
			Source:  r.Source,
			Type:    r.Type,
		})
	}
	return results
}

// --- Vector utilities ---

func vectorToBlob(vec []float32) []byte {
	blob := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(v))
	}
	return blob
}

func blobToVector(blob []byte) []float32 {
	n := len(blob) / 4
	vec := make([]float32, n)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vec
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
