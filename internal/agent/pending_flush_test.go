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

// blockingProvider parks every Chat call until released. The first call
// blocks so the test can queue an orphan mid-turn; releasing lets the turn
// finish and the pending flush re-dispatch run.
type blockingProvider struct {
	mu       sync.Mutex
	release  chan struct{}
	prompted []string
}

func (p *blockingProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	p.mu.Lock()
	p.prompted = append(p.prompted, messages[len(messages)-1].Content)
	rel := p.release
	p.mu.Unlock()
	if rel != nil {
		select {
		case <-rel:
		case <-time.After(5 * time.Second):
			return providers.LLMResponse{}, context.DeadlineExceeded
		}
	}
	return providers.LLMResponse{Content: "done"}, nil
}

func (p *blockingProvider) GetDefaultModel() string { return "block-model" }

func (p *blockingProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

func (p *blockingProvider) arm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.release == nil {
		p.release = make(chan struct{})
	}
}

func (p *blockingProvider) letGo() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.release != nil {
		close(p.release)
		p.release = nil
	}
}

func (p *blockingProvider) prompts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompted...)
}

func waitCondPF(t *testing.T, cond func() bool, label string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", label)
}

func newBlockingAgent(t *testing.T, b *chat.Hub, p *blockingProvider) *AgentLoop {
	t.Helper()
	return NewAgentLoop(b, p, p.GetDefaultModel(), 5, t.TempDir(), nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
}

// TestPendingFlushTurnEndRequeue covers the core stale-answer contract:
// input arriving while the final LLM call is in flight must not be answered
// by a FUTURE unrelated turn's context. It is flushed as its own turn whose
// prompt is the queued content itself (not a mid-turn "[Additional input]"
// injection into a stale chain).
func TestPendingFlushTurnEndRequeue(t *testing.T) {
	b := chat.NewHub(8)
	p := &blockingProvider{}
	p.arm()
	ag := newBlockingAgent(t, b, p)
	ag.flushPendingAtTurnEnd = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	b.In <- chat.Inbound{Channel: "telegram", SenderID: "777", ChatID: "555", Content: "first"}
	waitCondPF(t, func() bool { return ag.hasActiveTurn("telegram:555") }, "first turn active")

	// Orphan: lands after the turn's only LLM call is already blocked.
	b.In <- chat.Inbound{Channel: "telegram", SenderID: "777", ChatID: "555", Content: "second"}
	waitCondPF(t, func() bool {
		ag.mu.Lock()
		defer ag.mu.Unlock()
		return len(ag.pending["telegram:555"]) == 1
	}, "orphan queued while turn active")

	p.letGo()
	// Count-agnostic wait: the flush prompt may land at any index because a
	// background auto-title call races it. The flush prompt carries the
	// sender-attribution prefix ("[777]:\nsecond"), so match loosely.
	waitCondPF(t, func() bool {
		for _, pr := range p.prompts() {
			if strings.Contains(pr, "second") {
				return true
			}
		}
		return false
	}, "flush re-dispatch as its own turn")

	// The orphan must appear as a FRESH prompt, not inside the first turn's
	// prompt (stale-answer) and not as a mid-turn "[Additional input...]"
	// injection into it.
	ps := p.prompts()
	if strings.Contains(ps[0], "second") {
		t.Fatalf("first turn's prompt must not contain the orphan (stale-answer): %q", ps[0])
	}
	if strings.Contains(ps[0], "Additional input") {
		t.Fatalf("orphan was injected mid-turn, not flushed as its own prompt: %q", ps[0])
	}

	waitCondPF(t, func() bool { return !ag.hasActiveTurn("telegram:555") }, "flush turn finished")

	// Count-agnostic: background calls (auto-title etc.) may race us. Assert
	// on which prompts ran, not on the total number of LLM calls.
	ps = p.prompts()
	if len(ps) < 2 || !strings.Contains(ps[0], "first") {
		t.Fatalf("first call must prompt the original message, got %q", ps)
	}
	if strings.Contains(ps[0], "second") {
		t.Fatalf("first turn's prompt must not contain the orphan (stale-answer): %q", ps[0])
	}
	foundFlush := false
	for _, pr := range ps {
		if strings.Contains(pr, "Additional input from 777") {
			t.Fatalf("orphan was injected mid-turn, not flushed as its own prompt: %q", pr)
		}
		if strings.Contains(pr, "second") && !strings.Contains(pr, "generate short titles") {
			foundFlush = true
		}
	}
	if !foundFlush {
		t.Fatalf("no fresh turn prompted the orphan itself: %q", ps)
	}

	// Reply routing: everything on hub.Out must address telegram:555 — no
	// cli-channel fallback, no dropped reply.
	for {
		select {
		case out := <-b.Out:
			if out.Channel == "cli" {
				t.Fatalf("flush reply routed to cli channel: %+v", out)
			}
			if out.Channel != "telegram" || out.ChatID != "555" {
				t.Fatalf("reply routed to wrong target: %+v", out)
			}
			continue
		default:
		}
		break
	}

	ag.mu.Lock()
	left := len(ag.pending["telegram:555"])
	ag.mu.Unlock()
	if left != 0 {
		t.Fatalf("pending queue not drained, %d left", left)
	}
}

// TestPendingFlushRoutingKeys pins the channel/chatID reconstruction from
// session keys, including namespaced project keys and colon-bearing chatIDs.
func TestPendingFlushRoutingKeys(t *testing.T) {
	cases := []struct{ key, wantKey string }{
		{"telegram:555", "telegram:555"},
		{"discord:chan:999", "discord:chan:999"},
		{"proj:alpha:telegram:77", "telegram:77"},
		{"signal:cli:1", "signal:cli:1"},
		{"weird", "cli:weird"}, // legacy fallback: no colon
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			b := chat.NewHub(8)
			p := &blockingProvider{}
			p.arm()
			ag := newBlockingAgent(t, b, p)
			ag.flushPendingAtTurnEnd = true

			ag.queuePendingMsg(tc.key, "orphan text", "")
			go ag.flushOrphanedPending(context.Background(), tc.key)
			waitCondPF(t, func() bool { return ag.hasActiveTurn(tc.wantKey) },
				"flush turn dispatched under "+tc.wantKey)
			p.letGo()
			waitCondPF(t, func() bool { return !ag.hasActiveTurn(tc.wantKey) }, "flush turn done")
		})
	}
}

// TestPendingFlushDefaultOn pins the shipped behavior: turn-end flush is
// the DEFAULT (binary config only turns it OFF). Orphans get their own
// prompt instead of being parked for the next unrelated turn.
func TestPendingFlushDefaultOn(t *testing.T) {
	b := chat.NewHub(8)
	p := &blockingProvider{}
	p.arm()
	ag := newBlockingAgent(t, b, p)
	if !ag.flushPendingAtTurnEnd {
		t.Fatal("flushPendingAtTurnEnd must default to true (kill-switch off)")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	b.In <- chat.Inbound{Channel: "telegram", ChatID: "42", Content: "one"}
	waitCondPF(t, func() bool { return ag.hasActiveTurn("telegram:42") }, "turn active")
	b.In <- chat.Inbound{Channel: "telegram", ChatID: "42", Content: "two"}
	waitCondPF(t, func() bool {
		ag.mu.Lock()
		defer ag.mu.Unlock()
		return len(ag.pending["telegram:42"]) == 1
	}, "orphan queued")

	p.letGo()
	waitCondPF(t, func() bool { return !ag.hasActiveTurn("telegram:42") }, "turn finished")

	// Count-agnostic: assert on which prompts ran, not on total calls.
	ps := p.prompts()
	if len(ps) < 1 || !strings.Contains(ps[0], "one") {
		t.Fatalf("first call must prompt the original message, got %q", ps)
	}
	if strings.Contains(ps[0], "two") {
		t.Fatalf("orphan leaked into the first turn (stale-answer): %q", ps[0])
	}
	foundFlush := false
	for _, pr := range ps {
		if strings.HasPrefix(strings.TrimSpace(pr), "two") {
			foundFlush = true
		}
	}
	if !foundFlush {
		t.Fatalf("default flush: orphan must get its own prompt, got %q", ps)
	}
	ag.mu.Lock()
	left := len(ag.pending["telegram:42"])
	ag.mu.Unlock()
	if left != 0 {
		t.Fatalf("queue must be drained after flush, %d left", left)
	}
}
