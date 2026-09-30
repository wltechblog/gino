package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// errProvider fails every Chat call — models the terminal turn error: the
// provider already exhausted its internal retries (maxRetries) before the
// loop ever sees the error.
type errProvider struct {
	calls int
}

func (p *errProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	p.calls++
	return providers.LLMResponse{}, errors.New("LLM request failed after 5 retries: context deadline exceeded")
}

func (p *errProvider) GetDefaultModel() string { return "err-model" }

func (p *errProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

func newExitLoop(t *testing.T) (*AgentLoop, *chat.Hub) {
	t.Helper()
	b := chat.NewHub(10)
	ag := NewAgentLoop(b, &errProvider{}, "err-model", 5, t.TempDir(), nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	return ag, b
}

// Terminal turn errors bump the counter ONLY when armed. Unarmed gateways
// (the default) keep running after turn errors — long-lived bots must not
// die because one turn hit a provider outage.
func TestExitOnTurnErrorCount(t *testing.T) {
	// Unarmed hub path: error turn completes, counter stays zero.
	ag, b := newExitLoop(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()
	b.In <- chat.Inbound{Channel: "telegram", ChatID: "gate-1", Content: "hi"}

	// Wait until the failing turn has finished running (no active turn),
	// THEN assert: proves the error path executed without bumping.
	deadline := time.After(5 * time.Second)
	for !ag.Idle() {
		select {
		case <-deadline:
			t.Fatal("turn never completed")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if ag.TurnErrorCount() != 0 {
		t.Fatalf("unarmed loop must not count turn errors: got %d", ag.TurnErrorCount())
	}
}

// The hub path (dispatchMessage → processTurn) must bump the same counter.
func TestExitOnTurnErrorHubPath(t *testing.T) {
	b := chat.NewHub(10)
	ag := NewAgentLoop(b, &errProvider{}, "err-model", 5, t.TempDir(), nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	ag.SetExitOnTurnError(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()

	b.In <- chat.Inbound{Channel: "telegram", ChatID: "1", Content: "hi"}

	deadline := time.After(5 * time.Second)
	for ag.TurnErrorCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("hub-path turn error never counted")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
