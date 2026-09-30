package agent

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// doorbellProvider answers immediately so the turn completes and the
// parent doorbell (if armed) fires.
type doorbellProvider struct{}

func (p *doorbellProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	return providers.LLMResponse{Content: "done"}, nil
}
func (p *doorbellProvider) GetDefaultModel() string { return "doorbell-model" }
func (p *doorbellProvider) GetModelContext(ctx context.Context, m string) (int, error) {
	return 0, nil
}

// TestParentDoorbellFiresAfterTurn: a supervised child (parentSignalSocket
// armed) fires the task_done doorbell at its parent's socket after every
// completed turn. A fake parent listener on a real Unix socket captures it.
func TestParentDoorbellFiresAfterTurn(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "parent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type doorbell struct {
		Source string `json:"source"`
		Action string `json:"action"`
	}
	got := make(chan doorbell, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Mirror the real listener: one Read of the raw JSON payload
		// (SendSignal writes no newline), then write the ack.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil && n == 0 {
			return
		}
		var sig doorbell
		_ = json.Unmarshal(buf[:n], &sig)
		got <- sig
		_, _ = conn.Write([]byte(`{"status":"ok"}`))
	}()

	b := chat.NewHub(10)
	p := &doorbellProvider{}
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 5, t.TempDir(), nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	ag.SetParentSignalSocket(sock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()
	b.StartRouter(ctx)

	b.In <- chat.Inbound{
		Channel:   "cli",
		ChatID:    "doorbell-test",
		SenderID:  "tester",
		Content:   "finish quickly",
		Timestamp: time.Now(),
	}

	select {
	case sig := <-got:
		if sig.Source != "gino-intern" || sig.Action != "task_done" {
			t.Fatalf("wrong doorbell: %+v", sig)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("doorbell never fired after turn completion")
	}
}

// Unarmed gateways (no parentSignalSocket) never fire the doorbell.
func TestNoDoorbellWhenUnarmed(t *testing.T) {
	b := chat.NewHub(10)
	p := &doorbellProvider{}
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 5, t.TempDir(), nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	if ag.parentSignalSocket != "" {
		t.Fatal("parentSignalSocket should default empty")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	defer ag.Close()
	b.StartRouter(ctx)

	b.In <- chat.Inbound{
		Channel:   "cli",
		ChatID:    "unarmed-test",
		SenderID:  "tester",
		Content:   "no doorbell",
		Timestamp: time.Now(),
	}

	// Turn completes; the armed branch is skipped entirely. Wait long
	// enough for a turn + cleanup, assert nothing explodes and no
	// doorbell path was taken (field still empty).
	time.Sleep(500 * time.Millisecond)
	if ag.parentSignalSocket != "" {
		t.Fatal("parentSignalSocket mutated unexpectedly")
	}
}
