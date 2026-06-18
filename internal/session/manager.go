package session

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MaxHistorySize is the maximum number of messages kept in a session.
const MaxHistorySize = 50

// Session holds a short chat history.
type Session struct {
	Key     string   `json:"key"`
	History []string `json:"history"`
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
	os.Remove(path)
}

// DeleteByPrefix removes all sessions whose key starts with the given prefix.
// Returns the number of sessions deleted.
func (sm *SessionManager) DeleteByPrefix(prefix string) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	prefix = sanitizeKey(prefix)
	var deleted int
	for key := range sm.sessions {
		if strings.HasPrefix(key, prefix) {
			delete(sm.sessions, key)
			os.Remove(filepath.Join(sm.workspace, "sessions", key+".json"))
			deleted++
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
}

func (s *Session) GetHistory() []string {
	return s.History
}

func (s *Session) trim() {
	if len(s.History) > MaxHistorySize {
		s.History = s.History[len(s.History)-MaxHistorySize:]
	}
}
