package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/agent/memory"
	"github.com/wltechblog/gino/internal/brain"
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

// ---- FIX 3: degenerate-query guard for automatic enrichment ----

// TestIsContentQuery covers the low-signal heuristic directly.
func TestIsContentQuery(t *testing.T) {
	low := []string{"", "  ", "1", "2", "say 1", "ok", "42", "- ?", "!!!"}
	for _, q := range low {
		if isContentQuery(q) {
			t.Errorf("isContentQuery(%q) = true, want false (low-signal)", q)
		}
	}
	high := []string{"deploy the staging cluster", "what is 42?", "fix the api", "résumé update"}
	for _, q := range high {
		if !isContentQuery(q) {
			t.Errorf("isContentQuery(%q) = false, want true", q)
		}
	}
}

// TestBuildMessagesSkipsRankedMemoriesOnLowSignal: the stale-context class
// this closes had a scheduled-reminder turn decline a "credential rotation"
// task that existed nowhere in its session — the only source was the ranker
// surfacing stale high-ranked fragments on a near-empty prompt. Low-signal
// prompts must inject NOTHING.
func TestBuildMessagesSkipsRankedMemoriesOnLowSignal(t *testing.T) {
	cb := NewContextBuilder(".", memory.NewSimpleRanker(), 5)
	mems := []memory.MemoryItem{
		{Kind: "short", Text: "rotate credentials on friday"},
		{Kind: "short", Text: "deploy cluster with blue green"},
	}
	history := []string{"user: hi"}

	// "2" is a degenerate prompt: ranker must not run, memories must not
	// enter the prompt.
	msgs := cb.BuildMessages(history, "2", "telegram", "1", "", "", mems, nil)
	last := msgs[len(msgs)-1]
	if strings.Contains(last.Content, "rotate credentials") {
		t.Fatalf("low-signal prompt ranked stale memories into the prompt: %q", last.Content)
	}
	if strings.Contains(last.Content, "<turn_context>") {
		// other enrichment is possible, but not ranked memories
		if strings.Contains(last.Content, "Relevant memories:") {
			t.Fatalf("low-signal prompt injected ranked memories: %q", last.Content)
		}
	}

	// A content query still ranks normally.
	msgs = cb.BuildMessages(history, "deploy the cluster", "telegram", "1", "", "", mems, nil)
	last = msgs[len(msgs)-1]
	if !strings.Contains(last.Content, "deploy cluster with blue green") {
		t.Fatalf("content query did not rank memories: %q", last.Content)
	}
}

// ---- FIX 3: brain enrichment gated by context floor + low-signal guard ----

func TestBuildMessagesBrainGate(t *testing.T) {
	dir := t.TempDir()
	b, err := brain.Init(filepath.Join(dir, "brain.db"), nil, brain.DefaultOptions())
	if err != nil {
		t.Fatalf("init brain: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	ctx := context.Background()
	if _, err := b.IngestPage(ctx, brain.Page{
		SourceID: "default", Slug: "sunset", Title: "Sunset",
		Content: "sunset over the harbor paints the sky in deep orange light",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	cb := NewContextBuilder(".", memory.NewSimpleRanker(), 5)
	cb.SetBrain(b)
	history := []string{"user: hi"}

	// Content query: enrichment fires.
	msgs := cb.BuildMessages(history, "describe the sunset colors", "telegram", "1", "", "", nil, nil)
	last := msgs[len(msgs)-1]
	if !strings.Contains(last.Content, "Relevant Brain Context:") {
		t.Fatalf("content query missing brain enrichment: %q", last.Content)
	}
	if !strings.Contains(last.Content, "Sunset") {
		t.Fatalf("brain enrichment missing matched page title: %q", last.Content)
	}

	// Degenerate prompt: enrichment must be skipped entirely ("2" cannot
	// meaningfully match anything; pre-fix this injected RRF noise).
	msgs = cb.BuildMessages(history, "2", "telegram", "1", "", "", nil, nil)
	last = msgs[len(msgs)-1]
	if strings.Contains(last.Content, "Relevant Brain Context:") {
		t.Fatalf("low-signal prompt injected brain enrichment: %q", last.Content)
	}

	// Floor sanity: default floor is 0.01 on the fused RRF scale (top hit
	// ≈ 0.0164); matches must survive it.
	res, err := b.Search(ctx, "sunset over harbor", brain.SearchOpts{Limit: 3, KeywordOnly: true, MinScore: b.ContextFloor()})
	if err != nil {
		t.Fatalf("floored search: %v", err)
	}
	if len(res) == 0 {
		t.Fatalf("default floor %f filtered out the matched page", b.ContextFloor())
	}
}
