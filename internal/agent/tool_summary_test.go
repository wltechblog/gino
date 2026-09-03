package agent

import (
	"strings"
	"testing"
)

func TestStripLeakedToolSummaryTrailing(t *testing.T) {
	reply := "Here's the fix summary.\n\nAll tests pass."
	leaked := reply + "\n\n[Previous turn made 3 tool call(s):\n  exec → ls -la → ok\n  filesystem edit main.go → edited\n  exec → go build ./... → (tool error) exit status 1\n  exec → tests [10]\n]"
	got := stripLeakedToolSummary(leaked)
	if got != reply {
		t.Fatalf("expected trailing block stripped, got %q", got)
	}
}

func TestStripLeakedToolSummaryOnlyBlock(t *testing.T) {
	leaked := "[Previous turn made 1 tool call(s):\n  exec → echo hi → ok\n]"
	if got := stripLeakedToolSummary(leaked); got != "" {
		t.Fatalf("expected empty after stripping block-only reply, got %q", got)
	}
}

func TestStripLeakedToolSummaryClean(t *testing.T) {
	clean := "Normal reply mentioning tool call(s): in prose but no block."
	if got := stripLeakedToolSummary(clean); got != clean {
		t.Fatalf("clean reply must be unchanged, got %q", got)
	}
}

func TestStripLeakedToolSummaryNoHeaderMatch(t *testing.T) {
	// A bracketed line that is NOT the leak header must survive.
	clean := "Result: [Previous turn made something up entirely by the model]"
	if got := stripLeakedToolSummary(clean); got != clean {
		t.Fatalf("non-matching bracket text must be unchanged, got %q", got)
	}
}

func TestSummarizeToolCallsHeader(t *testing.T) {
	s := summarizeToolCalls([]toolCallRecord{{Name: "exec", Args: map[string]interface{}{"cmd": "ls"}, Result: "ok"}})
	if !strings.HasPrefix(s, "[Previous turn made 1 tool call(s):") {
		t.Fatalf("unexpected summary: %q", s)
	}
	if !strings.Contains(s, "exec") {
		t.Fatalf("summary should name the tool: %q", s)
	}
}

func TestToolCallContextMessageSystemRole(t *testing.T) {
	m := toolCallContextMessage("[Previous turn made 1 tool call(s):\n  exec → ls → ok\n]")
	if m.Role != "system" {
		t.Fatalf("summary context must be system role, got %q", m.Role)
	}
	if !strings.Contains(m.Content, "Do NOT quote") {
		t.Fatalf("wrapper must carry the no-echo instruction: %q", m.Content)
	}
}
