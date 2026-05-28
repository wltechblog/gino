package session

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxHistorySize is the maximum number of messages kept in a session.
// Older messages are trimmed on save to keep the session file small
// and avoid blowing up the LLM context window.
// Important information should be persisted via write_memory, not session history.
const MaxHistorySize = 50

// Session holds a short chat history.
type Session struct {
	Key     string
	History []string
}

// ActiveTurn represents an in-flight agent turn that was interrupted by a restart.
// It stores the full LLM message chain (including tool calls/results) so the
// agent can recover and continue processing instead of losing context.
type ActiveTurn struct {
	Key         string    `json:"key"`
	Channel     string    `json:"channel"`
	ChatID      string    `json:"chatID"`
	SenderID    string    `json:"senderID"`
	UserContent string    `json:"userContent"`
	Messages    []Message `json:"messages"`  // full LLM message chain
	Iteration   int       `json:"iteration"` // which iteration we were on
	SavedAt     time.Time `json:"savedAt"`
	Media       []string  `json:"media,omitempty"`
}

// Message mirrors providers.Message for JSON serialization.
// This avoids an import cycle between the session and providers packages.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall mirrors providers.ToolCall for JSON serialization.
type ToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
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
	// Trim history to the most recent messages
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
		// Skip active turn files
		name := e.Name()
		if len(name) > len(".active.json") && name[len(name)-len(".active.json"):] == ".active.json" {
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
	return nil
}

// SaveActive persists an in-flight turn to disk so it can be recovered after restart.
// The file is written atomically (write to .tmp then rename).
func (sm *SessionManager) SaveActive(turn *ActiveTurn) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	path := filepath.Join(sm.workspace, "sessions")
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}

	turn.SavedAt = time.Now().UTC()
	fpath := filepath.Join(path, turn.Key+".active.json")
	tmpPath := fpath + ".tmp"

	b, err := json.MarshalIndent(turn, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal active turn: %w", err)
	}

	if err := os.WriteFile(tmpPath, b, 0644); err != nil {
		return fmt.Errorf("write active turn tmp: %w", err)
	}

	if err := os.Rename(tmpPath, fpath); err != nil {
		return fmt.Errorf("rename active turn: %w", err)
	}

	log.Printf("Session: saved active turn for %s (iteration %d, %d messages)", turn.Key, turn.Iteration, len(turn.Messages))
	return nil
}

// LoadActive loads a previously saved in-flight turn for the given session key.
// Returns nil if no active turn exists.
func (sm *SessionManager) LoadActive(key string) (*ActiveTurn, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	fpath := filepath.Join(sm.workspace, "sessions", key+".active.json")
	b, err := os.ReadFile(fpath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read active turn: %w", err)
	}

	var turn ActiveTurn
	if err := json.Unmarshal(b, &turn); err != nil {
		return nil, fmt.Errorf("unmarshal active turn: %w", err)
	}

	log.Printf("Session: loaded active turn for %s (iteration %d, %d messages, saved %s)",
		turn.Key, turn.Iteration, len(turn.Messages), turn.SavedAt.Format(time.RFC3339))
	return &turn, nil
}

// ClearActive removes the active turn file after the turn completes successfully.
func (sm *SessionManager) ClearActive(key string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	fpath := filepath.Join(sm.workspace, "sessions", key+".active.json")
	err := os.Remove(fpath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove active turn: %w", err)
	}
	if err == nil {
		log.Printf("Session: cleared active turn for %s", key)
	}
	return nil
}

// LoadAllActive loads all active turns from disk. Used at startup to recover
// interrupted turns.
func (sm *SessionManager) LoadAllActive() []*ActiveTurn {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	path := filepath.Join(sm.workspace, "sessions")
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}

	var turns []*ActiveTurn
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < len(".active.json") || name[len(name)-len(".active.json"):] != ".active.json" {
			continue
		}

		b, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			continue
		}
		var turn ActiveTurn
		if err := json.Unmarshal(b, &turn); err != nil {
			log.Printf("Session: failed to parse active turn %s: %v", name, err)
			continue
		}
		turns = append(turns, &turn)
	}

	if len(turns) > 0 {
		log.Printf("Session: found %d interrupted turns to recover", len(turns))
	}
	return turns
}

func (s *Session) AddMessage(role, content string) {
	s.History = append(s.History, role+": "+content)
}

// GetHistory returns the session history.
func (s *Session) GetHistory() []string {
	return s.History
}

// trim keeps only the last MaxHistorySize messages, discarding the oldest.
func (s *Session) trim() {
	if len(s.History) > MaxHistorySize {
		s.History = s.History[len(s.History)-MaxHistorySize:]
	}
}
