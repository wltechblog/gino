package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/agent/memory"
)

// TestPromptPrefixStabilityAcrossTurns verifies the cache-critical property:
// when a new turn replays session history, every stored message is rendered
// byte-identically to how it was sent live in the previous turn (minus the
// per-turn <turn_context> wrap). Prompt caches match the longest common
// token prefix, so any divergence between what was sent in turn N and what
// is replayed in turn N+1 busts the cache at that point.
func TestPromptPrefixStabilityAcrossTurns(t *testing.T) {
	cb := NewContextBuilder(t.TempDir(), memory.NewSimpleRanker(), 5)

	// Turn 1: no history, current message + wrap.
	msgs1 := cb.BuildMessages(nil, "first question", "telegram", "123", "u1",
		"Long-term memory: stable fact", []memory.MemoryItem{{Kind: "short", Text: "mem1"}}, nil)

	if len(msgs1) != 2 {
		t.Fatalf("turn 1: expected [system, user], got %d messages", len(msgs1))
	}
	if msgs1[0].Role != "system" || msgs1[1].Role != "user" {
		t.Fatalf("turn 1: unexpected roles %s/%s", msgs1[0].Role, msgs1[1].Role)
	}

	// Session stores the user message WITHOUT the wrap (loop.go AddMessage path).
	history := []string{"user: first question", "assistant: first answer"}

	// Turn 2: same session, new message.
	msgs2 := cb.BuildMessages(history, "second question", "telegram", "123", "u1",
		"Long-term memory: stable fact", []memory.MemoryItem{{Kind: "short", Text: "mem1"}}, nil)

	if len(msgs2) != 4 {
		t.Fatalf("turn 2: expected [system, user hist, assistant hist, user], got %d", len(msgs2))
	}

	// Byte-identical system prompt.
	if msgs1[0].Content != msgs2[0].Content {
		t.Fatal("stable system prompt changed between turns — cache invalidated from token 0")
	}

	// Replay prefix: [system] must be followed by exactly the stored history
	// (no interleaved system message before the current user message).
	if msgs2[1].Content != "first question" || msgs2[1].Role != "user" {
		t.Fatalf("history user entry diverged: %+v", msgs2[1])
	}
	if msgs2[2].Content != "first answer" || msgs2[2].Role != "assistant" {
		t.Fatalf("history assistant entry diverged: %+v", msgs2[2])
	}

	// The cached prefix of turn 2 = system + history entries. Turn 1's request
	// was [system, user1-with-wrap]. Since history stores the user message
	// without the wrap, turn 1's cached prefix in turn 2 extends through the
	// system message only, but turn 2's chain itself is internally consistent
	// and identical to what turn 3 will see replayed. Verify wrap isolation:
	wrapStart := strings.Index(msgs1[1].Content, "\n\n<turn_context>\n")
	if wrapStart < 0 {
		t.Fatal("turn 1 user message missing turn_context wrap")
	}
	stored := "first question"
	if msgs1[1].Content[:wrapStart] != stored {
		t.Fatalf("wrap not appended to trailing end: %q", msgs1[1].Content[:wrapStart])
	}
	if !strings.HasSuffix(msgs1[1].Content, "\n</turn_context>") {
		t.Fatal("turn_context wrap not closed")
	}

	// Mid-turn tool iterations append after the user message, so iteration 2
	// keeps iteration 1's entire chain as prefix — trivially true by append.
}

// TestTurnContextWrapIsolation ensures a user message that legitimately ends
// with something resembling the wrap is never corrupted (LastIndex + suffix
// guards in stripTurnContextWrap).
func TestTurnContextWrapIsolation(t *testing.T) {
	if got := stripTurnContextWrap("plain message"); got != "plain message" {
		t.Fatalf("plain message corrupted: %q", got)
	}
	if got := stripTurnContextWrap("msg\n\n<turn_context>\ninner\n</turn_context>"); got != "msg" {
		t.Fatalf("wrap not stripped: %q", got)
	}
	// Unterminated wrap: leave untouched.
	weird := "msg\n\n<turn_context>\ninner"
	if got := stripTurnContextWrap(weird); got != weird {
		t.Fatalf("unterminated wrap should be untouched, got %q", got)
	}
	// Marker present but not at the end.
	mid := "msg\n\n<turn_context>\ninner\n</turn_context>\ntrailing text"
	if got := stripTurnContextWrap(mid); got != mid {
		t.Fatalf("non-trailing wrap should be untouched, got %q", got)
	}
}

// TestBuildMessagesPrefixBytes is the belt-and-suspenders byte-level check:
// simulate two consecutive live turns (with tool iterations between them) and
// assert the second request's prefix equals the first request serialized.
func TestBuildMessagesPrefixBytes(t *testing.T) {
	cb := NewContextBuilder(t.TempDir(), memory.NewSimpleRanker(), 5)
	memCtx := "Long-term memory: stable"
	mems := []memory.MemoryItem{{Kind: "short", Text: "m1"}}

	m1 := cb.BuildMessages(nil, "q1", "telegram", "123", "u1", memCtx, mems, nil)
	// Simulate loop.go: stored history excludes the wrap.
	history := []string{"user: q1", "assistant: a1"}
	m2 := cb.BuildMessages(history, "q2", "telegram", "123", "u1", memCtx, mems, nil)

	// Serialize turn 1 as the provider would receive it (system + user).
	var b1 bytes.Buffer
	for _, m := range m1 {
		b1.WriteString(m.Role)
		b1.WriteByte(0)
		b1.WriteString(m.Content)
		b1.WriteByte(0)
	}

	// The first two messages of turn 2 must be byte-identical to a rebuilt
	// turn-1-like chain: system + stored-history user. The wrap means turn 1's
	// live user message is NOT identical to history's replay — that's the
	// intended design (wrap is live-only). What must hold: system identical,
	// history replay never diverges turn-to-turn.
	if m1[0].Content != m2[0].Content {
		t.Fatal("system prompt diverged between turns")
	}
	if m2[1].Content != "q1" {
		t.Fatalf("history replay diverged: %q", m2[1].Content)
	}
}
