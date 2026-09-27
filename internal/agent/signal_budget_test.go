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

// signalBudgetProvider behaves like a turn that keeps issuing tool calls
// until a specific LLM call number. It counts calls and classifies signal
// vs interactive turns by the Signal-handling prompt shape so background
// LLM calls (auto-titler, turn-extract — no tools present) don't race the
// count. It records whether the wrap-up note was injected and whether any
// raw "continue" ever reached the model.
type signalBudgetProvider struct {
	mu        sync.Mutex
	calls     int
	finishAt  int // finish (final text reply) when calls > finishAt
	sawWrapUp bool
	neverDone bool // true = never produce a final reply (hard-cap path)
}

func (p *signalBudgetProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	if len(tools) == 0 {
		return providers.LLMResponse{Content: "title"}, nil
	}
	p.mu.Lock()
	p.calls++
	calls := p.calls
	p.mu.Unlock()
	for _, m := range messages {
		if strings.Contains(m.Content, "budget for this background task is nearly exhausted") {
			p.mu.Lock()
			p.sawWrapUp = true
			p.mu.Unlock()
		}
	}
	if p.neverDone {
		tc := providers.ToolCall{ID: fmt.Sprintf("c%d", calls), Name: "exec", Arguments: map[string]interface{}{"cmd": "true"}}
		return providers.LLMResponse{HasToolCalls: true, ToolCalls: []providers.ToolCall{tc}}, nil
	}
	if calls > p.finishAt {
		return providers.LLMResponse{Content: "task complete"}, nil
	}
	tc := providers.ToolCall{ID: fmt.Sprintf("c%d", calls), Name: "exec", Arguments: map[string]interface{}{"cmd": "true"}}
	return providers.LLMResponse{HasToolCalls: true, ToolCalls: []providers.ToolCall{tc}}, nil
}

func (p *signalBudgetProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *signalBudgetProvider) SawWrapUp() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sawWrapUp
}

func (p *signalBudgetProvider) GetDefaultModel() string { return "sb-test" }

func (p *signalBudgetProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

func newSignalBudgetLoop(t *testing.T, p providers.LLMProvider, maxIter, blocks int) (*AgentLoop, *chat.Hub) {
	t.Helper()
	b := chat.NewHub(16)
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), maxIter, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	if blocks > 0 {
		ag.SetSignalBudgetBlocks(blocks)
	}
	ag.SetSessionAutoTitle(false)
	t.Cleanup(func() { ag.Close() })
	return ag, b
}

// sendBudgetSignal injects a signal message and collects outbound until the
// turn finishes (poll: no new output for 700ms).
func sendBudgetSignal(t *testing.T, b *chat.Hub) []chat.Outbound {
	t.Helper()
	meta := map[string]interface{}{
		"signal_source": "agentchat",
		"signal_action": "check_messages",
		"signal_silent": false,
	}
	select {
	case b.In <- chat.Inbound{Channel: "telegram", SenderID: "signal:agentchat", ChatID: "222", Content: "[New messages]", Metadata: meta}:
	default:
		t.Fatal("hub input full")
	}
	var outs []chat.Outbound
	deadline := time.After(10 * time.Second)
	for {
		var done bool
		select {
		case out := <-b.Out:
			outs = append(outs, out)
		case <-time.After(700 * time.Millisecond):
			done = true
		case <-deadline:
			done = true
		}
		if done {
			return outs
		}
	}
}

// TestSignalTurnExtendsBudgetAndFinishes: a signal task that needs more
// than one maxIterations block must run through its extended budget
// (blocks x maxIterations) and finish — never pause with a ⏳ notice that
// nobody can answer.
func TestSignalTurnExtendsBudgetAndFinishes(t *testing.T) {
	p := &signalBudgetProvider{finishAt: 7}
	ag, b := newSignalBudgetLoop(t, p, 3, 3) // budget = 9, finish at call 8

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	outs := sendBudgetSignal(t, b)
	if p.Calls() != 8 {
		t.Fatalf("expected 8 LLM calls (7 tool + 1 finish), got %d", p.Calls())
	}
	for _, out := range outs {
		if strings.Contains(out.Content, "Reply **continue**") {
			t.Fatalf("signal turn paused with the interactive ⏳ notice: %q", out.Content)
		}
	}
}

// TestSignalTurnWrapUpPass: a signal turn that exhausts even its extended
// budget must get the wrap-up note injected (stop working, send your result
// to the assigner) — and the wrap-up reply is the final reply.
func TestSignalTurnWrapUpPass(t *testing.T) {
	// never finishes on its own during the main budget; wrap-up note sets
	// wrapUpDone and the provider sees it and returns a final reply.
	p := &wrapUpFinishProvider{}
	ag, b := newSignalBudgetLoop(t, p, 2, 1) // budget = 2, then wrap-up window = maxIter/4 >= 5 → 5

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	outs := sendBudgetSignal(t, b)
	if !p.SawWrapUp() {
		t.Fatal("wrap-up note was never injected")
	}
	found := false
	for _, out := range outs {
		if strings.Contains(out.Content, "wrap-up summary sent") {
			found = true
		}
	}
	if !found {
		t.Fatalf("wrap-up reply not delivered; outs = %+v", outs)
	}
}

// wrapUpFinishProvider tool-calls forever until it sees the wrap-up note,
// then returns a final reply — the cooperative wrap-up shape.
type wrapUpFinishProvider struct {
	signalBudgetProvider
}

func (p *wrapUpFinishProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	if len(tools) == 0 {
		return providers.LLMResponse{Content: "title"}, nil
	}
	for _, m := range messages {
		if strings.Contains(m.Content, "budget for this background task is nearly exhausted") {
			p.mu.Lock()
			p.sawWrapUp = true
			p.mu.Unlock()
			return providers.LLMResponse{Content: "wrap-up summary sent"}, nil
		}
	}
	p.mu.Lock()
	p.calls++
	calls := p.calls
	p.mu.Unlock()
	tc := providers.ToolCall{ID: fmt.Sprintf("c%d", calls), Name: "exec", Arguments: map[string]interface{}{"cmd": "true"}}
	return providers.LLMResponse{HasToolCalls: true, ToolCalls: []providers.ToolCall{tc}}, nil
}

// TestSignalTurnHardCap: a signal turn that burns even the wrap-up window
// without a final reply must end with the synthesized hard-cap reply —
// bounded burn, no orphaned pause stash, no infinite loop.
func TestSignalTurnHardCap(t *testing.T) {
	p := &signalBudgetProvider{neverDone: true}
	ag, b := newSignalBudgetLoop(t, p, 2, 2) // budget = 4, wrap-up = 5 → total ≤ 9

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	outs := sendBudgetSignal(t, b)
	if p.Calls() > 9 {
		t.Fatalf("hard cap failed: %d LLM calls (budget 4 + wrap-up 5)", p.Calls())
	}
	found := false
	for _, out := range outs {
		if strings.Contains(out.Content, "hard tool-call budget") {
			found = true
		}
	}
	if !found {
		t.Fatalf("synthesized hard-cap reply missing; outs = %+v", outs)
	}
	// The pause stash must NOT hold an orphaned signal turn.
	ag.mu.Lock()
	stashed := len(ag.paused)
	ag.mu.Unlock()
	if stashed != 0 {
		t.Fatalf("signal turn left %d orphaned pause stash(es)", stashed)
	}
}

// TestInteractiveTurnStillPauses: interactive turns keep the exact
// pause-and-resume contract — the fix must not change them.
func TestInteractiveTurnStillPauses(t *testing.T) {
	p := &finishAfterProvider{finishAt: 100} // never finishes
	ag, b := newSignalBudgetLoop(t, p, 3, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	select {
	case b.In <- chat.Inbound{Channel: "telegram", SenderID: "u1", ChatID: "333", Content: "do a big task"}:
	default:
		t.Fatal("hub input full")
	}
	var outs []chat.Outbound
	deadline := time.After(10 * time.Second)
	sawPause := false
	for !sawPause {
		var done bool
		select {
		case out := <-b.Out:
			outs = append(outs, out)
			if strings.Contains(out.Content, "Reply **continue**") {
				sawPause = true
			}
		case <-time.After(700 * time.Millisecond):
			done = true
		case <-deadline:
			done = true
		}
		if done {
			t.Fatalf("interactive turn never paused; outs = %+v", outs)
		}
	}
	// Paused stash exists for the bare session key (interactive path unchanged).
	ag.mu.Lock()
	_, ok := ag.paused["telegram:333"]
	ag.mu.Unlock()
	if !ok {
		t.Fatal("interactive pause stash missing under bare session key")
	}
}

// TestSignalTurnNeverStashes: signal turns must never leave an entry in
// the pause map under ANY key — the field incident shape (stash under
// signal: key, unresumable by the bare-key continue probe) is now
// structurally impossible because signal turns never take the pause path.
func TestSignalTurnNeverStashes(t *testing.T) {
	p := &signalBudgetProvider{neverDone: true}
	ag, b := newSignalBudgetLoop(t, p, 2, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	outs := sendBudgetSignal(t, b)
	_ = outs // hard-cap reply asserted elsewhere; here we only care about stash state

	ag.mu.Lock()
	stashed := len(ag.paused)
	ag.mu.Unlock()
	if stashed != 0 {
		t.Fatalf("signal turn left %d pause stash(es) under any key", stashed)
	}
}
