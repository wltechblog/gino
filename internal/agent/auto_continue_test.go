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
// models a task that legitimately needs more iterations than one budget
// block — the exact case auto-continue exists for.
type finishAfterProvider struct {
	mu       sync.Mutex
	calls    int
	finishAt int
	sawNote  bool
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

	// Detect the synthetic continuation note among the messages.
	for _, m := range messages {
		if m.Role == "user" && strings.Contains(m.Content, "iteration limit") {
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

func (p *finishAfterProvider) GetDefaultModel() string { return "ac-test" }

func (p *finishAfterProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

// TestProcessDirectAutoContinuesAndFinishes: with a budget of 3 iterations and
// a task that needs 7 tool calls + 1 final reply, the turn must extend its own
// budget and finish — no "continue" round-trip to the caller.
func TestProcessDirectAutoContinuesAndFinishes(t *testing.T) {
	b := chat.NewHub(10)
	prov := &finishAfterProvider{finishAt: 7}
	ag := NewAgentLoop(b, prov, prov.GetDefaultModel(), 3, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")

	resp, err := ag.ProcessDirect("keep working", 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "task complete" {
		t.Fatalf("expected finished reply, got %q", resp)
	}
	if calls := prov.Calls(); calls != 8 {
		t.Fatalf("expected 8 LLM calls (7 tool + 1 final), got %d", calls)
	}
	if !prov.SawNote() {
		t.Fatal("continuation note was never injected into the message chain")
	}
}

// TestProcessDirectAutoContinueCapTerminates: a provider that NEVER finishes
// must still terminate — capped extensions, then the fallback notice.
func TestProcessDirectAutoContinueCapTerminates(t *testing.T) {
	b := chat.NewHub(10)
	prov := &toolLoopProvider{}
	ag := NewAgentLoop(b, prov, prov.GetDefaultModel(), 3, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")

	resp, err := ag.ProcessDirect("keep working", 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp, "Max tool iterations reached") {
		t.Fatalf("expected fallback limit notice after cap, got %q", resp)
	}
	// 3 base + 4 extension blocks of 3 = 15 total iterations.
	if prov.calls != 15 {
		t.Fatalf("expected 15 LLM calls (5 budget blocks of 3), got %d", prov.calls)
	}
}

// TestProcessDirectAutoContinueDisabled: with auto-continue off, the legacy
// behavior holds — exactly maxIterations calls, then the notice.
func TestProcessDirectAutoContinueDisabled(t *testing.T) {
	b := chat.NewHub(10)
	prov := &toolLoopProvider{}
	ag := NewAgentLoop(b, prov, prov.GetDefaultModel(), 3, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	ag.SetAutoContinue(false)

	resp, err := ag.ProcessDirect("keep working", 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp, "Max tool iterations reached") {
		t.Fatalf("expected limit notice, got %q", resp)
	}
	if prov.calls != 3 {
		t.Fatalf("expected exactly 3 LLM calls (no extensions), got %d", prov.calls)
	}
}

// TestHubTurnAutoContinues: the interactive dispatch path (processTurn via
// dispatchMessage) auto-continues too — the user receives the finished reply,
// never the ⏳ notice, and never has to send "continue".
func TestHubTurnAutoContinues(t *testing.T) {
	b := chat.NewHub(10)
	prov := &finishAfterProvider{finishAt: 7}
	ag := NewAgentLoop(b, prov, prov.GetDefaultModel(), 3, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	ag.SetSessionAutoTitle(false) // the auto-titler would add an extra provider call and race the count assertions

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()

	sub := b.Subscribe("cli")
	b.StartRouter(ctx)

	b.In <- chat.Inbound{
		Channel:   "cli",
		ChatID:    "autocont-test",
		SenderID:  "tester",
		Content:   "do a long task",
		Timestamp: time.Now(),
	}

	deadline := time.After(30 * time.Second)
	for {
		select {
		case out := <-sub:
			if strings.Contains(out.Content, "task complete") {
				if !prov.SawNote() {
					t.Fatal("finished without ever injecting a continuation note — test bug or wrong code path")
				}
				if calls := prov.Calls(); calls != 8 {
					t.Fatalf("expected 8 LLM calls, got %d", calls)
				}
				return
			}
			if strings.Contains(out.Content, "⏳") {
				t.Fatalf("iteration-limit notice reached the user: %q", out.Content)
			}
			// tool-activity notifications etc. — ignore
		case <-deadline:
			t.Fatal("turn did not finish within 30s")
		}
	}
}

// TestHubTurnAutoContinueDisabled: legacy behavior on the hub path.
func TestHubTurnAutoContinueDisabled(t *testing.T) {
	b := chat.NewHub(10)
	prov := &finishAfterProvider{finishAt: 7}
	ag := NewAgentLoop(b, prov, prov.GetDefaultModel(), 3, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	ag.SetAutoContinue(false)
	ag.SetSessionAutoTitle(false) // keep provider call count deterministic

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()

	sub := b.Subscribe("cli")
	b.StartRouter(ctx)

	b.In <- chat.Inbound{
		Channel:   "cli",
		ChatID:    "autocont-off",
		SenderID:  "tester",
		Content:   "do a long task",
		Timestamp: time.Now(),
	}

	deadline := time.After(30 * time.Second)
	for {
		select {
		case out := <-sub:
			if strings.Contains(out.Content, "tool-call limit") {
				if calls := prov.Calls(); calls != 3 {
					t.Fatalf("expected exactly 3 LLM calls, got %d", calls)
				}
				return
			}
			if strings.Contains(out.Content, "task complete") {
				t.Fatal("turn finished despite needing 8 calls with auto-continue disabled")
			}
		case <-deadline:
			t.Fatal("limit notice never reached the user within 30s")
		}
	}
}
