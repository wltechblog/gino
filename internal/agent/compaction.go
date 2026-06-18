package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// compactor performs LLM-based context compaction.
type compactor struct {
	provider          providers.LLMProvider
	model             string
	maxContextTokens  int
	reserveTokens     int
	keepRecentTokens  int
	maxSummaryTokens  int
	fallbackMaxMsgs   int // used if compaction LLM call fails
	memoryFlusher     MemoryFlusher
}

// MemoryFlusher extracts key facts from messages and saves them to memory before compaction.
type MemoryFlusher interface {
	FlushToMemory(ctx context.Context, messages []providers.Message) error
}

func newCompactor(provider providers.LLMProvider, model string, cfg *config.CompactionConfig, fallbackMaxMsgs int, flusher MemoryFlusher) *compactor {
	if cfg == nil {
		cfg = &config.CompactionConfig{}
	}
	maxCtx := cfg.MaxContextTokens
	if maxCtx <= 0 {
		maxCtx = 128000
	}
	reserve := cfg.ReserveTokens
	if reserve <= 0 {
		reserve = 16384
	}
	keepRecent := cfg.KeepRecentTokens
	if keepRecent <= 0 {
		keepRecent = 20000
	}
	maxSummary := cfg.MaxSummaryTokens
	if maxSummary <= 0 {
		maxSummary = 4000
	}
	return &compactor{
		provider:          provider,
		model:             model,
		maxContextTokens:  maxCtx,
		reserveTokens:     reserve,
		keepRecentTokens:  keepRecent,
		maxSummaryTokens:  maxSummary,
		fallbackMaxMsgs:   fallbackMaxMsgs,
		memoryFlusher:     flusher,
	}
}

// estimateTokens provides a rough token count using chars/4 heuristic.
// This is a conservative estimate; actual token counts vary by tokenizer.
func estimateTokens(messages []providers.Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content) / 4
		// Tool result messages have a ToolCallID that consumes tokens
		if m.ToolCallID != "" {
			total += len(m.ToolCallID) / 4
		}
		for _, tc := range m.ToolCalls {
			b, _ := json.Marshal(tc.Arguments)
			total += len(b) / 4
			total += len(tc.ID) / 4
			total += len(tc.Name) / 4
		}
	}
	return total
}

// shouldCompact returns true if the total token estimate exceeds the compaction threshold.
func (c *compactor) shouldCompact(messages []providers.Message) bool {
	estimated := estimateTokens(messages)
	threshold := c.maxContextTokens - c.reserveTokens
	return estimated > threshold
}

// compact performs LLM-based summarization of older messages.
// It keeps the system prompt, a summary of old messages, and the recent tail intact.
// Returns the compacted message slice and any error.
func (c *compactor) compact(ctx context.Context, messages []providers.Message, userMsgIdx int) ([]providers.Message, error) {
	if userMsgIdx < 0 {
		userMsgIdx = 0
	}
	if userMsgIdx >= len(messages) {
		userMsgIdx = len(messages) - 1
	}

	// Find the cut point: scan backwards from userMsgIdx accumulating tokens
	// until we've collected keepRecentTokens worth of recent messages.
	recentTokens := 0
	cutIdx := 1 // at minimum, skip system[0]
	for i := userMsgIdx - 1; i >= 1; i-- {
		msgTokens := estimateTokens([]providers.Message{messages[i]})
		recentTokens += msgTokens
		if recentTokens >= c.keepRecentTokens {
			cutIdx = i
			break
		}
	}

	// Ensure we don't cut into tool-call pairs — find a clean boundary.
	// A "tool" role message must always be preceded by an assistant message with tool_calls.
	cutIdx = findCleanCutPoint(messages, cutIdx)

	// If there's nothing old to summarize, return unchanged.
	if cutIdx <= 1 {
		log.Printf("Compaction: nothing to summarize (cutIdx=%d), skipping", cutIdx)
		return messages, nil
	}

	oldMessages := messages[1:cutIdx] // exclude system[0]
	recentMessages := messages[cutIdx:]

	log.Printf("Compaction: summarizing %d old messages (%d tokens), keeping %d recent messages (%d tokens)",
		len(oldMessages), estimateTokens(oldMessages), len(recentMessages), estimateTokens(recentMessages))

	// Memory flush: extract important facts before they're lost to summarization.
	if c.memoryFlusher != nil {
		if flushErr := c.memoryFlusher.FlushToMemory(ctx, oldMessages); flushErr != nil {
			log.Printf("Compaction: memory flush failed (non-fatal): %v", flushErr)
		}
	}

	// Build the summarization prompt
	summary, err := c.summarizeMessages(ctx, oldMessages)
	if err != nil {
		log.Printf("Compaction: LLM summarization failed (%v), falling back to trim", err)
		return trimTurnMessages(messages, userMsgIdx, c.fallbackMaxMsgs), nil
	}

	// Build the compacted message chain:
	// [system] [summary] [recent messages...]
	result := make([]providers.Message, 0, 2+len(recentMessages))
	result = append(result, messages[0]) // system prompt

	summaryContent := fmt.Sprintf("[Conversation Summary — earlier messages have been compacted]\n\n%s\n\n[End of summary. Continue from where we left off.]", summary)
	result = append(result, providers.Message{
		Role:    "user",
		Content: summaryContent,
	})

	// Add recent messages, skipping orphaned tool results at the start
	start := 0
	for start < len(recentMessages) && recentMessages[start].Role == "tool" {
		start++
	}
	result = append(result, recentMessages[start:]...)

	log.Printf("Compaction: %d messages → %d messages (summary %d tokens)",
		len(messages), len(result), estimateTokens([]providers.Message{{Content: summaryContent}}))

	return result, nil
}

// findCleanCutPoint adjusts the cut index so we don't split a tool call from its result.
// It scans forward from the proposed cut point until it finds a non-tool message.
func findCleanCutPoint(messages []providers.Message, cutIdx int) int {
	for cutIdx < len(messages)-1 && messages[cutIdx].Role == "tool" {
		cutIdx++
	}
	// Also skip past the assistant message that initiated the tool calls
	// so we don't leave orphaned tool_calls without their results.
	if cutIdx < len(messages) && messages[cutIdx].Role == "assistant" && len(messages[cutIdx].ToolCalls) > 0 {
		// This assistant message has tool calls — we need its results too.
		// Move cut point past all the tool results for these calls.
		toolCallIDs := make(map[string]bool, len(messages[cutIdx].ToolCalls))
		for _, tc := range messages[cutIdx].ToolCalls {
			toolCallIDs[tc.ID] = true
		}
		cutIdx++ // skip the assistant message
		for cutIdx < len(messages) && messages[cutIdx].Role == "tool" && toolCallIDs[messages[cutIdx].ToolCallID] {
			cutIdx++
		}
	}
	return cutIdx
}

// summarizeMessages calls the LLM to summarize a slice of messages into a structured checkpoint.
func (c *compactor) summarizeMessages(ctx context.Context, messages []providers.Message) (string, error) {
	// Build a text representation of the messages for the summarization prompt
	var sb strings.Builder
	sb.WriteString("Conversation history to summarize:\n\n")
	for i, m := range messages {
		role := m.Role
		content := m.Content
		if content == "" && len(m.ToolCalls) > 0 {
			// Summarize tool calls instead of leaving blank
			var tcNames []string
			for _, tc := range m.ToolCalls {
				tcNames = append(tcNames, tc.Name)
			}
			content = fmt.Sprintf("[Called tools: %s]", strings.Join(tcNames, ", "))
		}
		// Truncate very long individual messages to keep the summarization prompt manageable
		if len(content) > 2000 {
			content = content[:2000] + "... [truncated]"
		}
		sb.WriteString(fmt.Sprintf("[%d] %s: %s\n", i, role, content))
	}

	prompt := `You are a conversation summarizer. Summarize the following conversation history into a structured checkpoint.

Your summary MUST follow this exact format:

## Goal
[What the user is trying to accomplish in 1-2 sentences]

## Progress
### Done
- [completed items with key details]

### In Progress
- [current work items]

## Key Decisions
- [Decision]: [Brief rationale]

## Important Context
[Critical facts, file paths, variable names, values, or user preferences that must be preserved for continuity]

## Next Steps
1. [What should happen next based on the conversation]

Rules:
- Be concise but thorough — do not lose important details
- Preserve exact file paths, variable names, and code references
- Preserve any numbers, IDs, or specific values mentioned
- If the user expressed preferences, include them
- Focus on what's actionable and what must not be forgotten
- Keep the summary under 4000 tokens`

	summarizeMessages := []providers.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: sb.String()},
	}

	resp, err := c.provider.Chat(ctx, summarizeMessages, nil, c.model)
	if err != nil {
		return "", fmt.Errorf("summarization LLM call failed: %w", err)
	}

	summary := resp.Content
	// Cap summary length
	maxChars := c.maxSummaryTokens * 4 // rough token-to-char conversion
	if len(summary) > maxChars {
		summary = summary[:maxChars] + "\n... [summary truncated]"
	}

	return summary, nil
}
