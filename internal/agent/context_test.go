package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/agent/memory"
)

func TestBuildMessagesIncludesMemories(t *testing.T) {
	cb := NewContextBuilder(".", memory.NewSimpleRanker(), 5)
	history := []string{"user: hi"}
	mems := []memory.MemoryItem{{Kind: "short", Text: "remember this"}, {Kind: "long", Text: "big fact"}}
	memCtx := "Long-term memory: important fact"
	msgs := cb.BuildMessages(history, "hello", "telegram", "123", "", memCtx, mems, "", nil)

	// Expect at least 1 system message + 1 user history + 1 current user message
	if len(msgs) < 3 {
		t.Fatalf("expected at least 3 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("expected first message to be system prompt, got %s", msgs[0].Role)
	}
	// find a system message containing the memory context
	foundMemCtx := false
	foundSummary := false
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "Long-term memory: important fact") {
			foundMemCtx = true
		}
		if m.Role == "system" && strings.Contains(m.Content, "remember this") && strings.Contains(m.Content, "big fact") {
			foundSummary = true
		}
	}
	if !foundMemCtx {
		t.Fatalf("expected memory context system message to be present in messages: %v", msgs)
	}
	if !foundSummary {
		t.Fatalf("expected memory summary to be present in messages: %v", msgs)
	}
}

func TestBuildMessagesLoadsProfileAndProjectAgents(t *testing.T) {
	profile := t.TempDir()
	project := t.TempDir()
	mustWrite := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(profile, "SOUL.md", "profile soul")
	mustWrite(profile, "AGENTS.md", "profile agents")
	mustWrite(profile, "USER.md", "profile user")
	mustWrite(profile, "TOOLS.md", "profile tools")
	mustWrite(project, "AGENTS.md", "project agents")
	mustWrite(project, "SOUL.md", "project soul should be ignored")

	cb := NewContextBuilderWithProfile(project, profile, memory.NewSimpleRanker(), 5)
	msgs := cb.BuildMessages(nil, "hello", "cli", "1", "", "", nil, "", nil)
	if len(msgs) == 0 || msgs[0].Role != "system" {
		t.Fatalf("expected system message, got %#v", msgs)
	}
	sys := msgs[0].Content
	for _, want := range []string{
		"## Profile SOUL.md", "profile soul",
		"## Profile AGENTS.md", "profile agents",
		"## Profile USER.md", "profile user",
		"## Profile TOOLS.md", "profile tools",
		"## Project AGENTS.md", "project agents",
	} {
		if !strings.Contains(sys, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "project soul should be ignored") {
		t.Fatal("project SOUL.md must not replace profile identity")
	}
}

func TestBuildMessagesBackwardsCompatibleHeadings(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "SOUL.md"), []byte("one workspace soul"), 0o644); err != nil {
		t.Fatal(err)
	}
	cb := NewContextBuilder(ws, memory.NewSimpleRanker(), 5)
	msgs := cb.BuildMessages(nil, "hello", "cli", "1", "", "", nil, "", nil)
	sys := msgs[0].Content
	if !strings.Contains(sys, "## SOUL.md") {
		t.Fatalf("same-root workspace should keep original headings:\n%s", sys)
	}
	if strings.Contains(sys, "## Profile SOUL.md") {
		t.Fatal("profile heading should not appear when profile and project are the same")
	}
}
