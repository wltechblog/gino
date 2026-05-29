package heartbeat

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/picobot/internal/chat"
)

// hasActionableTasks checks whether the HEARTBEAT.md content contains any
// real tasks. It looks for non-comment, non-heading, non-empty lines under
// a "## Periodic Tasks" (or similar) section. Template rules, instructions,
// and HTML comments are ignored.
func hasActionableTasks(content string) bool {
	lines := strings.Split(content, "\n")
	inTasks := false
	inHTMLComment := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track HTML comment boundaries (may span multiple lines)
		if strings.Contains(trimmed, "<!--") {
			inHTMLComment = true
		}
		if inHTMLComment {
			if strings.Contains(trimmed, "-->") {
				inHTMLComment = false
			}
			continue
		}

		// Detect task section header
		if strings.HasPrefix(trimmed, "## ") {
			lower := strings.ToLower(trimmed)
			if strings.Contains(lower, "task") || strings.Contains(lower, "periodic") || strings.Contains(lower, "action") {
				inTasks = true
			} else {
				inTasks = false
			}
			continue
		}

		if !inTasks {
			continue
		}

		// Skip empty lines, markdown comments, horizontal rules
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "---") {
			continue
		}

		// A line that starts with - or * is a task item
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			return true
		}
		// A numbered task item (e.g., "1. Do something")
		if len(trimmed) > 2 && trimmed[0] >= '0' && trimmed[0] <= '9' && trimmed[1] == '.' {
			return true
		}
	}
	return false
}

// StartHeartbeat starts a periodic check that reads HEARTBEAT.md and pushes
// its content into the agent's inbound chat hub for processing.
// If HEARTBEAT.md has no actionable tasks (only template/instructions),
// the LLM call is skipped entirely.
func StartHeartbeat(ctx context.Context, workspace string, interval time.Duration, hub *chat.Hub) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		log.Printf("heartbeat: started (every %v)", interval)
		for {
			select {
			case <-ctx.Done():
				log.Println("heartbeat: stopping")
				return
			case <-ticker.C:
				path := filepath.Join(workspace, "HEARTBEAT.md")
				data, err := os.ReadFile(path)
				if err != nil {
					// file doesn't exist or can't be read — skip silently
					continue
				}
				content := strings.TrimSpace(string(data))
				if content == "" {
					continue
				}

				// Skip LLM call if there are no actionable tasks
				if !hasActionableTasks(content) {
					continue
				}

				// Push heartbeat content into the agent loop for processing
				log.Println("heartbeat: sending tasks to agent")
				hub.In <- chat.Inbound{
					Channel:  "heartbeat",
					ChatID:   "system",
					SenderID: "heartbeat",
					Content:  "[HEARTBEAT CHECK] Review and execute any pending tasks from HEARTBEAT.md:\n\n" + content,
				}
			}
		}
	}()
}
