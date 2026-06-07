package brain

// adapter.go — Picobot integration guide
//
// This file documents how to integrate picobot-brain into Picobot.
// The actual adapter code lives inside Picobot's codebase.
//
// === Integration Points ===
//
// 1. DEPENDENCY: Add to Picobot's go.mod:
//
//   require github.com/WLTBAgent/picobot-brain v0.1.0
//
// 2. INIT (in Picobot's gateway startup, cmd/picobot/main.go):
//
//   import brain "github.com/WLTBAgent/picobot-brain"
//
//   var brainInst *brain.Brain
//
//   func initBrain(homeDir string, cfg Config) error {
//       dbPath := filepath.Join(homeDir, "brain.db")
//
//       // Determine embedding provider
//       var embedder brain.EmbeddingProvider
//
//       // Try local Ollama first
//       resp, err := http.Get("http://localhost:11434/api/tags")
//       if err == nil && resp.StatusCode == 200 {
//           resp.Body.Close()
//           embedder = brain.NewOllamaProvider(brain.OllamaConfig{
//               Model: "nomic-embed-text",
//           })
//       } else if cfg.Providers.OpenAI.APIKey != "" {
//           // Fall back to remote API
//           embedder = brain.NewRemoteAPIProvider(brain.RemoteAPIConfig{
//               BaseURL: cfg.Providers.OpenAI.APIBase,
//               APIKey:  cfg.Providers.OpenAI.APIKey,
//               Model:   "text-embedding-3-small",
//           })
//       }
//       // If neither available, embedder is nil → FTS5-only mode
//
//       opts := brain.DefaultOptions()
//       brainInst, err = brain.Init(dbPath, embedder, opts)
//       if err != nil {
//           return err
//       }
//
//       // Auto-import existing memories on first run
//       stats, _ := brainInst.Stats(context.Background())
//       if stats.Pages == 0 {
//           memDir := filepath.Join(homeDir, "workspace", "memory")
//           if _, err := os.Stat(memDir); err == nil {
//               brainInst.ImportMemories(context.Background(), memDir)
//           }
//       }
//       return nil
//   }
//
// 3. TOOLS (add to Picobot's tool registry, internal/agent/tools/):
//
//   // brain_search tool
//   {
//       Name:        "brain_search",
//       Description: "Search the knowledge brain for relevant information",
//       Parameters: map[string]any{
//           "query": map[string]string{"type": "string", "description": "Search query"},
//           "limit": map[string]string{"type": "integer", "description": "Max results (default 10)"},
//       },
//       Handler: func(args map[string]any) (string, error) {
//           query, _ := args["query"].(string)
//           limit := 10
//           if l, ok := args["limit"].(float64); ok {
//               limit = int(l)
//           }
//           results, err := brainInst.Search(ctx, query, brain.SearchOpts{Limit: limit})
//           if err != nil {
//               return "", err
//           }
//           // Format results as text for LLM consumption
//           var b strings.Builder
//           for i, r := range results {
//               fmt.Fprintf(&b, "%d. [%s] %s (score: %.2f)\n%s\n\n",
//                   i+1, r.Type, r.Title, r.Score, r.Snippet)
//           }
//           return b.String(), nil
//       },
//   },
//
//   // brain_ingest tool
//   {
//       Name:        "brain_ingest",
//       Description: "Import a file or directory into the knowledge brain",
//       Parameters: map[string]any{
//           "path":       map[string]string{"type": "string", "description": "File or directory path"},
//           "source_id":  map[string]string{"type": "string", "description": "Source ID (default: 'default')"},
//       },
//       Handler: func(args map[string]any) (string, error) {
//           path, _ := args["path"].(string)
//           sourceID, _ := args["source_id"].(string)
//
//           info, err := os.Stat(path)
//           if err != nil {
//               return "", err
//           }
//           if info.IsDir() {
//               n, err := brainInst.IngestDir(ctx, sourceID, path)
//               return fmt.Sprintf("Imported %d pages", n), err
//           }
//           _, err = brainInst.IngestFile(ctx, sourceID, path)
//           return "Imported 1 page", err
//       },
//   },
//
//   // brain_entity tool
//   {
//       Name:        "brain_entity",
//       Description: "Look up an entity and its relationships in the knowledge graph",
//       Parameters: map[string]any{
//           "name":       map[string]string{"type": "string", "description": "Entity name to search"},
//           "type":       map[string]string{"type": "string", "description": "Entity type filter (person, company, concept)"},
//           "depth":      map[string]string{"type": "integer", "description": "Graph traversal depth (default 1, max 3)"},
//       },
//       Handler: func(args map[string]any) (string, error) {
//           name, _ := args["name"].(string)
//           eType, _ := args["type"].(string)
//           depth := 1
//           if d, ok := args["depth"].(float64); ok {
//               depth = int(d)
//           }
//           entities, err := brainInst.FindEntities(ctx, name, eType, 5)
//           if err != nil || len(entities) == 0 {
//               return "No entities found", nil
//           }
//           var b strings.Builder
//           for _, e := range entities {
//               fmt.Fprintf(&b, "## %s (%s)\nSlug: %s\n\n", e.Name, e.Type, e.Slug)
//               neighbors, edges, _ := brainInst.GraphNeighbors(ctx, e.ID, depth)
//               if len(edges) > 0 {
//                   b.WriteString("Relationships:\n")
//                   for _, edge := range edges {
//                       // Find the other entity name
//                       for _, n := range neighbors {
//                           if n.ID == edge.ToID || n.ID == edge.FromID {
//                               fmt.Fprintf(&b, "  - %s → %s (%s)\n", e.Name, n.Name, edge.Type)
//                           }
//                       }
//                   }
//               }
//               b.WriteString("\n")
//           }
//           return b.String(), nil
//       },
//   },
//
//   // brain_status tool
//   {
//       Name:        "brain_status",
//       Description: "Show brain statistics (pages, entities, embeddings)",
//       Handler: func(args map[string]any) (string, error) {
//           stats, err := brainInst.Stats(ctx)
//           if err != nil {
//               return "", err
//           }
//           return fmt.Sprintf("Brain: %d pages, %d entities, %d edges, %d embeddings",
//               stats.Pages, stats.Entities, stats.Edges, stats.Embeddings), nil
//       },
//   },
//
// 4. CONTEXT INJECTION (in internal/agent/context.go):
//
//   // Before building the LLM context, query the brain for relevant info:
//   if brainInst != nil {
//       results, _ := brainInst.Search(ctx, userMessage, brain.SearchOpts{Limit: 5})
//       if len(results) > 0 {
//           var brainCtx strings.Builder
//           brainCtx.WriteString("\n## Relevant Brain Context\n")
//           for _, r := range results {
//               brainCtx.WriteString(fmt.Sprintf("- [%s] %s: %s\n", r.Type, r.Title, r.Snippet))
//           }
//           systemPrompt += brainCtx.String()
//       }
//   }
//
// 5. HEARTBEAT INTEGRATION (optional, in internal/heartbeat/service.go):
//
//   // During heartbeat, run maintenance if not done recently
//   if brainInst != nil && time.Since(lastMaintain) > 24*time.Hour {
//       brainInst.Maintain(ctx)
//       lastMaintain = time.Now()
//   }
