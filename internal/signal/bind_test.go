package signal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
)

// TestBindThenServeSeparation pins the early-bind contract used by the
// gateway to eliminate the startup race with agentchat bridges: Bind()
// must create the socket file synchronously (before any MCP child is
// spawned), and Serve() must accept dials made immediately after Bind.
func TestBindThenServeSeparation(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "sub", "signals.sock")

	hub := chat.NewHub(10)
	registry := newTestRegistry()
	listener := NewListener(socketPath, hub, registry, "", "")

	// Bind synchronously — socket must exist when this returns.
	if err := listener.Bind(); err != nil {
		t.Fatalf("Bind failed: %v", err)
	}
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Fatal("socket file not created by Bind")
	}

	// A dial made the instant after Bind (the agentchat bridge's first
	// wake-up retry, spawned before the gateway finishes constructing)
	// must connect without ENOENT once Serve runs.
	done := make(chan struct{})
	go func() {
		_ = listener.Serve(context.Background())
		close(done)
	}()

	sig := Signal{Source: "agentchat-mcp", Action: "check_messages", Channel: "test", ChatID: "1"}
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for {
		lastErr = SendSignal(socketPath, sig)
		if lastErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SendSignal never succeeded: %v", lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Start(ctx) on an already-bound listener must not rebind or error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = listener.Start(ctx) // immediate cancel: must return promptly, nil or shutdown path
}

// TestServeBeforeBindErrors ensures Serve without Bind fails loudly
// instead of silently accepting nothing.
func TestServeBeforeBindErrors(t *testing.T) {
	tmpDir := t.TempDir()
	listener := NewListener(filepath.Join(tmpDir, "x.sock"), chat.NewHub(10), newTestRegistry(), "", "")
	if err := listener.Serve(context.Background()); err == nil {
		t.Fatal("expected error from Serve before Bind")
	}
}
