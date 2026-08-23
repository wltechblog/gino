package session

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxHistorySize is the maximum number of messages kept in a session.
const MaxHistorySize = 50

// Session holds a short chat history.
type Session struct {
	Key       string    `json:"key"`
	Title     string    `json:"title,omitempty"`
	History   []string  `json:"history"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// SessionManager stores sessions in memory and persists to disk under workspace.
type SessionManager struct {
	mu        sync.RWMutex
	sessions  map[string]*Session
	workspace string
}

func NewSessionManager(workspace string) *SessionManager {
	return &SessionManager{sessions: make(map[string]*Session), workspace: workspace}
}

// sanitizeKey replaces path-unsafe characters in a session key.
// This prevents path traversal (e.g., "../../etc/passwd") when the key
// is used to construct a file path.
func sanitizeKey(key string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", "..", "_", string(os.PathSeparator), "_")
	return replacer.Replace(key)
}

func (sm *SessionManager) GetOrCreate(key string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	key = sanitizeKey(key)
	if s, ok := sm.sessions[key]; ok {
		return s
	}
	s := &Session{Key: key, History: make([]string, 0)}
	sm.sessions[key] = s
	return s
}

// DeleteSession removes a session from memory and deletes its file from disk.
func (sm *SessionManager) DeleteSession(key string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	key = sanitizeKey(key)
	delete(sm.sessions, key)
	path := filepath.Join(sm.workspace, "sessions", key+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("Session: delete file %s: %v", path, err)
	}
}

// DeleteAllExcept removes all sessions from memory and disk except the one
// matching keepKey. Active turns for deleted sessions should be cancelled by
// the caller before invoking this. Returns the number of sessions deleted.
func (sm *SessionManager) DeleteAllExcept(keepKey string) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	keepKey = sanitizeKey(keepKey)

	// 1. Remove from in-memory map.
	deleted := 0
	for key := range sm.sessions {
		if key != keepKey {
			delete(sm.sessions, key)
			deleted++
		}
	}

	// 2. Scan the sessions directory on disk and delete every file whose
	//    name does not match keepKey. This catches both .json and
	//    .active.json checkpoint files, including ones that were never
	//    loaded into memory.
	dir := filepath.Join(sm.workspace, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return deleted
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Keep both the session JSON and its checkpoint file.
		if name == keepKey+".json" || name == keepKey+".active.json" {
			continue
		}
		// Only touch .json and .active.json files — leave anything else alone.
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".active.json") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			log.Printf("Session: delete file %s: %v", name, err)
		} else {
			// Count disk files too, but avoid double-counting pairs.
			if !strings.HasSuffix(name, ".active.json") {
				deleted++
			}
		}
	}
	return deleted
}

// DeleteByPrefix removes all sessions whose key starts with the given prefix.
// It scans both the in-memory map and the on-disk sessions directory so that
// files from previous runs (or orphaned checkpoint files) are also cleaned up.
// Returns the number of files deleted.
func (sm *SessionManager) DeleteByPrefix(prefix string) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	prefix = sanitizeKey(prefix)

	// 1. Remove from in-memory map.
	for key := range sm.sessions {
		if strings.HasPrefix(key, prefix) {
			delete(sm.sessions, key)
		}
	}

	// 2. Scan the sessions directory on disk and delete every file whose
	//    sanitized name starts with the prefix. This catches both .json and
	//    .active.json checkpoint files, including ones that were never loaded
	//    into memory.
	dir := filepath.Join(sm.workspace, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var deleted int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, prefix) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				log.Printf("Session: delete file %s: %v", name, err)
			} else {
				deleted++
			}
		}
	}
	return deleted
}

// Save persists the session to disk.
func (sm *SessionManager) Save(s *Session) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s.trim()
	dir := filepath.Join(sm.workspace, "sessions")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	safeKey := sanitizeKey(s.Key)
	fpath := filepath.Join(dir, safeKey+".json")
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(fpath, b, 0644)
}

func (sm *SessionManager) LoadAll() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	path := filepath.Join(sm.workspace, "sessions")
	_ = os.MkdirAll(path, 0755)
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip active turn checkpoint files and temp files
		if len(name) > len(".active.json") && name[len(name)-len(".active.json"):] == ".active.json" {
			continue
		}
		if len(name) > len(".tmp") && name[len(name)-len(".tmp"):] == ".tmp" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			continue
		}
		var s Session
		if err := json.Unmarshal(b, &s); err != nil {
			continue
		}
		sm.sessions[s.Key] = &s
	}
	if len(sm.sessions) > 0 {
		log.Printf("Session: loaded %d sessions from disk", len(sm.sessions))
	}
	return nil
}

func (s *Session) AddMessage(role, content string) {
	s.History = append(s.History, role+": "+content)
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	s.UpdatedAt = time.Now()
}

// SetTitle updates the session title and persists the change.
func (sm *SessionManager) SetTitle(key, title string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	key = sanitizeKey(key)
	if s, ok := sm.sessions[key]; ok {
		s.Title = title
		s.UpdatedAt = time.Now()
	}
}

// ListByPrefix returns all sessions whose key starts with prefix.
// Results are sorted by UpdatedAt (most recent first).
func (sm *SessionManager) ListByPrefix(prefix string) []*Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	prefix = sanitizeKey(prefix)
	var result []*Session
	for _, s := range sm.sessions {
		if strings.HasPrefix(s.Key, prefix) {
			result = append(result, s)
		}
	}
	// Sort by UpdatedAt descending (most recent first)
	sort.Slice(result, func(i, j int) bool {
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result
}

// Get returns a session by key without creating it. Returns nil if not found.
func (sm *SessionManager) Get(key string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	key = sanitizeKey(key)
	return sm.sessions[key]
}



// PurgeOlderThan removes all sessions (including archives) whose UpdatedAt is
// older than the specified number of days. Sessions whose UpdatedAt is zero
// (legacy sessions without timestamps) are also purged. The active session
// key is always preserved. Returns the number of sessions deleted.
func (sm *SessionManager) PurgeOlderThan(days int, excludeKey string) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cutoff := time.Now().AddDate(0, 0, -days)
	excludeKey = sanitizeKey(excludeKey)

	deleted := 0
	for key, s := range sm.sessions {
		if key == excludeKey {
			continue
		}
		if s.UpdatedAt.IsZero() || s.UpdatedAt.Before(cutoff) {
			delete(sm.sessions, key)
			path := filepath.Join(sm.workspace, "sessions", key+".json")
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				log.Printf("Session: purge file %s: %v", path, err)
			}
			// Also remove checkpoint file if present (best-effort; may not exist).
			cpPath := filepath.Join(sm.workspace, "sessions", key+".active.json")
			_ = os.Remove(cpPath)
			deleted++
		}
	}
	return deleted
}

// SearchResult is a single match from SearchSessions.
type SearchResult struct {
	SessionKey string
	Title      string
	Snippet    string // matching text excerpt
	MessageN   int
	UpdatedAt  time.Time
}

// SearchSessions searches all sessions matching the given prefix for the query
// string. It searches both session titles and message history (case-insensitive).
// Returns matches sorted by UpdatedAt (most recent first).
func (sm *SessionManager) SearchSessions(prefix, query string) []SearchResult {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	prefix = sanitizeKey(prefix)
	lowerQuery := strings.ToLower(query)

	var results []SearchResult
	for _, s := range sm.sessions {
		if !strings.HasPrefix(s.Key, prefix) {
			continue
		}
		// Search title first.
		matched := false
		snippet := ""
		if s.Title != "" && strings.Contains(strings.ToLower(s.Title), lowerQuery) {
			matched = true
			snippet = s.Title
		}
		// Search history.
		if !matched {
			for _, entry := range s.History {
				if strings.Contains(strings.ToLower(entry), lowerQuery) {
					matched = true
					snippet = extractSnippet(entry, lowerQuery, 80)
					break
				}
			}
		}
		if matched {
			results = append(results, SearchResult{
				SessionKey: s.Key,
				Title:      s.Title,
				Snippet:    snippet,
				MessageN:   len(s.History),
				UpdatedAt:  s.UpdatedAt,
			})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].UpdatedAt.After(results[j].UpdatedAt)
	})
	return results
}

// extractSnippet pulls a short excerpt around the first match of query in text.
func extractSnippet(text, query string, radius int) string {
	lowerText := strings.ToLower(text)
	idx := strings.Index(lowerText, strings.ToLower(query))
	if idx < 0 {
		return text
	}
	start := idx - radius
	if start < 0 {
		start = 0
	}
	end := idx + len(query) + radius
	if end > len(text) {
		end = len(text)
	}
	snippet := text[start:end]
	if start > 0 {
		snippet = "..." + snippet
	}
	if end < len(text) {
		snippet = snippet + "..."
	}
	return snippet
}

func (s *Session) GetHistory() []string {
	return s.History
}

func (s *Session) trim() {
	if len(s.History) > MaxHistorySize {
		s.History = s.History[len(s.History)-MaxHistorySize:]
	}
}
