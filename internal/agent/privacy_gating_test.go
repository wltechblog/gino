package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/agent/memory"
	"github.com/wltechblog/gino/internal/brain"
	"github.com/wltechblog/gino/internal/providers"
)

// mockExtractProvider returns canned content, used to drive extractTurnMemory.
type mockExtractProvider struct {
	content string
}

func (m *mockExtractProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	return providers.LLMResponse{Content: m.content}, nil
}

func (m *mockExtractProvider) GetDefaultModel() string { return "mock" }

func (m *mockExtractProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

// TestUnprivilegedPromptExcludesMemory verifies the privacy gate in
// BuildMessages: unprivileged users must NOT receive the file-based memory
// context (MEMORY.md + daily note) or the ranked short-term memories —
// both hold owner facts and admin DM content.
// allPromptContent joins every message content in the chain. The volatile
// context (memory/brain/current-user) is folded into the current user
// message as a <turn_context> wrap, so privacy assertions must scan all
// roles, not just system messages.
func allPromptContent(msgs []providers.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}

func TestUnprivilegedPromptExcludesMemory(t *testing.T) {
	cb := NewContextBuilder(t.TempDir(), nil, 5)

	memCtx := "MEMORY.md content: admin's secret API key location"
	memories := []memory.MemoryItem{
		{Text: "[exec] cat /etc/shadow output for admin task", Kind: "short"},
		{Text: "Discord user's own preference", Kind: "short"},
	}

	// Unprivileged Discord user
	msgs := cb.BuildMessages(nil, "hello", "discord", "999", "u1", memCtx, memories, map[string]interface{}{
		"privileged":  false,
		"sender_name": "RandomUser",
	})
	sys := allPromptContent(msgs)

	if strings.Contains(sys, "admin's secret") {
		t.Error("unprivileged prompt contains MEMORY.md content (daily note leak)")
	}
	if strings.Contains(sys, "/etc/shadow") {
		t.Error("unprivileged prompt contains owner tool output from short-term memory")
	}
	if strings.Contains(sys, "Memory:\n") {
		t.Error("unprivileged prompt contains the Memory: block at all")
	}
	if strings.Contains(sys, "Relevant memories:\n") {
		t.Error("unprivileged prompt contains Relevant memories block")
	}

	// Privileged user still gets everything
	msgs = cb.BuildMessages(nil, "hello", "telegram", "123", "owner", memCtx, memories, map[string]interface{}{
		"privileged": true,
	})
	sys = allPromptContent(msgs)
	if !strings.Contains(sys, "admin's secret") {
		t.Error("privileged prompt lost MEMORY.md content — regression in owner path")
	}
	if !strings.Contains(sys, "/etc/shadow") {
		t.Error("privileged prompt lost short-term memories — regression in owner path")
	}
}

// TestUnprivilegedBrainScoped verifies unprivileged brain searches are
// scoped to the user's own source, never global.
func TestUnprivilegedBrainScoped(t *testing.T) {
	b, err := brain.Init(filepath.Join(t.TempDir(), "test.db"), nil, brain.DefaultOptions())
	if err != nil {
		t.Fatalf("brain init: %v", err)
	}
	defer b.Close()

	if _, err := b.IngestPage(context.Background(), brain.Page{
		SourceID: "default", Slug: "owner-secret", Type: "note",
		Title: "Owner secret project", Content: "Owner secret project codename is Xylophone",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.IngestPage(context.Background(), brain.Page{
		SourceID: "user:discord:u1", Slug: "user-note", Type: "note",
		Title: "User preference", Content: "User u1 prefers concise answers about codename",
	}); err != nil {
		t.Fatal(err)
	}

	cb := NewContextBuilder(t.TempDir(), nil, 5)
	cb.SetBrain(b)

	// Unprivileged user — must be scoped to their source only
	msgs := cb.BuildMessages(nil, "concise codename", "discord", "999", "u1", "", nil, map[string]interface{}{
		"privileged": false,
	})
	sys := allPromptContent(msgs)
	if strings.Contains(sys, "Xylophone") {
		t.Error("unprivileged prompt contains global brain page (leak via brain context)")
	}
	if !strings.Contains(sys, "concise answers") {
		t.Error("unprivileged prompt missing user-scoped brain content (personalization broken)")
	}
}

// TestUnprivilegedTurnExtractRouting verifies turn-extract facts from
// unprivileged turns go ONLY to the per-user brain source, never the
// global daily note — and privileged turns keep the daily-note path.
func TestUnprivilegedTurnExtractRouting(t *testing.T) {
	dir := t.TempDir()
	b, err := brain.Init(filepath.Join(dir, "brain.db"), nil, brain.DefaultOptions())
	if err != nil {
		t.Fatalf("brain init: %v", err)
	}
	defer b.Close()

	al := &AgentLoop{
		memory:   memory.NewMemoryStoreWithWorkspace(dir, 100),
		provider: &mockExtractProvider{content: "- User likes cats"},
		brain:    b,
	}

	// Unprivileged Discord turn
	al.extractTurnMemory("what pets do you like", "I like cats too!", nil, "discord", "u1", map[string]interface{}{
		"privileged": false,
	})

	// Daily note must NOT contain the fact
	today, _ := al.memory.ReadToday()
	if strings.Contains(today, "cats") {
		t.Error("unprivileged turn-extract fact leaked into global daily note")
	}

	// User brain source must contain it
	results, err := b.Search(context.Background(), "cats", brain.SearchOpts{Sources: []string{"user:discord:u1"}})
	if err != nil || len(results) == 0 {
		t.Errorf("unprivileged fact missing from per-user brain source: %v (results=%d)", err, len(results))
	}

	// Privileged telegram turn — daily note must contain it
	al.extractTurnMemory("what pets do you like", "I like cats too!", nil, "telegram", "owner", map[string]interface{}{
		"privileged": true,
	})
	today, _ = al.memory.ReadToday()
	if !strings.Contains(today, "cats") {
		t.Error("privileged turn-extract fact missing from daily note — owner path broken")
	}
}

// TestCaptureToolMemoryPrivilegeGate verifies unprivileged tool outputs
// never enter shared short-term memory (which is injected into prompts).
func TestCaptureToolMemoryPrivilegeGate(t *testing.T) {
	dir := t.TempDir()
	al := &AgentLoop{memory: memory.NewMemoryStoreWithWorkspace(dir, 100)}

	// Unprivileged exec output must be dropped
	al.captureToolMemory("exec", "secret admin command output", false)
	for _, it := range al.memory.Recent(10) {
		if strings.Contains(it.Text, "secret admin command") {
			t.Error("unprivileged exec output leaked into shared short-term memory")
		}
	}

	// Privileged exec output must be captured
	al.captureToolMemory("exec", "owner command output captured", true)
	found := false
	for _, it := range al.memory.Recent(10) {
		if strings.Contains(it.Text, "owner command output captured") {
			found = true
		}
	}
	if !found {
		t.Error("privileged exec output missing from short-term memory — owner path broken")
	}
}

// TestUnprivilegedSystemNotice checks the pre-existing unprivileged notice
// still fires while owner prompts stay clean (regression guard for the
// metadata contract consumed by the gating code).
func TestUnprivilegedSystemNotice(t *testing.T) {
	cb := NewContextBuilder(t.TempDir(), nil, 5)

	msgs := cb.BuildMessages(nil, "hi", "telegram", "-100123", "999", "", nil, map[string]interface{}{
		"privileged":  false,
		"group":       true,
		"sender_name": "GroupMember",
	})
	if !strings.Contains(allPromptContent(msgs), "UNPRIVILEGED USER") {
		t.Error("group user missing unprivileged system notice")
	}

	msgs = cb.BuildMessages(nil, "hi", "telegram", "123", "owner", "", nil, map[string]interface{}{
		"privileged": true,
	})
	if strings.Contains(allPromptContent(msgs), "UNPRIVILEGED USER") {
		t.Error("owner DM got unprivileged notice — privilege regression")
	}
}
