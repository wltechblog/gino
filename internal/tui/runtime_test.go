package tui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

type blockingProvider struct {
	started   chan struct{}
	cancelled chan error
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{
		started:   make(chan struct{}),
		cancelled: make(chan error, 1),
	}
}

func (p *blockingProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	<-ctx.Done()
	p.cancelled <- ctx.Err()
	return providers.LLMResponse{}, ctx.Err()
}

func (p *blockingProvider) GetDefaultModel() string { return "blocking-model" }

func (p *blockingProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

func TestStartRuntimeConsumesInboundMessages(t *testing.T) {
	ws := t.TempDir()
	s := New(config.Config{}, providers.NewStubProvider(), ws, ws)
	s.out = io.Discard

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cliOut := s.startRuntime(ctx)
	defer s.agent.Close()

	s.hub.In <- chat.Inbound{
		Channel:   "cli",
		SenderID:  "tui-user",
		ChatID:    s.chatID,
		Content:   "hello from tui test",
		Timestamp: time.Now(),
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case out, ok := <-cliOut:
			if !ok {
				t.Fatal("cli subscriber closed before a response arrived")
			}
			if isActivityNotification(out.Content) {
				continue
			}
			if !strings.Contains(out.Content, "hello from tui test") {
				t.Fatalf("unexpected response %q", out.Content)
			}
			return
		case <-deadline:
			t.Fatal("agent loop did not consume the inbound TUI message")
		}
	}
}

func TestSendMessageReceivesStubResponse(t *testing.T) {
	ws := t.TempDir()
	var buf bytes.Buffer
	s := New(config.Config{}, providers.NewStubProvider(), ws, ws)
	s.out = &buf

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cliOut := s.startRuntime(ctx)
	defer s.agent.Close()

	done := make(chan struct{})
	go func() {
		s.sendMessage(ctx, cliOut, "direct tui works")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sendMessage did not return; agent loop is likely not running")
	}

	if !strings.Contains(buf.String(), "direct tui works") {
		t.Fatalf("expected stub echo in TUI output, got %q", buf.String())
	}
}

func TestSendMessageTimeoutCancelsActiveTurn(t *testing.T) {
	ws := t.TempDir()
	p := newBlockingProvider()
	var buf bytes.Buffer
	s := New(config.Config{}, p, ws, ws)
	s.out = &buf
	s.responseWait = 400 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cliOut := s.startRuntime(ctx)
	defer s.agent.Close()

	done := make(chan struct{})
	go func() {
		s.sendMessage(ctx, cliOut, "please hang")
		close(done)
	}()

	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider never started; agent loop did not begin the turn")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sendMessage did not return after response timeout")
	}

	if !strings.Contains(buf.String(), "timeout waiting for response") {
		t.Fatalf("expected timeout message, got %q", buf.String())
	}

	select {
	case err := <-p.cancelled:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active turn was not cancelled after TUI response timeout")
	}
}
