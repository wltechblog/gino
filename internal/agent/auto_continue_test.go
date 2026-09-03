package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// finishAfterProvider issues tool calls for its first finishAt invocations,
// then returns a final text reply. Combined with a small maxIterations it
// models a task that legitimately needs more than one budget block — the
// exact case the pause/resume flow exists for.
type finishAfterProvider struct {
	mu       sync.Mutex
	calls    int
	finishAt int
	sawNote  bool
	rawCont  bool // saw a bare "continue" as a user message (must stay false)
}

func (p *finishAfterProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	// Background LLM calls (turn-extract memory, session auto-title) share
	// this provider but carry no tool definitions. They race the turn's call
	// count, so only turn-loop calls (tools present) are counted.
	if len(tools) == 0 {
		return providers.LLMResponse{Content: ""}, nil
	}
	p.mu.Lock()
	p.calls++
	calls := p.calls
	p.mu.Unlock()

	// The user's "continue" must be intercepted by the harness, never sent
	// to the LLM as a raw prompt. The synthetic resume note is user-role
	// too — distinguish by exact content match.
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		// BuildMessages appends the <turn_context> wrap after the user
		// content, so a genuine prompt starts with the word. The resume
		// note starts with "[System:" — prefix match separates them.
		if strings.HasPrefix(strings.TrimSpace(m.Content), "continue") && !strings.HasPrefix(strings.TrimSpace(m.Content), "[System:") {
			p.mu.Lock()
			p.rawCont = true
			p.mu.Unlock()
		}
		if strings.Contains(m.Content, `user replied "continue"`) {
			p.mu.Lock()
			p.sawNote = true
			p.mu.Unlock()
		}
	}

	if calls > p.finishAt {
		return providers.LLMResponse{Content: "task complete"}, nil
	}
	tc := providers.ToolCall{ID: fmt.Sprintf("c%d", calls), Name: "exec", Arguments: map[string]interface{}{"query": "anything"}}
	return providers.LLMResponse{HasToolCalls: true, ToolCalls: []providers.ToolCall{tc}}, nil
}

func (p *finishAfterProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *finishAfterProvider) SawNote() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sawNote
}

func (p *finishAfterProvider) RawContinueSeen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rawCont
}

func (p *finishAfterProvider) GetDefaultModel() string { return "ac-test" }

func (p *finishAfterProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

func newPauseTestAgent(b *chat.Hub, prov providers.LLMProvider, maxIter int) *AgentLoop {
	ag := NewAgentLoop(b, prov, prov.GetDefaultModel(), maxIter, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	ag.SetSessionAutoTitle(false) // auto-titler would add extra provider calls and race counts
	return ag
}

// waitForReply polls the subscription until a message matching predicate
// arrives or the deadline expires.
func waitForReply(t *testing.T, sub <-chan chat.Outbound, match func(chat.Outbound) bool, what string) chat.Outbound {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case out := <-sub:
			if match(out) {
				return out
			}
		case <-deadline:
			t.Fatalf("%s never arrived within 30s", what)
			return chat.Outbound{}
		}
	}
}

// TestHubTurnPausesAtLimit: a task needing more iterations than the budget
// ends with the ⏳ notice (user gating preserved) AND stashes the live turn.
func TestHubTurnPausesAtLimit(t *testing.T) {
	b := chat.NewHub(10)
	prov := &toolLoopProvider{}
	ag := newPauseTestAgent(b, prov, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()

	sub := b.Subscribe("cli")
	b.StartRouter(ctx)

	b.In <- chat.Inbound{Channel: "cli", ChatID: "pause-1", SenderID: "tester", Content: "do a long task", Timestamp: time.Now()}

	out := waitForReply(t, sub, func(o chat.Outbound) bool { return strings.Contains(o.Content, "tool-call limit") }, "⏳ limit notice")
	if !strings.Contains(out.Content, "continue") {
		t.Fatalf("notice must tell the user how to resume: %q", out.Content)
	}

	// The turn must now be parked in the paused map under the session key.
	deadline := time.After(5 * time.Second)
	for {
		ag.mu.Lock()
		_, ok := ag.paused["cli:pause-1"]
		ag.mu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("turn not stashed in paused map after limit notice")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if prov.calls != 3 {
		t.Fatalf("expected exactly 3 LLM calls (one budget block), got %d", prov.calls)
	}
}

// TestHubTurnResumeOnContinue: the full loop — pause at limit, user replies
// "continue", harness resumes the stashed turn, which then finishes. The word
// itself must never reach the LLM as a raw user prompt.
func TestHubTurnResumeOnContinue(t *testing.T) {
	b := chat.NewHub(10)
	// finishAt=5 with budget 3: block 1 runs calls 1-3 (all tool calls),
	// pause; one resume block runs calls 4,5 (tool) and 6 (final reply).
	prov := &finishAfterProvider{finishAt: 5}
	ag := newPauseTestAgent(b, prov, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()

	sub := b.Subscribe("cli")
	b.StartRouter(ctx)

	b.In <- chat.Inbound{Channel: "cli", ChatID: "resume-1", SenderID: "tester", Content: "do a long task", Timestamp: time.Now()}
	waitForReply(t, sub, func(o chat.Outbound) bool { return strings.Contains(o.Content, "tool-call limit") }, "⏳ limit notice")

	b.In <- chat.Inbound{Channel: "cli", ChatID: "resume-1", SenderID: "tester", Content: "continue", Timestamp: time.Now()}

	out := waitForReply(t, sub, func(o chat.Outbound) bool { return strings.Contains(o.Content, "task complete") }, "finished reply after resume")
	_ = out

	if !prov.SawNote() {
		t.Fatal("resume note never reached the LLM — turn was rebuilt from scratch instead of resumed")
	}
	if prov.RawContinueSeen() {
		t.Fatal(`raw "continue" prompt reached the LLM — interception failed`)
	}
	// 3 (block one) + 3 (resume block, finishing at call 6) = 6 calls total.
	if calls := prov.Calls(); calls != 6 {
		t.Fatalf("expected 6 LLM calls across both blocks, got %d", calls)
	}
}

// TestHubTurnNewMessageDropsPause: a follow-up message that is NOT
// "continue" supersedes the paused turn — no resume, stash cleared.
func TestHubTurnNewMessageDropsPause(t *testing.T) {
	b := chat.NewHub(10)
	prov := &toolLoopProvider{}
	ag := newPauseTestAgent(b, prov, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()

	sub := b.Subscribe("cli")
	b.StartRouter(ctx)

	b.In <- chat.Inbound{Channel: "cli", ChatID: "drop-1", SenderID: "tester", Content: "do a long task", Timestamp: time.Now()}
	waitForReply(t, sub, func(o chat.Outbound) bool { return strings.Contains(o.Content, "tool-call limit") }, "⏳ limit notice")

	// Wait for the stash to land.
	deadline := time.After(5 * time.Second)
	for {
		ag.mu.Lock()
		_, ok := ag.paused["cli:drop-1"]
		ag.mu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("turn not stashed")
		case <-time.After(20 * time.Millisecond):
		}
	}

	b.In <- chat.Inbound{Channel: "cli", ChatID: "drop-1", SenderID: "tester", Content: "actually, do something else", Timestamp: time.Now()}
	waitForReply(t, sub, func(o chat.Outbound) bool { return strings.Contains(o.Content, "tool-call limit") }, "second ⏳ notice")

	// The new turn ran to ITS limit; the old stash was replaced by the new
	// one. Total calls: 3 + 3 = 6 — the paused turn was NOT resumed first.
	if prov.calls != 6 {
		t.Fatalf("expected 6 LLM calls (two independent budget blocks, no resume), got %d", prov.calls)
	}
}

// TestHubTurnStopDiscardsPause: /stop drops the stash with a clear notice.
func TestHubTurnStopDiscardsPause(t *testing.T) {
	b := chat.NewHub(10)
	prov := &toolLoopProvider{}
	ag := newPauseTestAgent(b, prov, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()

	sub := b.Subscribe("cli")
	b.StartRouter(ctx)

	b.In <- chat.Inbound{Channel: "cli", ChatID: "stop-1", SenderID: "tester", Content: "do a long task", Timestamp: time.Now()}
	waitForReply(t, sub, func(o chat.Outbound) bool { return strings.Contains(o.Content, "tool-call limit") }, "⏳ limit notice")

	deadline := time.After(5 * time.Second)
	for {
		ag.mu.Lock()
		_, ok := ag.paused["cli:stop-1"]
		ag.mu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("turn not stashed")
		case <-time.After(20 * time.Millisecond):
		}
	}

	b.In <- chat.Inbound{Channel: "cli", ChatID: "stop-1", SenderID: "tester", Content: "/stop", Timestamp: time.Now()}
	waitForReply(t, sub, func(o chat.Outbound) bool { return strings.Contains(o.Content, "Paused turn discarded") }, "discard notice")

	ag.mu.Lock()
	_, still := ag.paused["cli:stop-1"]
	ag.mu.Unlock()
	if still {
		t.Fatal("paused stash survived /stop")
	}
}

// TestContinueWithoutPauseFallsThrough: "continue" with no paused turn acts
// as an ordinary prompt (dispatch returns without swallowing it).
func TestContinueWithoutPauseFallsThrough(t *testing.T) {
	b := chat.NewHub(10)
	prov := &finishAfterProvider{finishAt: 0} // answers immediately
	ag := newPauseTestAgent(b, prov, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()

	sub := b.Subscribe("cli")
	b.StartRouter(ctx)

	b.In <- chat.Inbound{Channel: "cli", ChatID: "fall-1", SenderID: "tester", Content: "continue", Timestamp: time.Now()}
	out := waitForReply(t, sub, func(o chat.Outbound) bool { return strings.Contains(o.Content, "task complete") }, "ordinary reply")
	_ = out
	// It reached the LLM as a normal prompt (raw "continue" seen) because
	// there was nothing to resume.
	if !prov.RawContinueSeen() {
		t.Fatal(`"continue" with no paused turn should fall through as an ordinary prompt`)
	}
}
