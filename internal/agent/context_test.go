package agent

import (
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/agent/memory"
)

func TestBuildMessagesIncludesMemories(t *testing.T) {
	cb := NewContextBuilder(".", memory.NewSimpleRanker(), 5)
	history := []string{"user: hi"}
	mems := []memory.MemoryItem{{Kind: "short", Text: "remember this"}, {Kind: "long", Text: "big fact"}}
	memCtx := "Long-term memory: important fact"
	msgs := cb.BuildMessages(history, "hello", "telegram", "123", "", memCtx, mems, nil)

	// Expect at least 1 system message + 1 user history + 1 current user message
	if len(msgs) < 3 {
		t.Fatalf("expected at least 3 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("expected first message to be system prompt, got %s", msgs[0].Role)
	}
	// Memory context and ranked memories now live in the <turn_context> wrap
	// folded into the current user message (cache-friendly layout), not in a
	// separate system message.
	foundMemCtx := false
	foundSummary := false
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		t.Fatalf("expected last message to be the current user message, got %s", last.Role)
	}
	if !strings.Contains(last.Content, "<turn_context>") || !strings.Contains(last.Content, "</turn_context>") {
		t.Fatalf("expected <turn_context> wrap in current user message")
	}
	if strings.Contains(last.Content, "Long-term memory: important fact") {
		foundMemCtx = true
	}
	if strings.Contains(last.Content, "remember this") && strings.Contains(last.Content, "big fact") {
		foundSummary = true
	}
	if last := msgs[len(msgs)-1].Content; strings.Contains(last, "user: hi") {
		t.Fatalf("history leaked into current message")
	}
	if !foundMemCtx {
		t.Fatalf("expected memory context system message to be present in messages: %v", msgs)
	}
	if !foundSummary {
		t.Fatalf("expected memory summary to be present in messages: %v", msgs)
	}
}
