package tui

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

// TestIdleSignalReplyDisplaysWithoutPrompt reproduces the reported bug: a
// turn has ended, a signal (background job, async spawn, cron) injects a
// message via hub.In while the TUI sits at the idle prompt, and the agent's
// reply must be displayed immediately — not held in the cliOut buffer until
// the user's next prompt misreads it as its own answer.
func TestIdleSignalReplyDisplaysWithoutPrompt(t *testing.T) {
	ws := t.TempDir()
	buf := &syncBuffer{}
	s := New(config.Config{}, providers.NewStubProvider(), ws, ws)
	s.out = buf

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.startRuntime(ctx)
	defer s.agent.Close()

	// Run one normal turn so the runtime is warm (pump running, agent loop up).
	done := make(chan struct{})
	go func() {
		s.sendMessage(ctx, "warm up")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("warm-up turn never completed")
	}

	// Turn is over; TUI idle. Fire a signal into the hub the way the signal
	// listener does (signal_action metadata, signal: session namespace).
	s.hub.In <- chat.Inbound{
		Channel:   "cli",
		SenderID:  "signal:test-src",
		ChatID:    s.chatID,
		Content:   "[Signal from test-src: job_done] background job finished",
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"signal_action": "job_done",
			"signal_source": "test-src",
			"signal_silent": false,
		},
	}

	// The reply must appear in the output buffer without any new user input.
	deadline := time.After(3 * time.Second)
	for {
		if strings.Contains(buf.String(), "job finished") {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("signal reply not displayed while idle; output=%q", buf.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestSignalReplyDuringTurnDoesNotEndWait verifies that a background reply
// arriving mid-turn is displayed but does NOT satisfy the prompt's wait —
// the final answer to the user's question still arrives separately.
func TestSignalReplyDuringTurnDoesNotEndWait(t *testing.T) {
	ws := t.TempDir()
	buf := &syncBuffer{}

	// Provider answers the user's turn only after seeing the signal reply
	// marker in the conversation is irrelevant — the stub echoes the prompt;
	// instead use a slow provider for the main turn and inject a signal
	// while it is thinking.
	p := newSlowEchoProvider(600 * time.Millisecond)
	s := New(config.Config{}, p, ws, ws)
	s.out = buf

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.startRuntime(ctx)
	defer s.agent.Close()

	done := make(chan struct{})
	go func() {
		s.sendMessage(ctx, "what is the answer")
		close(done)
	}()

	// Wait until the provider is mid-turn, then fire the signal.
	<-p.started

	s.hub.In <- chat.Inbound{
		Channel:   "cli",
		SenderID:  "signal:test-src",
		ChatID:    s.chatID,
		Content:   "[Signal from test-src: job_done] job result xyz",
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"signal_action": "job_done",
			"signal_source": "test-src",
			"signal_silent": false,
		},
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sendMessage never returned")
	}

	if !strings.Contains(buf.String(), "what is the answer") {
		t.Fatalf("final answer to prompt missing; output=%q", buf.String())
	}

	// The signal turn (independent 600ms provider call) may finish slightly
	// after the main turn; the pump displays it once the turn ends. Poll.
	deadline := time.After(3 * time.Second)
	for {
		if strings.Contains(buf.String(), "job result xyz") {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("mid-turn signal reply not displayed; output=%q", buf.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// slowEchoProvider delays its (final) response, exposing a started channel
// so tests can inject traffic mid-turn.
type slowEchoProvider struct {
	delay   time.Duration
	started chan struct{}
}

func newSlowEchoProvider(d time.Duration) *slowEchoProvider {
	return &slowEchoProvider{delay: d, started: make(chan struct{})}
}

func (p *slowEchoProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	select {
	case <-time.After(p.delay):
		return providers.LLMResponse{Content: lastUserContent(messages)}, nil
	case <-ctx.Done():
		return providers.LLMResponse{}, ctx.Err()
	}
}

func (p *slowEchoProvider) GetDefaultModel() string { return "slow-echo-model" }

func (p *slowEchoProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

func lastUserContent(messages []providers.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// TestIsSignalReply pins the metadata-tag contract between agent loop and TUI.
func TestIsSignalReply(t *testing.T) {
	if !isSignalReply(chat.Outbound{Metadata: map[string]interface{}{"signal": true}}) {
		t.Fatal("signal-tagged outbound must classify as signal reply")
	}
	if isSignalReply(chat.Outbound{Metadata: map[string]interface{}{"signal": false}}) {
		t.Fatal("signal=false must not classify as signal reply")
	}
	if isSignalReply(chat.Outbound{}) {
		t.Fatal("untagged outbound (normal prompt answer) must not classify as signal reply")
	}
}

// syncBuffer is a mutex-guarded bytes.Buffer: the pump goroutine writes idle
// arrivals while the test goroutine polls String() — a raw bytes.Buffer
// races under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
