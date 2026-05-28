package agent

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/local/picobot/internal/chat"
	"github.com/local/picobot/internal/providers"
)

// ActiveTurn represents the state of an in-flight agent turn that was
// interrupted by a restart. It captures everything needed to resume
// processing or at minimum deliver a partial response.
type ActiveTurn struct {
	// Metadata about the originating message
	Channel  string `json:"channel"`
	ChatID   string `json:"chat_id"`
	SenderID string `json:"sender_id"`
	Content  string `json:"content"` // original user message

	// The full message chain built so far (system + user + assistant + tool messages)
	Messages []providers.Message `json:"messages"`

	// How many iterations have been completed
	Iteration int `json:"iteration"`

	// The last tool result (in case we need to synthesize a response)
	LastToolResult string `json:"last_tool_result"`

	// Whether this turn has been completed (if true, this file is stale)
	Completed bool `json:"completed"`
}

// CheckpointManager handles saving and loading of in-flight turn state.
// It uses a file per conversation: workspace/sessions/{channel}:{chatID}.active.json
type CheckpointManager struct {
	mu        sync.Mutex
	workspace string
}

// NewCheckpointManager creates a new checkpoint manager rooted at workspace.
func NewCheckpointManager(workspace string) *CheckpointManager {
	return &CheckpointManager{workspace: workspace}
}

func (cm *CheckpointManager) sessionsDir() string {
	return filepath.Join(cm.workspace, "sessions")
}

func (cm *CheckpointManager) activePath(key string) string {
	return filepath.Join(cm.sessionsDir(), key+".active.json")
}

// Save persists the current in-flight turn state to disk.
// This should be called before each LLM invocation within the agent loop
// so that the most recent state is always available for recovery.
func (cm *CheckpointManager) Save(key string, turn *ActiveTurn) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	dir := cm.sessionsDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	b, err := json.MarshalIndent(turn, "", "  ")
	if err != nil {
		return err
	}

	// Write atomically: write to temp file then rename
	tmpPath := cm.activePath(key) + ".tmp"
	if err := os.WriteFile(tmpPath, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, cm.activePath(key))
}

// MarkCompleted marks an active turn as completed so it won't be recovered.
func (cm *CheckpointManager) MarkCompleted(key string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	path := cm.activePath(key)
	// Check if the file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // nothing to mark
	}

	turn := &ActiveTurn{Completed: true}
	b, err := json.MarshalIndent(turn, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// Load reads the active turn state for a given session key.
// Returns nil if no active turn exists or if it was already completed.
func (cm *CheckpointManager) Load(key string) *ActiveTurn {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	path := cm.activePath(key)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var turn ActiveTurn
	if err := json.Unmarshal(b, &turn); err != nil {
		log.Printf("checkpoint: failed to parse %s: %v", path, err)
		return nil
	}

	if turn.Completed {
		return nil
	}

	return &turn
}

// Delete removes the active turn file after successful recovery.
func (cm *CheckpointManager) Delete(key string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	path := cm.activePath(key)
	os.Remove(path)
}

// RecoverAll scans the sessions directory for any .active.json files
// that haven't been completed and returns them as InboundMessages
// that can be re-injected into the agent loop.
func (cm *CheckpointManager) RecoverAll() []InboundRecovery {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	dir := cm.sessionsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var recoveries []InboundRecovery
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < len(".active.json") || name[len(name)-len(".active.json"):] != ".active.json" {
			continue
		}

		// Extract key: "telegram:12345.active.json" -> "telegram:12345"
		key := name[:len(name)-len(".active.json")]

		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}

		var turn ActiveTurn
		if err := json.Unmarshal(b, &turn); err != nil {
			continue
		}

		if turn.Completed {
			continue
		}

		recoveries = append(recoveries, InboundRecovery{
			Key:  key,
			Turn: &turn,
		})
	}

	return recoveries
}

// InboundRecovery pairs a session key with its recovered active turn.
type InboundRecovery struct {
	Key  string
	Turn *ActiveTurn
}

// ToInbound converts a recovered turn back into an Inbound message for the hub.
func (r *InboundRecovery) ToInbound() chat.Inbound {
	return chat.Inbound{
		Channel:  r.Turn.Channel,
		SenderID: r.Turn.SenderID,
		ChatID:   r.Turn.ChatID,
		Content:  r.Turn.Content,
	}
}
