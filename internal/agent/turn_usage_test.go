package agent

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// analyticsProvider finishes immediately and stamps a fake served model +
// duration so the TURN USAGE record can be asserted end-to-end.
type analyticsProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *analyticsProvider) Chat(_ context.Context, _ []providers.Message, tools []providers.ToolDefinition, _ string) (providers.LLMResponse, error) {
	if len(tools) == 0 {
		return providers.LLMResponse{Content: "title"}, nil // background calls (auto-titler)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return providers.LLMResponse{
		Content:    "done",
		Model:      "fake-served-model",
		DurationMS: 42,
		Usage: &providers.Usage{
			PromptTokens:     100,
			CompletionTokens: 5,
			TotalTokens:      105,
		},
	}, nil
}

func (p *analyticsProvider) GetDefaultModel() string { return "test-model" }
func (p *analyticsProvider) GetModelContext(_ context.Context, _ string) (int, error) {
	return 0, nil
}

// TestTurnUsageIncludesModelAndDurations pins the analytics contract: every
// TURN USAGE record names the model that served the turn and both wall-clock
// and LLM-only durations.
func TestTurnUsageIncludesModelAndDurations(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	b := chat.NewHub(64)
	p := &analyticsProvider{}
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 50, t.TempDir(), nil, nil, nil, nil, nil, config.Paths{}, config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	ag.SetAnalytics(true)
	ag.SetSessionAutoTitle(false)
	defer ag.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	select {
	case b.In <- chat.Inbound{Channel: "telegram", SenderID: "u1", ChatID: "333", Content: "say done"}:
	default:
		t.Fatal("hub input full")
	}

	deadline := time.After(30 * time.Second)
	for {
		var done bool
		select {
		case out := <-b.Out:
			if strings.Contains(out.Content, "done") {
				done = true
			}
		case <-time.After(2 * time.Second):
			done = true // drain window closed; check logs now
		case <-deadline:
			t.Fatal("turn never completed")
		}
		if done {
			break
		}
	}

	out := logs.String()
	idx := strings.Index(out, "TURN USAGE")
	if idx < 0 {
		t.Fatalf("TURN USAGE line missing:\n%s", out)
	}
	jsonStr := out[idx:]
	start := strings.Index(jsonStr, "{")
	if start < 0 {
		t.Fatalf("TURN USAGE payload has no JSON object:\n%s", jsonStr)
	}
	end := strings.IndexByte(jsonStr[start:], '\n')
	if end < 0 {
		end = len(jsonStr)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr[start:start+end]), &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload["model"] != "fake-served-model" {
		t.Errorf("model = %v, want fake-served-model", payload["model"])
	}
	if v, ok := payload["llmDurationMs"].(float64); !ok || v != 42 {
		t.Errorf("llmDurationMs = %v, want 42", payload["llmDurationMs"])
	}
	if v, ok := payload["turnDurationMs"].(float64); !ok || v < 0 {
		t.Errorf("turnDurationMs = %v, want a non-negative number", payload["turnDurationMs"])
	}
	if payload["channel"] != "telegram" {
		t.Errorf("channel = %v, want telegram", payload["channel"])
	}
	if v, ok := payload["iterations"].(float64); !ok || v < 1 {
		t.Errorf("iterations = %v, want >= 1", payload["iterations"])
	}
}
