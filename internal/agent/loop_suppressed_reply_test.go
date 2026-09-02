package agent

import (
	"context"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// lateSuccessProvider models the production race that orphaned TUI replies:
// the TUI response timeout fires while the provider call is in flight, but the
// call "completes anyway" (the HTTP response already finished server-side).
// It blocks until the turn context is cancelled, then returns SUCCESS.
type lateSuccessProvider struct {
	chatCalled chan struct{}
}

func (p *lateSuccessProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	close(p.chatCalled)
	<-ctx.Done()
	// The request had already completed server-side: return a real reply,
	// not an error. This is the exact shape of the 2026-09-02 incident.
	return providers.LLMResponse{Content: "late reply nobody is waiting for"}, nil
}

func (p *lateSuccessProvider) GetDefaultModel() string { return "late-model" }

func (p *lateSuccessProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

// TestStoppedTurnDoesNotQueueReply guards the reply-suppression fix: when a
// turn is stopped (TUI timeout, /stop) but the in-flight provider call returns
// a successful response anyway, the reply must NOT be queued on hub.Out.
// Previously it was queued with no reader, and the TUI misprinted it as the
// response to the NEXT prompt.
func TestStoppedTurnDoesNotQueueReply(t *testing.T) {
	b := chat.NewHub(10)
	p := &lateSuccessProvider{chatCalled: make(chan struct{})}

	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 5, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()

	sub := b.Subscribe("test")
	b.StartRouter(ctx)

	b.In <- chat.Inbound{
		Channel:   "cli",
		ChatID:    "race-test",
		SenderID:  "tester",
		Content:   "trigger the race",
		Timestamp: time.Now(),
	}
	sessionKey := "cli:race-test"

	select {
	case <-p.chatCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("provider never called; turn did not start")
	}

	// Stop the turn — mirrors the TUI response timeout / /stop.
	if !ag.StopTurn(sessionKey) {
		t.Fatal("StopTurn reported no active turn")
	}

	// The turn goroutine has now exited (StopTurn waits for at.done). Its
	// provider call returned a successful response AFTER the stop landed —
	// the old code would still have queued that reply. Assert it did not.
	select {
	case out, ok := <-sub:
		if !ok {
			t.Fatal("subscriber closed")
		}
		if !isAgentNotification(out) {
			t.Fatalf("stopped turn queued a reply: %q", out.Content)
		}
		// Tagged notifications (e.g. ⛔ Stopped.) are fine — skip them and
		// keep draining briefly to be sure no reply follows.
		deadline := time.After(300 * time.Millisecond)
	drain:
		for {
			select {
			case out2, ok := <-sub:
				if !ok {
					break drain
				}
				if !isAgentNotification(out2) {
					t.Fatalf("stopped turn queued a reply after notification: %q", out2.Content)
				}
			case <-deadline:
				break drain
			}
		}
	case <-time.After(500 * time.Millisecond):
		// Nothing arrived — the desired outcome (or only notifications,
		// which we never received here). Pass.
	}
}

func isAgentNotification(out chat.Outbound) bool {
	if out.Metadata != nil {
		if v, ok := out.Metadata["notification"].(bool); ok {
			return v
		}
	}
	return false
}
