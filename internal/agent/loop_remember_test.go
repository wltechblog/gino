package agent

import (
	"context"
	"testing"
	"time"

	"strings"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/providers"
	"github.com/wltechblog/gino/internal/config"
)

// Provider that fails the test if called (ensures remember shortcut skips provider)
type FailingProvider struct{}

func (f *FailingProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	panic("Chat should not be called when handling remember messages")
}
func (f *FailingProvider) GetDefaultModel() string { return "fail" }

func TestAgentRemembersToday(t *testing.T) {
	b := chat.NewHub(10)
	p := &FailingProvider{}
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 5, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go ag.Run(ctx)

	in := chat.Inbound{Channel: "cli", SenderID: "user", ChatID: "one", Content: "Remember to buy milk"}
	select {
	case b.In <- in:
	default:
		t.Fatalf("couldn't send inbound")
	}

	deadline := time.After(1 * time.Second)
	for {
		select {
		case out := <-b.Out:
			if out.Content == "OK, I've remembered that." {
				// success; verify today's file contains the note
				memCtx, _ := ag.memory.ReadToday()
				if memCtx == "" || !strings.Contains(memCtx, "buy milk") {
					t.Fatalf("expected today's memory to contain 'buy milk', got %q", memCtx)
				}
				return
			}
		case <-deadline:
			t.Fatalf("timeout waiting for remember confirmation")
		}
	}
}
