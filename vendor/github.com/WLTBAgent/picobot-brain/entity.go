package brain

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Entity extraction patterns — work without LLM calls.
var (
	// [[Wikilinks]] — explicit entity references
	wikilinkRe = regexp.MustCompile(`\[\[([^\]]+)\]\]`)

	// @mentions — person references
	mentionRe = regexp.MustCompile(`@(\w{2,30})`)

	// Email addresses — person references
	emailRe = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

	// "works at X", "CEO of X", "founded X" — relationship patterns
	worksAtRe  = regexp.MustCompile(`(?i)(?:works?\s+at|employed\s+by|joined)\s+([A-Z][A-Za-z0-9&\s]{1,40})`)
	ceoOfRe    = regexp.MustCompile(`(?i)(?:CEO|CTO|CFO|COO|founder|co-founder)\s+(?:of\s+)?([A-Z][A-Za-z0-9&\s]{1,40})`)
	investedRe = regexp.MustCompile(`(?i)(?:invested\s+in|funded|backed)\s+([A-Z][A-Za-z0-9&\s]{1,40})`)
	attendedRe = regexp.MustCompile(`(?i)(?:attended|present\s+at|spoke\s+at)\s+([A-Z][A-Za-z0-9&\s]{1,50})`)

	// Markdown structured fields: "- **Key**: Value" patterns
	// Matches lines like: - **Name**: Josh  or  - **Company**: Acme Corp
	markdownFieldRe = regexp.MustCompile(`(?im)^\s*[\-\*]\s+\*\*(?:name|company|organization|project|channel|role|title|location|city|country|employer|school|university|team|group|department|product|brand|app|tool|framework|language|platform|agent\s+identity)\*\*:\s*(.+)$`)

	// Section headers as concept entities: "## Project X" or "### Tool Y"
	sectionHeaderRe = regexp.MustCompile(`^#{2,3}\s+([A-Z][A-Za-z0-9\s&\-]{2,50})$`)

	// Bold references in running text: **ThingName**
	// NOTE: We filter out patterns followed by ":" in the extraction logic below
	boldRefRe = regexp.MustCompile(`\*\*([A-Z][A-Za-z0-9&]{2,40})\*\*`)

	// Parenthetical descriptions: Name (Type) — like "WLTechBlog (YouTube)"
	parenTypeRe = regexp.MustCompile(`([A-Z][A-Za-z0-9\s]{2,30})\s*\((YouTube|GitHub|Twitter|Discord|Telegram|Google|Apple|Microsoft|Amazon|AWS|Linux|Debian|Ubuntu|Go|Python|Rust|JavaScript|TypeScript)\)`)

	// Common words that are NOT entities — structural labels, generic terms
	entityStoplist = map[string]bool{
		// Markdown field keys
		"name": true, "channel": true, "role": true, "project": true,
		"company": true, "organization": true, "title": true, "location": true,
		"team": true, "group": true, "department": true, "product": true,
		"brand": true, "app": true, "tool": true, "framework": true,
		"language": true, "platform": true, "employer": true, "school": true,
		"university": true, "city": true, "country": true,
		// Structural/descriptive labels
		"bug": true, "root cause": true, "fix applied": true, "key facts": true,
		"the org": true, "user profile": true, "work context": true,
		"topics of interest": true, "preferences": true,
		// Generic descriptors from commit notes
		"startup registration": true, "presence heartbeat": true,
		"quiet heartbeat": true, "broadcast message detection": true,
		"llm call hangs": true, "no model fallback": true, "no retry logic": true,
		"sqlite read lock leak": true, "telegram http timeout with retry": true,
		"auto-create source in ingestpage": true,
	}
)

// ExtractEntities scans a page for entity references and creates entities/edges.
// This is the zero-LLM extraction path.
func (b *Brain) ExtractEntities(ctx context.Context, pageID int64) (int, error) {
	page, err := b.GetPageByID(ctx, pageID)
	if err != nil {
		return 0, err
	}

	created := 0
	content := page.Content

	// Extract [[wikilinks]]
	wikiMatches := wikilinkRe.FindAllStringSubmatch(content, -1)
	for _, m := range wikiMatches {
		name := strings.TrimSpace(m[1])
		entityType := inferEntityType(name, content)
		slug := "entities/" + slugify(name)
		if _, err := b.ensureEntity(ctx, page.SourceID, name, entityType, slug, &pageID); err == nil {
			created++
		}
	}

	// Extract @mentions as person entities
	mentionMatches := mentionRe.FindAllStringSubmatch(content, -1)
	for _, m := range mentionMatches {
		name := m[1]
		slug := "people/" + strings.ToLower(name)
		if _, err := b.ensureEntity(ctx, page.SourceID, name, "person", slug, &pageID); err == nil {
			created++
		}
	}

	// Extract relationship edges
	created += b.extractRelationships(ctx, page.SourceID, pageID, content, worksAtRe, "works_at")
	created += b.extractRelationships(ctx, page.SourceID, pageID, content, ceoOfRe, "leads")
	created += b.extractRelationships(ctx, page.SourceID, pageID, content, investedRe, "invested_in")
	created += b.extractRelationships(ctx, page.SourceID, pageID, content, attendedRe, "attended")

	// Extract structured markdown fields: - **Key**: Value
	fieldMatches := markdownFieldRe.FindAllStringSubmatch(content, -1)
	// Extract the field key from each match for type inference
	fieldKeyRe := regexp.MustCompile(`(?i)\*\*(\w[\w\s]*?)\*\*:\s*`)
	for _, m := range fieldMatches {
		value := strings.TrimSpace(m[1])
		// Strip everything after em-dash, en-dash, or " — " separator
		if idx := strings.Index(value, " — "); idx > 0 {
			value = strings.TrimSpace(value[:idx])
		}
		if idx := strings.Index(value, " – "); idx > 0 {
			value = strings.TrimSpace(value[:idx])
		}
		// Remove trailing punctuation
		value = strings.TrimRight(value, ".,;")
		if len(value) < 1 || len(value) > 60 {
			continue
		}
		// Skip stoplisted values
		if entityStoplist[strings.ToLower(value)] {
			continue
		}
		// Use field key to infer type
		entityType := inferEntityType(value, content)
		// Determine the field key for better type inference
		keyMatch := fieldKeyRe.FindStringSubmatch(m[0])
		if len(keyMatch) > 1 {
			fieldKey := strings.ToLower(strings.TrimSpace(keyMatch[1]))
			switch fieldKey {
			case "name":
				entityType = "person"
			case "channel", "platform":
				entityType = "channel"
			case "company", "organization", "employer":
				entityType = "company"
			case "project":
				entityType = "project"
			case "role", "title":
				entityType = "role"
			case "location", "city", "country":
				entityType = "place"
			case "school", "university":
				entityType = "organization"
			case "language", "framework", "tool", "app", "product", "brand":
				entityType = "technology"
			case "agent identity":
				entityType = "agent"
			}
		}
		slug := slugify(entityType) + "/" + slugify(value)
		if _, err := b.ensureEntity(ctx, page.SourceID, value, entityType, slug, &pageID); err == nil {
			created++
		}
	}

	// Extract parenthetical typed entities: Name (Type)
	parenMatches := parenTypeRe.FindAllStringSubmatch(content, -1)
	for _, m := range parenMatches {
		name := strings.TrimSpace(m[1])
		parentType := strings.ToLower(m[2])
		entityType := "concept"
		switch parentType {
		case "youtube", "github", "twitter", "discord", "telegram":
			entityType = "channel"
		case "google", "apple", "microsoft", "amazon", "aws":
			entityType = "company"
		case "linux", "debian", "ubuntu":
			entityType = "platform"
		case "go", "python", "rust", "javascript", "typescript":
			entityType = "language"
		}
		slug := slugify(entityType) + "/" + slugify(name)
		if _, err := b.ensureEntity(ctx, page.SourceID, name, entityType, slug, &pageID); err == nil {
			created++
		}
	}

	// Extract bold references (only unique, significant ones)
	// Skip: stoplisted terms, markdown field keys (**Name**:), and already-extracted field values
	boldMatches := boldRefRe.FindAllStringSubmatchIndex(content, -1)
	fieldValues := map[string]bool{}
	for _, m := range markdownFieldRe.FindAllStringSubmatch(content, -1) {
		val := strings.TrimSpace(m[1])
		if idx := strings.Index(val, " — "); idx > 0 {
			val = strings.TrimSpace(val[:idx])
		}
		if idx := strings.Index(val, " – "); idx > 0 {
			val = strings.TrimSpace(val[:idx])
		}
		fieldValues[val] = true
	}
	for _, loc := range boldMatches {
		name := strings.TrimSpace(content[loc[2]:loc[3]])
		// Skip if this is a markdown field key (followed by ":")
		afterIdx := loc[1]
		if afterIdx < len(content) && content[afterIdx] == ':' {
			continue
		}
		if fieldValues[name] || entityStoplist[strings.ToLower(name)] {
			continue
		}
		if len(name) < 3 {
			continue
		}
		entityType := inferEntityType(name, content)
		slug := slugify(entityType) + "/" + slugify(name)
		if _, err := b.ensureEntity(ctx, page.SourceID, name, entityType, slug, &pageID); err == nil {
			created++
		}
	}

	return created, nil
}

// extractRelationships finds relationship patterns and creates entities + edges.
func (b *Brain) extractRelationships(ctx context.Context, sourceID string, pageID int64, content string, re *regexp.Regexp, edgeType string) int {
	created := 0
	matches := re.FindAllStringSubmatch(content, -1)
	for _, m := range matches {
		targetName := strings.TrimSpace(m[1])
		targetType := "company"
		if edgeType == "attended" {
			targetType = "event"
		}
		targetSlug := slugify(targetType) + "/" + slugify(targetName)

		targetID, err := b.ensureEntity(ctx, sourceID, targetName, targetType, targetSlug, nil)
		if err != nil {
			continue
		}

		// Create edge (source entity is the page's primary entity if it's a person page)
		// For now, we create a page-level edge
		_, err = b.db.Exec(`
			INSERT OR IGNORE INTO edges (from_id, to_id, type, source_page, confidence)
			SELECT ?, ?, ?, ?, 0.7`,
			pageID, targetID, edgeType, pageID)
		if err == nil {
			created++
		}
	}
	return created
}

// ensureEntity creates an entity if it doesn't exist, returns its ID.
func (b *Brain) ensureEntity(ctx context.Context, sourceID, name, entityType, slug string, pageID *int64) (int64, error) {
	// Try to find existing entity by slug
	var id int64
	err := b.db.QueryRow(`SELECT id FROM entities WHERE slug = ?`, slug).Scan(&id)
	if err == nil {
		return id, nil
	}

	// Create new entity
	var pid *int64
	if pageID != nil {
		pid = pageID
	} else {
		// Can't use nil directly in Scan-compatible way, use a workaround
	}

	res, err := b.db.Exec(`
		INSERT OR IGNORE INTO entities (source_id, name, type, slug, page_id)
		VALUES (?, ?, ?, ?, ?)`,
		sourceID, name, entityType, slug, pid)
	if err != nil {
		return 0, err
	}

	id, _ = res.LastInsertId()
	if id == 0 {
		b.db.QueryRow(`SELECT id FROM entities WHERE slug = ?`, slug).Scan(&id)
	}
	return id, nil
}

// GraphNeighbors returns entities connected to the given entity through edges.
func (b *Brain) GraphNeighbors(ctx context.Context, entityID int64, depth int) ([]Entity, []Edge, error) {
	if depth <= 0 {
		depth = 1
	}
	if depth > 3 {
		depth = 3 // prevent runaway traversal
	}

	visited := map[int64]bool{entityID: true}
	var allEntities []Entity
	var allEdges []Edge

	current := []int64{entityID}
	for d := 0; d < depth; d++ {
		var nextBatch []int64
		for _, eid := range current {
			// Find outgoing edges
			rows, err := b.db.Query(`
				SELECT e.id, e.from_id, e.to_id, e.type, e.source_page, e.confidence
				FROM edges e WHERE e.from_id = ?`, eid)
			if err != nil {
				continue
			}
			for rows.Next() {
				var edge Edge
				if rows.Scan(&edge.ID, &edge.FromID, &edge.ToID, &edge.Type, &edge.SourcePage, &edge.Confidence) != nil {
					continue
				}
				allEdges = append(allEdges, edge)
				if !visited[edge.ToID] {
					nextBatch = append(nextBatch, edge.ToID)
				}
			}
			rows.Close()

			// Find incoming edges
			rows, err = b.db.Query(`
				SELECT e.id, e.from_id, e.to_id, e.type, e.source_page, e.confidence
				FROM edges e WHERE e.to_id = ?`, eid)
			if err != nil {
				continue
			}
			for rows.Next() {
				var edge Edge
				if rows.Scan(&edge.ID, &edge.FromID, &edge.ToID, &edge.Type, &edge.SourcePage, &edge.Confidence) != nil {
					continue
				}
				allEdges = append(allEdges, edge)
				if !visited[edge.FromID] {
					nextBatch = append(nextBatch, edge.FromID)
				}
			}
			rows.Close()
		}

		// Load entity details for newly discovered entities
		for _, nid := range nextBatch {
			if visited[nid] {
				continue
			}
			visited[nid] = true
			entity, err := b.getEntityByID(ctx, nid)
			if err == nil {
				allEntities = append(allEntities, *entity)
			}
		}
		current = nextBatch
	}

	return allEntities, allEdges, nil
}

// FindEntities searches for entities by name or type.
func (b *Brain) FindEntities(ctx context.Context, query string, entityType string, limit int) ([]Entity, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT id, source_id, name, type, slug, page_id FROM entities WHERE 1=1`
	args := []any{}
	if query != "" {
		q += ` AND name LIKE ?`
		args = append(args, "%"+query+"%")
	}
	if entityType != "" {
		q += ` AND type = ?`
		args = append(args, entityType)
	}
	q += ` LIMIT ?`
	args = append(args, limit)

	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entities []Entity
	for rows.Next() {
		var e Entity
		if rows.Scan(&e.ID, &e.SourceID, &e.Name, &e.Type, &e.Slug, &e.PageID) == nil {
			entities = append(entities, e)
		}
	}
	return entities, rows.Err()
}

func (b *Brain) getEntityByID(ctx context.Context, id int64) (*Entity, error) {
	var e Entity
	err := b.db.QueryRow(`SELECT id, source_id, name, type, slug, page_id FROM entities WHERE id = ?`, id).
		Scan(&e.ID, &e.SourceID, &e.Name, &e.Type, &e.Slug, &e.PageID)
	if err != nil {
		return nil, fmt.Errorf("entity not found: %w", err)
	}
	return &e, nil
}

// inferEntityType guesses entity type from name context.
func inferEntityType(name, content string) string {
	nameLower := strings.ToLower(name)
	// Check name first — the entity itself tells us the type
	switch {
	case strings.Contains(nameLower, "bot") || strings.Contains(nameLower, "agent"):
		return "agent"
	case strings.Contains(nameLower, "youtube") || strings.Contains(nameLower, "channel") ||
		strings.Contains(nameLower, "blog") || strings.Contains(nameLower, "video"):
		return "channel"
	case strings.Contains(nameLower, "corp") || strings.Contains(nameLower, "inc") ||
		strings.Contains(nameLower, "llc") || strings.Contains(nameLower, "ltd"):
		return "company"
	}

	// For short names (likely proper nouns), use content context
	if len(name) <= 30 {
		contentLower := strings.ToLower(content)
		// Only look for patterns near the entity name in content
		// Check if the name appears in a markdown field that hints at type
		namePattern := "(?i)-\\s*\\*\\*.*\\*\\*:\\s*" + regexp.QuoteMeta(name)
		if matched, _ := regexp.MatchString(namePattern, content); matched {
			// Look at which field key it's under
			fieldTypeRe := regexp.MustCompile(`(?i)\*\*(name|employer|school|university)\*\*:\s*` + regexp.QuoteMeta(name))
			if fieldTypeRe.MatchString(content) {
				return "person"
			}
			fieldTypeRe2 := regexp.MustCompile(`(?i)\*\*(channel|platform)\*\*:\s*` + regexp.QuoteMeta(name))
			if fieldTypeRe2.MatchString(content) {
				return "channel"
			}
		}

		// Fallback: check if name is surrounded by person-like context
		personHints := []string{"he ", "she ", "his ", "her ", "mr.", "mrs.", "dr."}
		for _, hint := range personHints {
			if strings.Contains(contentLower, strings.ToLower(name)+" "+hint) ||
				strings.Contains(contentLower, hint+strings.ToLower(name)) {
				return "person"
			}
		}
	}

	// Default
	return "concept"
}
