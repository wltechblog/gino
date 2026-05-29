package heartbeat

import "testing"

func TestHasActionableTasks(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected bool
	}{
		{
			name:     "empty file",
			content:  "",
			expected: false,
		},
		{
			name: "template only - no tasks",
			content: `# Heartbeat

This file is checked periodically.

## IMPORTANT RULES

- Do nothing if no tasks
- Never log status

## Periodic Tasks

<!-- Add tasks below -->
<!-- Example:
- Check server status
-->`,
			expected: false,
		},
		{
			name: "has one task",
			content: `# Heartbeat

## Periodic Tasks

- Check server status at https://example.com/health
`,
			expected: true,
		},
		{
			name: "has multiple tasks with bullet",
			content: `# Heartbeat

## Periodic Tasks

- Check server status
- Summarize unread messages
`,
			expected: true,
		},
		{
			name: "has numbered task",
			content: `# Heartbeat

## Periodic Tasks

1. Check server status
`,
			expected: true,
		},
		{
			name: "comment-only tasks section",
			content: `# Heartbeat

## Periodic Tasks

<!-- nothing here -->
`,
			expected: false,
		},
		{
			name: "no task section at all",
			content: `# Heartbeat

Just instructions and rules.
`,
			expected: false,
		},
		{
			name: "asterisk bullet task",
			content: `# Heartbeat

## Periodic Tasks

* Do something important
`,
			expected: true,
		},
		{
			name: "task in Actions section",
			content: `# Heartbeat

## Actions

- Run backup
`,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasActionableTasks(tt.content)
			if got != tt.expected {
				t.Errorf("hasActionableTasks() = %v, want %v", got, tt.expected)
			}
		})
	}
}
