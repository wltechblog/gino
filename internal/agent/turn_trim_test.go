package agent

import (
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/providers"
)

// withTrimCap temporarily sets the process-global emergency-trim cap and
// restores the default on cleanup. Every cap-mutating test here MUST go
// through it — the cap is global state shared by the whole test binary,
// and a leaked small cap would silently trim every other agent test.
func withTrimCap(t *testing.T, capChars int) {
	t.Helper()
	providers.SetMaxRequestChars(capChars)
	t.Cleanup(func() { providers.SetMaxRequestChars(providers.DefaultMaxRequestChars) })
}

// assertToolPairing scans a chain and fails if any tool result lacks a
// kept assistant tool_calls message with a matching call ID right before
// it (strict servers reject orphaned tool messages).
func assertToolPairing(t *testing.T, msgs []providers.Message) {
	t.Helper()
	for i, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		ok := false
		if i > 0 && msgs[i-1].Role == "assistant" {
			for _, tc := range msgs[i-1].ToolCalls {
				if tc.ID == m.ToolCallID {
					ok = true
				}
			}
		}
		if !ok {
			t.Fatalf("orphaned tool result %q at index %d (no matching assistant tool_calls before it)", m.ToolCallID, i)
		}
	}
}

func chainChars(msgs []providers.Message) int {
	total := 0
	for i := range msgs {
		total += turnTrimMsgChars(&msgs[i])
	}
	return total
}

// TestTurnTrimNoopOnHealthyChain: a small chain passes through untouched.
func TestTurnTrimNoopOnHealthyChain(t *testing.T) {
	msgs := []providers.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "history"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "current"},
	}
	out, newIdx := trimTurnForTokens(msgs, 3)
	if len(out) != len(msgs) {
		t.Fatalf("healthy chain changed: %d -> %d messages", len(msgs), len(out))
	}
	for i := range out {
		if out[i].Content != msgs[i].Content {
			t.Fatalf("healthy chain message %d modified", i)
		}
	}
	if newIdx != 3 {
		t.Fatalf("newIdx: want 3, got %d", newIdx)
	}
}

// TestTurnTrimDisabledByZeroCap: maxRequestChars=0 disables the guard —
// even a hugely oversized chain passes through untouched.
func TestTurnTrimDisabledByZeroCap(t *testing.T) {
	withTrimCap(t, 0)
	fat := strings.Repeat("F", 500_000)
	msgs := []providers.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: fat},
	}
	out, newIdx := trimTurnForTokens(msgs, 1)
	if len(out) != 2 || out[1].Content != fat {
		t.Fatalf("disabled trim modified the chain (len=%d)", len(out))
	}
	if newIdx != 1 {
		t.Fatalf("newIdx: want 1, got %d", newIdx)
	}
}

// TestTurnTrimShrinksFatShortChain is THE regression pin (work-order
// item 3): a chain far under maxTurnMessages in COUNT but over the char
// budget must still shrink — the old guard gated on count only, let this
// shape through, and every iteration burned on provider hard-rejects.
// Uses the default cap (240000 -> budget 200000).
func TestTurnTrimShrinksFatShortChain(t *testing.T) {
	msgs := []providers.Message{
		{Role: "system", Content: strings.Repeat("S", 100)},
		{Role: "user", Content: "h1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "current question"}, // idx 3 = userMsgIdx
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "call-1", Name: "exec", Arguments: map[string]interface{}{"cmd": "ls"}}}},
		{Role: "tool", ToolCallID: "call-1", Content: strings.Repeat("T", 300_000)},
	}
	out, newIdx := trimTurnForTokens(msgs, 3)

	budget := providers.EffectiveMaxRequestChars() - turnTrimHeadroom
	if got := chainChars(out); got > budget {
		t.Fatalf("chain still over budget: %d > %d", got, budget)
	}
	if out[0].Content != msgs[0].Content {
		t.Fatalf("system message not preserved")
	}
	if newIdx < 0 || newIdx >= len(out) || out[newIdx].Content != "current question" {
		t.Fatalf("current user message lost or newIdx wrong (newIdx=%d)", newIdx)
	}
	for _, m := range out {
		if m.Role == "tool" {
			t.Fatalf("fat tool result survived the trim")
		}
	}
	foundStub := false
	for _, m := range out {
		if strings.Contains(m.Content, "trimmed automatically") {
			foundStub = true
		}
	}
	if !foundStub {
		t.Fatalf("no trim stub inserted")
	}
}

// TestTurnTrimKeepsToolPairing: an over-budget chain with two fat pairs
// drops the OLDER pair whole; the newer pair must survive intact.
func TestTurnTrimKeepsToolPairing(t *testing.T) {
	msgs := []providers.Message{
		{Role: "system", Content: strings.Repeat("S", 100)},
		{Role: "user", Content: "current"}, // idx 1
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "call-1", Name: "web", Arguments: map[string]interface{}{"url": "http://x/" + strings.Repeat("a", 30)}}}},
		{Role: "tool", ToolCallID: "call-1", Content: strings.Repeat("1", 150_000)},
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "call-2", Name: "exec", Arguments: map[string]interface{}{"cmd": "ls"}}}},
		{Role: "tool", ToolCallID: "call-2", Content: strings.Repeat("2", 120_000)},
	}
	out, _ := trimTurnForTokens(msgs, 1)

	assertToolPairing(t, out)
	budget := providers.EffectiveMaxRequestChars() - turnTrimHeadroom
	if got := chainChars(out); got > budget {
		t.Fatalf("chain still over budget: %d > %d", got, budget)
	}
	sawCall2 := false
	for _, m := range out {
		if m.ToolCallID == "call-2" {
			sawCall2 = true
		}
		if m.ToolCallID == "call-1" {
			t.Fatalf("older pair partially survived: call-1 present")
		}
	}
	if !sawCall2 {
		t.Fatalf("newer pair dropped when only the older pair needed to go")
	}
}

// TestTurnTrimRepairsOrphans: when a budget cut lands mid-pair, the kept
// tool result must be dropped rather than orphaned.
func TestTurnTrimRepairsOrphans(t *testing.T) {
	withTrimCap(t, 60_000) // budget 20_000
	msgs := []providers.Message{
		{Role: "system", Content: strings.Repeat("S", 100)},
		{Role: "user", Content: "current"}, // idx 1
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "call-1", Name: "exec", Arguments: map[string]interface{}{"cmd": "big"}}}},
		{Role: "tool", ToolCallID: "call-1", Content: strings.Repeat("T", 30_000)},
	}
	out, _ := trimTurnForTokens(msgs, 1)
	assertToolPairing(t, out) // the tool result must not survive alone
	if out[0].Content != msgs[0].Content || out[1].Content != "current" {
		t.Fatalf("floor not preserved")
	}
}

// TestTurnTrimNeverDropsFloor: even when the floor itself busts the
// budget, system + current user survive and the function does not
// panic — the emergency net downstream handles content clamping.
func TestTurnTrimNeverDropsFloor(t *testing.T) {
	withTrimCap(t, 60_000) // budget 20_000
	msgs := []providers.Message{
		{Role: "system", Content: strings.Repeat("S", 500_000)},
		{Role: "user", Content: strings.Repeat("H", 30_000)},
		{Role: "user", Content: "current"}, // idx 2
	}
	out, newIdx := trimTurnForTokens(msgs, 2)
	if out[0].Content != msgs[0].Content {
		t.Fatalf("oversized system message dropped — floor violated")
	}
	if newIdx < 0 || newIdx >= len(out) || out[newIdx].Content != "current" {
		t.Fatalf("current user message lost (newIdx=%d, len=%d)", newIdx, len(out))
	}
}

// TestTurnTrimDropsOldestFirst: trimming eats history before the recent
// tail — "I forgot the list" must not come back via the size guard.
func TestTurnTrimDropsOldestFirst(t *testing.T) {
	msgs := []providers.Message{
		{Role: "system", Content: strings.Repeat("S", 100)},
		{Role: "user", Content: strings.Repeat("U", 250_000)}, // old fat history
		{Role: "assistant", Content: "kept answer"},
		{Role: "user", Content: "middle"},
		{Role: "user", Content: "current"}, // idx 4
	}
	out, newIdx := trimTurnForTokens(msgs, 4)

	if strings.Contains(joinContents(out), strings.Repeat("U", 100)) {
		t.Fatalf("oldest fat history survived")
	}
	if out[newIdx].Content != "current" {
		t.Fatalf("newIdx points at %q", out[newIdx].Content)
	}
	if !strings.Contains(joinContents(out), "kept answer") || !strings.Contains(joinContents(out), "middle") {
		t.Fatalf("recent tail was dropped before older history")
	}
	budget := providers.EffectiveMaxRequestChars() - turnTrimHeadroom
	if got := chainChars(out); got > budget {
		t.Fatalf("chain still over budget: %d > %d", got, budget)
	}
}

func joinContents(msgs []providers.Message) string {
	var sb strings.Builder
	for i := range msgs {
		sb.WriteString(msgs[i].Content)
		sb.WriteString("\n")
	}
	return sb.String()
}
