package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// panickyProvider panics on the FIRST Chat call, then succeeds. Used to
// drive a mid-turn panic through the turn goroutines.
type panickyProvider struct {
	mu       sync.Mutex
	calls    int
	prompted []string
}

func (p *panickyProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	p.mu.Lock()
	p.calls++
	p.prompted = append(p.prompted, messages[len(messages)-1].Content)
	n := p.calls
	p.mu.Unlock()
	if n == 1 {
		panic("provider exploded mid-turn")
	}
	return providers.LLMResponse{Content: "recovered reply"}, nil
}

func (p *panickyProvider) GetDefaultModel() string { return "panic-model" }

func (p *panickyProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

func (p *panickyProvider) prompts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompted...)
}

// waitPanic tracks the panic-recovery contract: after a panic the session
// must return to idle and the loop must keep dispatching.
func waitPanic(t *testing.T, cond func() bool, label string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", label)
}

// newPanicAgent constructs a loop with the shared-provider pattern used by
// pending_flush_test.go (workspace = t.TempDir for hermeticity).
func newPanicAgent(t *testing.T, b *chat.Hub, p *panickyProvider) *AgentLoop {
	t.Helper()
	return NewAgentLoop(b, p, p.GetDefaultModel(), 5, t.TempDir(), nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
}

// TestPanicDoesNotWedgeSession covers the FIX 1 contract on the paused-turn
// goroutine path: a panicking provider must NOT leave the active-turn entry
// behind (which would park the session "busy" forever and block all future
// turns + pending-queue growth). After recovery the same session must accept
// and complete a new turn.
func TestPanicDoesNotWedgeSession(t *testing.T) {
	b := chat.NewHub(8)
	p := &panickyProvider{}
	ag := newPanicAgent(t, b, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	b.In <- chat.Inbound{Channel: "telegram", SenderID: "777", ChatID: "555", Content: "cause a panic"}
	waitPanic(t, func() bool { return !ag.hasActiveTurn("telegram:555") && len(p.prompts()) >= 1 }, "first turn done after panic")

	// The loop must still be alive: a follow-up turn runs to completion
	// (pre-fix this hung forever: the active entry leaked, so the follow-up
	// was queued with "📥 Message queued" and never processed).
	b.In <- chat.Inbound{Channel: "telegram", SenderID: "777", ChatID: "555", Content: "are you still there?"}
	waitPanic(t, func() bool {
		for _, pr := range p.prompts() {
			if strings.Contains(pr, "still there") {
				return true
			}
		}
		return false
	}, "follow-up turn dispatched after panic")
	waitPanic(t, func() bool { return !ag.hasActiveTurn("telegram:555") }, "follow-up turn finished")
}

// TestSignalPanicFlushesPending covers FIX 1 on the signal-turn goroutine:
// a panic during a signal turn must still release the pending user message
// that queued while the signal ran. Pre-fix, the panic skipped the turn-end
// cleanup entirely and the queued message stayed orphaned until some future
// unrelated turn drained it as a mid-turn "[Additional input...]" injection
// (the stale-answer treadmill).
func TestSignalPanicFlushesPending(t *testing.T) {
	b := chat.NewHub(8)
	p := &panickyProvider{}
	ag := newPanicAgent(t, b, p)
	ag.flushPendingAtTurnEnd = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	// A signal turn (metadata signal_action) runs under signal:telegram:42
	// and panics on the first provider call — within milliseconds, so we do
	// NOT wait for the active flag (racy); we wait for evidence the turn
	// ran (its prompt recorded).
	b.In <- chat.Inbound{Channel: "telegram", SenderID: "777", ChatID: "42", Content: "signal work", Metadata: map[string]interface{}{"signal_action": "noop"}}
	waitPanic(t, func() bool {
		for _, pr := range p.prompts() {
			if strings.Contains(pr, "signal work") {
				return true
			}
		}
		return false
	}, "signal turn ran and panicked")
	b.In <- chat.Inbound{Channel: "telegram", SenderID: "777", ChatID: "42", Content: "user followup after signal panic"}

	waitPanic(t, func() bool {
		for _, pr := range p.prompts() {
			if strings.Contains(pr, "user followup") && !strings.Contains(pr, "Additional input") {
				return true
			}
		}
		return false
	}, "orphaned user message got its own turn after signal panic")

	waitPanic(t, func() bool {
		return !ag.hasActiveTurn("signal:telegram:42") && !ag.hasActiveTurn("telegram:42")
	}, "all turns finished")
}
