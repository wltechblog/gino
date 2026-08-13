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
