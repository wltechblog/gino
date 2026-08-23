package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// --- Test helpers ---

// mockSummarizingProvider returns a canned summary when it sees the summarization prompt,
// otherwise echoes the last user message.
type mockSummarizingProvider struct {
	summarizedInput string // captured input to summarization call
	summarizeResult string // what to return for summarization calls
	chatCalls       int
}

func (p *mockSummarizingProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	p.chatCalls++
	// Detect summarization call by checking system prompt
	for _, m := range messages {
		if m.Role == "system" && strings.Contains(m.Content, "conversation summarizer") {
			// Capture the user message (conversation to summarize)
			for _, m2 := range messages {
				if m2.Role == "user" {
					p.summarizedInput = m2.Content
				}
			}
			result := "## Goal\nTest goal\n## Progress\n### Done\n- Item 1\n## Important Context\nFile: test.go\n## Next Steps\n1. Continue"
			if p.summarizeResult != "" {
				result = p.summarizeResult
			}
			return providers.LLMResponse{Content: result}, nil
		}
	}
	// Regular call — echo
	last := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			last = messages[i].Content
			break
		}
	}
	return providers.LLMResponse{Content: "(mock) " + last}, nil
}

func (p *mockSummarizingProvider) GetDefaultModel() string { return "mock-model" }
func (p *mockSummarizingProvider) GetModelContext(ctx context.Context, model string) (int, error) { return 0, nil }

func makeMessages(count int) []providers.Message {
	msgs := make([]providers.Message, 0, count+1)
	msgs = append(msgs, providers.Message{Role: "system", Content: "You are a helpful assistant."})
	for i := 0; i < count; i++ {
		role := "user"
		content := ""
		if i%2 == 1 {
			role = "assistant"
			content = "Some response text here that is reasonably long enough to be meaningful."
		} else {
			content = "This is a user message with some content to test token estimation."
		}
		msgs = append(msgs, providers.Message{Role: role, Content: content})
	}
	return msgs
}

func makeMessagesWithToolCalls() []providers.Message {
	return []providers.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "read the file"},
		{Role: "assistant", Content: "", ToolCalls: []providers.ToolCall{
			{ID: "tc1", Name: "filesystem", Arguments: map[string]interface{}{"action": "read", "path": "test.txt"}},
		}},
		{Role: "tool", Content: "file contents here", ToolCallID: "tc1"},
		{Role: "assistant", Content: "Here's the file contents: ..."},
		{Role: "user", Content: "now edit line 5"},
		{Role: "assistant", Content: "", ToolCalls: []providers.ToolCall{
			{ID: "tc2", Name: "filesystem", Arguments: map[string]interface{}{"action": "edit", "path": "test.txt"}},
		}},
		{Role: "tool", Content: "edited successfully", ToolCallID: "tc2"},
		{Role: "assistant", Content: "Done! Line 5 has been updated."},
		{Role: "user", Content: "thanks"},
	}
}

// --- Tests ---

func TestEstimateTokens(t *testing.T) {
	msgs := []providers.Message{
		{Role: "user", Content: strings.Repeat("a", 400)}, // 100 tokens
		{Role: "assistant", Content: strings.Repeat("b", 800)}, // 200 tokens
	}
	tokens := estimateTokens(msgs)
	if tokens != 300 {
		t.Errorf("expected 300 tokens, got %d", tokens)
	}
}

func TestEstimateTokensWithToolCalls(t *testing.T) {
	msgs := []providers.Message{
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []providers.ToolCall{
				{ID: "1", Name: "exec", Arguments: map[string]interface{}{"cmd": "ls -la"}},
			},
		},
	}
	tokens := estimateTokens(msgs)
	if tokens <= 0 {
		t.Errorf("expected positive token count for tool calls, got %d", tokens)
	}
}

func TestShouldCompactBelowThreshold(t *testing.T) {
	c := newCompactor(nil, "model", &config.CompactionConfig{
		MaxContextTokens: 1000,
		ReserveTokens:    500,
	}, 60, nil)

	// Small messages — should not trigger compaction
	msgs := []providers.Message{
		{Role: "system", Content: "hi"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "world"},
	}
	if c.shouldCompact(msgs) {
		t.Error("should not compact small context")
	}
}

func TestShouldCompactAboveThreshold(t *testing.T) {
	c := newCompactor(nil, "model", &config.CompactionConfig{
		MaxContextTokens: 500,
		ReserveTokens:    200,
	}, 60, nil)

	// Generate enough messages to exceed (500-200)=300 token threshold
	// Each message ~30 chars = ~7-8 tokens, need ~40+ messages
	msgs := makeMessages(50)
	if !c.shouldCompact(msgs) {
		t.Error("should compact large context")
	}
}

func TestShouldCompactDefaultConfig(t *testing.T) {
	// Test defaults kick in when config values are zero.
	// With no provider (nil), context falls back to 128000 default,
	// and reserve/keepRecent are 15% of that, maxSummary is 5%.
	c := newCompactor(nil, "model", &config.CompactionConfig{}, 60, nil)
	if c.maxContextTokens != 128000 {
		t.Errorf("expected default maxContextTokens=128000, got %d", c.maxContextTokens)
	}
	// 15% of 128000 = 19200, but minReserveTokens=8192 so 19200 wins
	if c.reserveTokens != 19200 {
		t.Errorf("expected default reserveTokens=19200, got %d", c.reserveTokens)
	}
	// 15% of 128000 = 19200, but minKeepRecentTokens=10000 so 19200 wins
	if c.keepRecentTokens != 19200 {
		t.Errorf("expected default keepRecentTokens=19200, got %d", c.keepRecentTokens)
	}
	// 5% of 128000 = 6400, min 2000 so 6400 wins
	if c.maxSummaryTokens != 6400 {
		t.Errorf("expected default maxSummaryTokens=6400, got %d", c.maxSummaryTokens)
	}
}

func TestShouldCompactNilConfig(t *testing.T) {
	c := newCompactor(nil, "model", nil, 60, nil)
	if c.maxContextTokens != 128000 {
		t.Errorf("expected defaults with nil config, got maxContextTokens=%d", c.maxContextTokens)
	}
}

func TestCompactBasicSummarization(t *testing.T) {
	prov := &mockSummarizingProvider{}
	c := newCompactor(prov, "mock-model", &config.CompactionConfig{
		MaxContextTokens:  800,  // low threshold
		ReserveTokens:     400,
		KeepRecentTokens:  100,
		MaxSummaryTokens:  4000,
	}, 60, nil)

	// Create enough messages to trigger compaction
	msgs := makeMessages(60) // ~60 messages of ~50 chars each = ~750 tokens

	result, _, err := c.compact(context.Background(), msgs, len(msgs)-1)
	if err != nil {
		t.Fatalf("compact failed: %v", err)
	}

	// Result should be much shorter than input
	if len(result) >= len(msgs) {
		t.Errorf("expected compacted messages (%d) to be fewer than input (%d)", len(result), len(msgs))
	}

	// First message should be system
	if result[0].Role != "system" {
		t.Error("first message should be system prompt")
	}

	// Second message should contain the summary
	if result[1].Role != "user" {
		t.Error("second message should be the summary (user role)")
	}
	if !strings.Contains(result[1].Content, "Conversation Summary") {
		t.Error("second message should contain conversation summary marker")
	}
	if !strings.Contains(result[1].Content, "Test goal") {
		t.Error("second message should contain the LLM-generated summary")
	}

	// Provider should have been called for summarization
	if prov.summarizedInput == "" {
		t.Error("provider should have received summarization call")
	}

	// Summarization call should have received the old messages
	if !strings.Contains(prov.summarizedInput, "user:") || !strings.Contains(prov.summarizedInput, "assistant:") {
		t.Error("summarization input should contain the conversation messages")
	}
}

func TestCompactPreservesRecentMessages(t *testing.T) {
	prov := &mockSummarizingProvider{}
	c := newCompactor(prov, "mock-model", &config.CompactionConfig{
		MaxContextTokens:  800,
		ReserveTokens:     400,
		KeepRecentTokens:  200,
		MaxSummaryTokens:  4000,
	}, 60, nil)

	msgs := makeMessages(60)

	result, _, err := c.compact(context.Background(), msgs, len(msgs)-1)
	if err != nil {
		t.Fatalf("compact failed: %v", err)
	}

	// The last few messages from the original should be preserved in the tail
	// Check that the original last user message content appears somewhere in result
	lastOriginal := msgs[len(msgs)-1].Content
	found := false
	for _, m := range result {
		if m.Content == lastOriginal {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected last user message to be preserved in compacted result")
	}
}

func TestCompactWithToolCalls(t *testing.T) {
	prov := &mockSummarizingProvider{}
	c := newCompactor(prov, "mock-model", &config.CompactionConfig{
		MaxContextTokens:  500,
		ReserveTokens:     200,
		KeepRecentTokens:  50,
		MaxSummaryTokens:  4000,
	}, 60, nil)

	// Build messages with tool calls that would be in the "old" zone
	msgs := []providers.Message{
		{Role: "system", Content: "You are a helpful assistant."},
	}
	// Old messages with tool calls
	for i := 0; i < 10; i++ {
		msgs = append(msgs, providers.Message{Role: "user", Content: "do something"})
		msgs = append(msgs, providers.Message{
			Role:    "assistant",
			Content: "",
			ToolCalls: []providers.ToolCall{
				{ID: "tc", Name: "exec", Arguments: map[string]interface{}{"cmd": "echo hi"}},
			},
		})
		msgs = append(msgs, providers.Message{Role: "tool", Content: "output here", ToolCallID: "tc"})
		msgs = append(msgs, providers.Message{Role: "assistant", Content: "Done with step " + strings.Repeat("x", 20)})
	}
	// Recent messages
	msgs = append(msgs, providers.Message{Role: "user", Content: "final question"})
	msgs = append(msgs, providers.Message{Role: "assistant", Content: "final answer"})

	result, _, err := c.compact(context.Background(), msgs, len(msgs)-2)
	if err != nil {
		t.Fatalf("compact failed: %v", err)
	}

	// Should have compacted (40+ messages → fewer)
	if len(result) >= len(msgs) {
		t.Errorf("expected compaction, got %d messages (was %d)", len(result), len(msgs))
	}

	// No orphaned tool results — every tool message should have a preceding assistant with tool_calls
	for i, m := range result {
		if m.Role == "tool" {
			if i == 0 {
				t.Error("tool message cannot be first message")
				continue
			}
			prev := result[i-1]
			hasMatching := false
			for _, tc := range prev.ToolCalls {
				if tc.ID == m.ToolCallID {
					hasMatching = true
					break
				}
			}
			if !hasMatching {
				t.Errorf("orphaned tool result at index %d: toolCallID=%q, prev.ToolCalls=%v",
					i, m.ToolCallID, prev.ToolCalls)
			}
		}
	}
}

func TestFindCleanCutPoint(t *testing.T) {
	tests := []struct {
		name     string
		messages []providers.Message
		input    int
		want     int
	}{
		{
			name: "clean cut at normal message",
			messages: []providers.Message{
				{Role: "system"},
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello"},
				{Role: "user", Content: "bye"},
			},
			input: 2,
			want:  2,
		},
		{
			name: "skip past tool result",
			messages: []providers.Message{
				{Role: "system"},
				{Role: "user", Content: "run cmd"},
				{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "t1"}}},
				{Role: "tool", ToolCallID: "t1"},
				{Role: "user", Content: "next"},
			},
			input: 3, // points at tool result
			want:  4, // should skip to the user message after
		},
		{
			name: "skip assistant with tool calls and results",
			messages: []providers.Message{
				{Role: "system"},
				{Role: "user"},
				{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "t1"}, {ID: "t2"}}},
				{Role: "tool", ToolCallID: "t1"},
				{Role: "tool", ToolCallID: "t2"},
				{Role: "user"},
			},
			input: 3, // points at assistant with tool calls
			want:  5, // should skip past both tool results
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findCleanCutPoint(tt.messages, tt.input)
			if got != tt.want {
				t.Errorf("findCleanCutPoint() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCompactFallsBackOnLLMError(t *testing.T) {
	// Provider that always fails summarization
	failProv := &failingSummarizeProvider{}
	c := newCompactor(failProv, "mock-model", &config.CompactionConfig{
		MaxContextTokens:  500,
		ReserveTokens:     200,
		KeepRecentTokens:  50,
		MaxSummaryTokens:  4000,
	}, 10, nil) // fallbackMaxMsgs=10

	msgs := makeMessages(30)

	result, _, err := c.compact(context.Background(), msgs, len(msgs)-1)
	if err != nil {
		t.Fatalf("compact should not error even on LLM failure: %v", err)
	}

	// Should fall back to trim — result should have at most 10+2 messages
	if len(result) > 15 { // generous bound
		t.Errorf("expected fallback trim to limit messages, got %d", len(result))
	}
}

type failingSummarizeProvider struct{}

func (p *failingSummarizeProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	// Detect summarization and fail
	for _, m := range messages {
		if m.Role == "system" && strings.Contains(m.Content, "conversation summarizer") {
			return providers.LLMResponse{}, context.DeadlineExceeded
		}
	}
	return providers.LLMResponse{Content: "ok"}, nil
}
func (p *failingSummarizeProvider) GetDefaultModel() string { return "fail-model" }
func (p *failingSummarizeProvider) GetModelContext(ctx context.Context, model string) (int, error) { return 0, nil }

func TestCompactNothingToSummarize(t *testing.T) {
	prov := &mockSummarizingProvider{}
	c := newCompactor(prov, "mock-model", &config.CompactionConfig{
		MaxContextTokens:  500,
		ReserveTokens:     200,
		KeepRecentTokens:  5000, // huge — everything is "recent"
		MaxSummaryTokens:  4000,
	}, 60, nil)

	msgs := makeMessages(10)

	result, _, err := c.compact(context.Background(), msgs, len(msgs)-1)
	if err != nil {
		t.Fatalf("compact failed: %v", err)
	}

	// Should return unchanged since keepRecentTokens covers everything
	if len(result) != len(msgs) {
		t.Errorf("expected no compaction with huge keepRecent, got %d vs %d messages", len(result), len(msgs))
	}

	// Provider should NOT have been called for summarization
	if prov.summarizedInput != "" {
		t.Error("provider should not have been called when nothing to summarize")
	}
}

func TestCompactDoesNotBreakToolCallPairs(t *testing.T) {
	prov := &mockSummarizingProvider{}
	c := newCompactor(prov, "mock-model", &config.CompactionConfig{
		MaxContextTokens:  600,
		ReserveTokens:     200,
		KeepRecentTokens:  100,
		MaxSummaryTokens:  4000,
	}, 60, nil)

	msgs := makeMessagesWithToolCalls()
	// Duplicate to make it large enough
	for i := 0; i < 8; i++ {
		msgs = append(msgs, makeMessagesWithToolCalls()[1:]...)
	}

	result, _, err := c.compact(context.Background(), msgs, len(msgs)-1)
	if err != nil {
		t.Fatalf("compact failed: %v", err)
	}

	// Verify no orphaned tool results in the compacted output
	for i, m := range result {
		if m.Role == "tool" {
			if i == 0 {
				t.Fatal("tool message at index 0 (orphaned)")
			}
			prev := result[i-1]
			matched := false
			for _, tc := range prev.ToolCalls {
				if tc.ID == m.ToolCallID {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("orphaned tool result at index %d: toolCallID=%q, prev has no matching tool call", i, m.ToolCallID)
			}
		}
		// Verify no assistant with tool_calls is missing its results
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				found := false
				for j := i + 1; j < len(result); j++ {
					if result[j].Role == "tool" && result[j].ToolCallID == tc.ID {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("assistant at index %d has tool call %q with no matching result", i, tc.ID)
				}
			}
		}
	}
}

func TestCompactSummaryMaxLength(t *testing.T) {
	// Provider that returns a very long summary
	longProv := &mockSummarizingProvider{
		summarizeResult: strings.Repeat("This is a very long summary line. ", 1000), // ~34K chars
	}
	c := newCompactor(longProv, "mock-model", &config.CompactionConfig{
		MaxContextTokens:  800,
		ReserveTokens:     400,
		KeepRecentTokens:  100,
		MaxSummaryTokens:  100, // very small — 400 chars max
	}, 60, nil)

	msgs := makeMessages(40)
	result, _, err := c.compact(context.Background(), msgs, len(msgs)-1)
	if err != nil {
		t.Fatalf("compact failed: %v", err)
	}

	// Find the summary message
	for _, m := range result {
		if strings.Contains(m.Content, "Conversation Summary") {
			// The message includes wrapper text + summary, so allow overhead
			// maxSummaryTokens * 4 = 400 chars for summary, plus ~200 chars of wrapper
			if len(m.Content) > 800 {
				t.Errorf("summary should be capped at ~%d chars + overhead, got %d", 100*4, len(m.Content))
			}
			return
		}
	}
	t.Error("no summary message found in result")
}
