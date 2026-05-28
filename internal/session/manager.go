package session

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
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

func (sm *SessionManager) GetOrCreate(key string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if s, ok := sm.sessions[key]; ok {
		return s
	}
	s := &Session{Key: key, History: make([]string, 0)}
	sm.sessions[key] = s
	return s
}

func (sm *SessionManager) Save(s *Session) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s.trim()
	path := filepath.Join(sm.workspace, "sessions")
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	fpath := filepath.Join(path, s.Key+".json")
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
