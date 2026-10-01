package tools

import (
	"context"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/mcp"
)

// TestBackgroundOriginContextBeatsAmbient pins the reported bug: two live
// sessions calling background in overlapping turns. The per-turn context
// origin must win over the shared mutable SetContext state — job A must not
// inherit dispatch B's destination.
func TestBackgroundOriginContextBeatsAmbient(t *testing.T) {
	hub := chat.NewHub(16)
	execTool := NewExecToolWithSandbox(30, t.TempDir(), nil, config.SandboxConfig{Mode: "yolo", AllowStringCommands: true})
	tool := NewBackgroundTool(hub, execTool)

	// Session B dispatches LATER and clobbers the ambient state, as happens
	// with concurrent goroutine turns sharing one tool instance.
	tool.SetContext("discord", "thread-b")

	// Session A's turn carries its own origin in the context.
	ctxA := mcp.WithTurnOriginSession(context.Background(), "telegram", "111", "telegram:111")
	out, err := tool.Execute(ctxA, map[string]interface{}{
		"action":  "start",
		"name":    "origin-probe",
		"cmd":     []string{"true"},
		"timeout": "5s",
	})
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	t.Log(out)

	tool.mu.Lock()
	var job *bgOneShot
	for _, j := range tool.oneShots {
		job = j
	}
	tool.mu.Unlock()
	if job == nil {
		t.Fatal("no job registered")
	}
	if job.Channel != "telegram" || job.ChatID != "111" {
		t.Fatalf("job captured wrong origin: got %s:%s, want telegram:111 (ambient clobber must not win)", job.Channel, job.ChatID)
	}
	if job.SessionKey != "telegram:111" {
		t.Fatalf("job session key: got %q, want telegram:111", job.SessionKey)
	}
}

// TestBackgroundNotifyCarriesSessionKey verifies the completion notification
// routes into the originating session via session_key metadata.
func TestBackgroundNotifyCarriesSessionKey(t *testing.T) {
	hub := chat.NewHub(16)
	execTool := NewExecToolWithSandbox(30, t.TempDir(), nil, config.SandboxConfig{Mode: "yolo", AllowStringCommands: true})
	tool := NewBackgroundTool(hub, execTool)

	ctx := mcp.WithTurnOriginSession(context.Background(), "telegram", "42", "telegram:42")
	_, err := tool.Execute(ctx, map[string]interface{}{
		"action":  "start",
		"name":    "notify-probe",
		"cmd":     []string{"sh", "-c", "echo done"},
		"timeout": "10s",
	})
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case msg := <-hub.In:
			if sk, _ := msg.Metadata["session_key"].(string); sk == "telegram:42" {
				if msg.Channel != "telegram" || msg.ChatID != "42" {
					t.Fatalf("notification routed to %s:%s", msg.Channel, msg.ChatID)
				}
				return // pass
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("no notification with session_key telegram:42 arrived within deadline")
}

// TestBackgroundAmbientFallbackStillWorks: direct callers without an
// origin-carrying context keep the old behavior via SetContext.
func TestBackgroundAmbientFallbackStillWorks(t *testing.T) {
	hub := chat.NewHub(16)
	execTool := NewExecToolWithSandbox(30, t.TempDir(), nil, config.SandboxConfig{Mode: "yolo", AllowStringCommands: true})
	tool := NewBackgroundTool(hub, execTool)
	tool.SetContext("cli", "tui-1")

	ch, id, sk := tool.resolveOrigin(context.Background())
	if ch != "cli" || id != "tui-1" || sk != "" {
		t.Fatalf("ambient fallback broken: %q %q %q", ch, id, sk)
	}
}
