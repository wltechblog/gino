package agent

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/wltechblog/gino/internal/agent/memory"
	"github.com/wltechblog/gino/internal/agent/skills"
	"github.com/wltechblog/gino/internal/providers"
)

// ContextBuilder builds messages for the LLM from session history and current message.
type ContextBuilder struct {
	workspace        string
	profileWorkspace string
	ranker           memory.Ranker
	topK             int
	skillsLoader     *skills.Loader
	oauthNotifier    func() map[string]string // returns server→authURL for pending OAuth
}

func NewContextBuilder(workspace string, r memory.Ranker, topK int) *ContextBuilder {
	return NewContextBuilderWithProfile(workspace, workspace, r, topK)
}

func NewContextBuilderWithProfile(workspace, profileWorkspace string, r memory.Ranker, topK int) *ContextBuilder {
	if profileWorkspace == "" {
		profileWorkspace = workspace
	}

	return &ContextBuilder{
		workspace:        workspace,
		profileWorkspace: profileWorkspace,
		ranker:           r,
		topK:             topK,
		skillsLoader:     skills.NewLoader(profileWorkspace),
	}
}

// SetOAuthNotifier attaches a function that returns pending OAuth server names
// and their auth URLs. When non-empty, the system prompt will instruct the agent
// to proactively surface them to the user.
func (cb *ContextBuilder) SetOAuthNotifier(fn func() map[string]string) {
	cb.oauthNotifier = fn
}

func (cb *ContextBuilder) BuildMessages(history []string, currentMessage string, channel, chatID, senderID string, memoryContext string, memories []memory.MemoryItem, brainContext string, metadata map[string]interface{}) []providers.Message {
	msgs := make([]providers.Message, 0, len(history)+2)

	// Combine all system instructions into one message at position 0 to avoid errors in strict chat templates (e.g. llama.cpp)
	var sysParts []string

	sysParts = append(sysParts, "You are Gino, a helpful assistant.")

	// Load persistent Gino profile bootstrap files.
	bootstrapFiles := []string{"SOUL.md", "AGENTS.md", "USER.md", "TOOLS.md"}
	for _, name := range bootstrapFiles {
		p := filepath.Join(cb.profileWorkspace, name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue // file may not exist yet, skip silently
		}
		content := strings.TrimSpace(string(data))
		if content != "" {
			heading := name
			if !sameWorkspace(cb.profileWorkspace, cb.workspace) {
				heading = "Profile " + name
			}
			sysParts = append(sysParts, fmt.Sprintf("## %s\n\n%s", heading, content))
		}
	}

	// When working on a separate project, also load that project's AGENTS.md.
	if !sameWorkspace(cb.profileWorkspace, cb.workspace) {
		p := filepath.Join(cb.workspace, "AGENTS.md")
		if data, err := os.ReadFile(p); err == nil {
			content := strings.TrimSpace(string(data))
			if content != "" {
				sysParts = append(sysParts, fmt.Sprintf("## Project AGENTS.md\n\n%s", content))
			}
		}
	}

	// Channel context and tool availability
	sysParts = append(sysParts, fmt.Sprintf(
		"You are operating on channel=%q chatID=%q with workspace=%q. You have full access to all registered tools regardless of the channel. Always use your tools when the user asks you to perform actions (file operations, shell commands, web fetches, etc.).",
		channel, chatID, cb.workspace))

	// Telegram-specific formatting instructions
	if channel == "telegram" {
		sysParts = append(sysParts, `Format your response using these Markdown styles only:

*bold text*   _italic text_   __underline__   ~strikethrough~   ||spoiler||   `+"`"+`inline code`+"`"+`

`+"```"+`
code block
`+"```"+`

[inline URL](https://www.example.com/)   > Block quotation

Do NOT use: # headings, --- rulers, *-bullet-lists, --dash-lists, 1.-numbered-lists — Telegram does not support them. Avoid underscores inside words (like 'some_var') — Telegram interprets `+"`"+`_`+"`"+` as italic markers and will break. Do NOT escape special characters with backslashes — the system handles MarkdownV2 escaping for you. Keep responses clean and readable.`)
	}

	// User identity — include sender info for non-system channels so the LLM
	// can personalize responses and distinguish between users.
	if senderID != "" && channel != "cli" {
		sysParts = append(sysParts, fmt.Sprintf("Current user ID: %s (channel: %s)", senderID, channel))
	}

	// Privilege level — if metadata marks the user as unprivileged, inject
	// restrictions. This applies to Telegram group users and any other
	// channel that sets privileged=false.
	if metadata != nil {
		if privileged, ok := metadata["privileged"].(bool); ok && !privileged {
			senderName := ""
			if name, ok := metadata["sender_name"].(string); ok {
				senderName = name
			}
			parts := []string{
				"⚠️ UNPRIVILEGED USER: This user is not the owner. You must NOT execute shell commands, file operations, or any system-modifying tools on their behalf.",
				"You may answer questions, explain concepts, search the web, and provide helpful information.",
				"Do NOT read or expose sensitive files, API keys, credentials, or internal system configuration.",
			}
			if senderName != "" {
				parts = append(parts, fmt.Sprintf("The user's name is %s. Be friendly and helpful.", senderName))
			}
			sysParts = append(sysParts, strings.Join(parts, " "))
		}
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

	// Brain context enrichment — pre-computed by the caller (per-user in multi-tenant mode).
	if brainContext != "" {
		sysParts = append(sysParts, "Relevant Brain Context:\n"+brainContext)
	}

	// Pending OAuth notifications — if MCP servers need auth, tell the agent to
	// proactively inform the user.
	if cb.oauthNotifier != nil {
		pending := cb.oauthNotifier()
		if len(pending) > 0 {
			var sb strings.Builder
			sb.WriteString("⚠️ OAuth Authentication Required:\n")
			sb.WriteString("The following MCP servers need authentication. You MUST proactively inform the user about this NOW:\n\n")
			for name, authURL := range pending {
				fmt.Fprintf(&sb, "• Server: %s\n  URL: %s\n", name, authURL)
			}
			sb.WriteString("\nTell the user to open the URL, authenticate, then paste the full redirect URL (from their browser address bar after the page fails to load) back to you. Then use the mcp_auth tool with action='complete' to finish authentication.\n")
			sysParts = append(sysParts, sb.String())
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
