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

// hallucinatingProvider always emits a tool call to a nonexistent tool
// (shaped like the 2026-09-21 field incident: the model emitted the literal
// string "exec → ok" — a summary LINE — as a tool name).
type hallucinatingProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *hallucinatingProvider) Chat(_ context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string) (providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return providers.LLMResponse{
		ToolCalls: []providers.ToolCall{{
			ID:   fmt.Sprintf("call-%d", p.calls),
			Name: "exec → ok",
			Arguments: map[string]interface{}{
				"cmd": []interface{}{"true"},
			},
		}},
		HasToolCalls: true,
	}, nil
}

func (p *hallucinatingProvider) GetDefaultModel() string { return "test-model" }
func (p *hallucinatingProvider) GetModelContext(_ context.Context, _ string) (int, error) {
	return 0, nil
}

// TestToolNotFoundCircuitBreaker pins the anti-loop behavior: a model that
// repeatedly calls a nonexistent tool must NOT burn the full iteration
// budget. The turn aborts after 5 consecutive not-found failures.
func TestToolNotFoundCircuitBreaker(t *testing.T) {
	b := chat.NewHub(64)
	p := &hallucinatingProvider{}
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 50, t.TempDir(), nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	defer ag.Close()

	done := make(chan string, 1)
	go func() {
		out, _ := ag.ProcessDirect("do the thing", 30*time.Second)
		done <- out
	}()
	select {
	case out := <-done:
		if !strings.Contains(out, "tool that doesn't exist") {
			t.Fatalf("expected breaker message, got: %q", out)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("turn never completed — circuit breaker did not trip")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// 5 consecutive not-found failures trip the breaker.
	if p.calls > 6 {
		t.Fatalf("expected ≤6 LLM calls (5 failures + margin), got %d", p.calls)
	}
}
