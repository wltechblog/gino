package agent

import (
	"log"

	"github.com/wltechblog/gino/internal/providers"
)

// turnTrimHeadroom is subtracted from the emergency-trim cap to derive the
// per-turn char budget: the live turn replays its whole chain once more
// before the next trim point, and this guard's purpose is to make that
// next request fit the net with room for the next response.
const turnTrimHeadroom = 40_000

// turnTrimStub is inserted in place of trimmed ranges so the model knows
// the chain was shortened (same framing as the providers emergency trim).
const turnTrimStub = "[Earlier conversation trimmed automatically to keep the request within the model's context window. Recent messages below carry the current state.]"

// turnTrimCharBudget derives the per-turn char budget from the shared
// emergency-trim cap. 0 means the trim is disabled (maxRequestChars = 0).
func turnTrimCharBudget() int {
	capChars := providers.EffectiveMaxRequestChars()
	if capChars <= 0 {
		return 0
	}
	return capChars - turnTrimHeadroom
}

// turnTrimMsgChars estimates a message's serialized size in characters:
// content plus tool-call arguments (which ride along in the request JSON).
// Mirrors the providers.RequestChars per-message math closely enough for
// budgeting; the emergency net downstream is the exact backstop.
func turnTrimMsgChars(m *providers.Message) int {
	n := len(m.Content) + 16
	for _, img := range m.Images {
		n += len(img) + 32
	}
	for _, tc := range m.ToolCalls {
		n += len(tc.Name) + 16
		for _, v := range tc.Arguments {
			if s, ok := v.(string); ok {
				n += len(s)
			} else {
				n += 16 // non-string scalars contribute a small constant
			}
		}
	}
	return n
}

// trimTurnForTokens is the size-aware second half of the turn-trim guard
// (t-gino-rails-006 item 3). It runs AFTER trimTurnMessages has done its
// structural (count-based) trim — including when that was a no-op because
// the chain is short in MESSAGE COUNT but fat in CHARS (few huge tool
// results), which is exactly the shape that used to reach the provider,
// get hard-rejected, and burn the whole iteration budget.
//
// Behavior:
//   - Gate on estimated serialized chars vs budget (cap - headroom), not
//     on message count, so the count no-op case is covered.
//   - Drop whole OLDEST-FIRST messages (history first, then turn
//     exchanges) until the estimate fits; most recent exchanges survive.
//   - Keep tool pairing valid: an assistant tool_calls message and the
//     tool results answering it are dropped as a unit; any tool result
//     whose caller was dropped is dropped too (strict servers reject
//     orphaned tool messages).
//   - Never drop below system + current user message (the turn must be
//     able to proceed even if the floor alone exceeds the budget — the
//     emergency net then handles content-level clamping).
//   - Never mutates message content; bounded single O(n) pass.
//
// Returns the new userMsgIdx and the trimmed chain.
func trimTurnForTokens(messages []providers.Message, userMsgIdx int) ([]providers.Message, int) {
	budget := turnTrimCharBudget()
	if budget <= 0 || len(messages) == 0 {
		return messages, userMsgIdx
	}

	total := 0
	for i := range messages {
		total += turnTrimMsgChars(&messages[i])
	}
	if total <= budget {
		return messages, userMsgIdx // healthy, nothing to do
	}

	// Floor: system messages and the current user message are never dropped.
	floor := make([]bool, len(messages))
	for i := range messages {
		if messages[i].Role == "system" || i == userMsgIdx {
			floor[i] = true
		}
	}

	// kept starts ALL-TRUE (keep everything); floor messages are protected
	// by the drop() guard below, drop() flips entries to false.
	kept := make([]bool, len(messages))
	for i := range kept {
		kept[i] = true
	}

	// Tool-pair spans in the turn tail: [start, end) covering the
	// assistant tool_calls message plus its contiguous tool results.
	type pairSpan struct{ start, end int }
	var pairs []pairSpan
	for i := userMsgIdx + 1; i < len(messages); i++ {
		if messages[i].Role == "assistant" && len(messages[i].ToolCalls) > 0 {
			j := i + 1
			for j < len(messages) && messages[j].Role == "tool" {
				j++
			}
			pairs = append(pairs, pairSpan{start: i, end: j})
			i = j - 1
		}
	}

	droppedCount := 0
	droppedChars := 0
	remaining := total
	drop := func(i int) {
		if floor[i] || !kept[i] {
			return
		}
		remaining -= turnTrimMsgChars(&messages[i])
		kept[i] = false
		droppedCount++
		droppedChars += turnTrimMsgChars(&messages[i])
	}

	// Oldest-first: session history, then whole turn-tail pairs.
	for i := 1; i < userMsgIdx && remaining > budget; i++ {
		drop(i)
	}
	for _, p := range pairs {
		if remaining <= budget {
			break
		}
		for i := p.start; i < p.end && remaining > budget; i++ {
			drop(i)
		}
	}

	// Repair: drop kept tool results whose calling assistant was dropped.
	for i := userMsgIdx + 1; i < len(messages); i++ {
		if messages[i].Role != "tool" || !kept[i] {
			continue
		}
		orphan := true
		for j := i - 1; j >= userMsgIdx+1; j-- {
			if messages[j].Role == "assistant" && len(messages[j].ToolCalls) > 0 {
				orphan = !kept[j]
				break
			}
		}
		if orphan {
			kept[i] = false
			droppedCount++
			droppedChars += turnTrimMsgChars(&messages[i])
		}
	}

	// Assemble: one stub marks each trimmed gap so the model knows context
	// was shortened (never presented as if the assistant said it).
	out := make([]providers.Message, 0, len(messages))
	newIdx := 0
	stubPending := false
	for i := range messages {
		if kept[i] {
			if stubPending {
				out = append(out, providers.Message{Role: "user", Content: turnTrimStub})
				stubPending = false
			}
			out = append(out, messages[i])
			if i == userMsgIdx {
				newIdx = len(out) - 1
			}
			continue
		}
		stubPending = true
	}
	if stubPending {
		out = append(out, providers.Message{Role: "user", Content: turnTrimStub})
	}

	log.Printf("[turn-trim] estimated %d chars > budget %d (trim cap %d): dropped %d messages (~%d chars), %d -> %d messages — chain pre-shrunk before LLM call",
		total, budget, providers.EffectiveMaxRequestChars(), droppedCount, droppedChars, len(messages), len(out))

	return out, newIdx
}
