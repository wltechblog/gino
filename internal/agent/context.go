package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/wltechblog/gino/internal/brain"
	"github.com/wltechblog/gino/internal/agent/memory"
	"github.com/wltechblog/gino/internal/agent/skills"
	"github.com/wltechblog/gino/internal/providers"
)

// ContextBuilder builds messages for the LLM from session history and current message.
type ContextBuilder struct {
	workspace    string
	ranker       memory.Ranker
	topK         int
	skillsLoader *skills.Loader
	brain        *brain.Brain
}

func NewContextBuilder(workspace string, r memory.Ranker, topK int) *ContextBuilder {
	return &ContextBuilder{
		workspace:    workspace,
		ranker:       r,
		topK:         topK,
		skillsLoader: skills.NewLoader(workspace),
	}
}

// SetBrain attaches a knowledge brain for context enrichment.
func (cb *ContextBuilder) SetBrain(b *brain.Brain) {
	cb.brain = b
}

func (cb *ContextBuilder) BuildMessages(history []string, currentMessage string, channel, chatID, senderID string, memoryContext string, memories []memory.MemoryItem) []providers.Message {
	msgs := make([]providers.Message, 0, len(history)+2)

	// Combine all system instructions into one message at position 0 to avoid errors in strict chat templates (e.g. llama.cpp)
	var sysParts []string

	sysParts = append(sysParts, "You are Gino, a helpful assistant.")

	// Load workspace bootstrap files
	bootstrapFiles := []string{"SOUL.md", "AGENTS.md", "USER.md", "TOOLS.md"}
	for _, name := range bootstrapFiles {
		p := filepath.Join(cb.workspace, name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue // file may not exist yet, skip silently
		}
		content := strings.TrimSpace(string(data))
		if content != "" {
			sysParts = append(sysParts, fmt.Sprintf("## %s\n\n%s", name, content))
		}
	}

	// Channel context and tool availability
	sysParts = append(sysParts, fmt.Sprintf(
		"You are operating on channel=%q chatID=%q with workspace=%q. You have full access to all registered tools regardless of the channel. Always use your tools when the user asks you to perform actions (file operations, shell commands, web fetches, etc.).",
		channel, chatID, cb.workspace))

	// Telegram-specific formatting instructions
	if channel == "telegram" {
		sysParts = append(sysParts, `Format your response using Telegram-compatible MarkdownV2. Supported formatting (and ONLY these):

*bold text*   _italic text_   __underline__   ~strikethrough~   ||spoiler||   `+"`"+`inline code`+"`"+`

`+"```"+`
code block
`+"```"+`

[inline URL](https://www.example.com/)   > Block quotation

Do NOT use: # headings, --- rulers, *-bullet-lists, --dash-lists, 1.-numbered-lists — Telegram does not support them. Avoid underscores inside words (like 'some_var') — Telegram interprets `+"`"+`_`+"`"+` as italic markers and will break. Keep responses clean and readable.`)
	}

	// User identity — include sender info for non-system channels so the LLM
	// can personalize responses and distinguish between users.
	if senderID != "" && channel != "cli" {
		sysParts = append(sysParts, fmt.Sprintf("Current user ID: %s (channel: %s)", senderID, channel))
	}

	// Memory tool instruction
	sysParts = append(sysParts, "If you decide something should be remembered, call the tool 'write_memory' with JSON arguments: {\"target\": \"today\"|\"long\", \"content\": \"...\", \"append\": true|false}. Use a tool call rather than plain chat text when writing memory.")

	// Skills context
	loadedSkills, err := cb.skillsLoader.LoadAll()
	if err != nil {
		log.Printf("error loading skills: %v", err)
	}
	if len(loadedSkills) > 0 {
		var sb strings.Builder
		sb.WriteString("Available Skills:\n")
		for _, skill := range loadedSkills {
			fmt.Fprintf(&sb, "\n## %s\n%s\n\n%s\n", skill.Name, skill.Description, skill.Content)
		}
		sysParts = append(sysParts, sb.String())
	}

	// File-based memory context (long-term + today's notes)
	if memoryContext != "" {
		sysParts = append(sysParts, "Memory:\n"+memoryContext)
	}

	// Top-K ranked memories
	selected := memories
	if cb.ranker != nil && len(memories) > 0 {
		selected = cb.ranker.Rank(currentMessage, memories, cb.topK)
	}
	if len(selected) > 0 {
		var sb strings.Builder
		sb.WriteString("Relevant memories:\n")
		for _, m := range selected {
			fmt.Fprintf(&sb, "- %s (%s)\n", m.Text, m.Kind)
		}
		sysParts = append(sysParts, sb.String())
	}

	// Brain context enrichment — search the knowledge brain for relevant info.
	// For non-owner channels (e.g. Discord), scope search to the user's personal
	// source first, then fall back to global.
	if cb.brain != nil {
		searchOpts := brain.SearchOpts{Limit: 5}

		// Determine if this is a non-owner channel that needs user-scoped memory
		if senderID != "" && channel != "cli" && channel != "telegram" {
			userSource := fmt.Sprintf("user:%s:%s", channel, senderID)
			searchOpts.Sources = []string{userSource}
		}

		results, err := cb.brain.Search(context.Background(), currentMessage, searchOpts)
		if err == nil && len(results) > 0 {
			var brainSb strings.Builder
			brainSb.WriteString("Relevant Brain Context:\n")
			for _, r := range results {
				fmt.Fprintf(&brainSb, "- [%s] %s: %s\n", r.Type, r.Title, r.Snippet)
			}
			sysParts = append(sysParts, brainSb.String())
		}
	}

	// Emit the single consolidated system message
	msgs = append(msgs, providers.Message{Role: "system", Content: strings.Join(sysParts, "\n\n")})

	// Replay history, preserving each message's original role (user/assistant).
	// Items are stored in "role: content" format by session.AddMessage.
	for _, h := range history {
		if len(h) == 0 {
			continue
		}
		role := "user"
		content := h
		if idx := strings.Index(h, ": "); idx > 0 {
			r := h[:idx]
			if r == "user" || r == "assistant" {
				role = r
				content = h[idx+2:]
			}
		}
		msgs = append(msgs, providers.Message{Role: role, Content: content})
	}

	// Current user message
	msgs = append(msgs, providers.Message{Role: "user", Content: currentMessage})
	return msgs
}
