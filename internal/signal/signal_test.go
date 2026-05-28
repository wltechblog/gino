package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/local/picobot/internal/chat"
	"github.com/local/picobot/internal/config"
)

func newTestRegistry() *Registry {
	return NewRegistry(map[string]config.SignalActionConfig{
		"check_messages": {
			Description: "Check agentchat for new messages",
			Response:    "You received a signal to check your messages. Use the receive_messages or wait_for_message tool to check for new messages.",
		},
		"motion_detected": {
			Description: "Camera sensor triggered",
			Response:    "A motion sensor was triggered. You may want to check the camera feed.",
		},
	})
}

func TestListenerBasic(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	hub := chat.NewHub(10)
	registry := newTestRegistry()
	listener := NewListener(socketPath, hub, registry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go listener.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Fatal("socket file not created")
	}

	sig := Signal{
		Source:  "agentchat-mcp",
		Action:  "check_messages",
		Channel: "test",
		ChatID:  "123",
	}

	err := SendSignal(socketPath, sig)
	if err != nil {
		t.Fatalf("SendSignal failed: %v", err)
	}

	select {
	case msg := <-hub.In:
		if msg.Channel != "test" {
			t.Errorf("expected channel 'test', got %q", msg.Channel)
		}
		if msg.ChatID != "123" {
			t.Errorf("expected chatID '123', got %q", msg.ChatID)
		}
		if msg.SenderID != "signal:agentchat-mcp" {
			t.Errorf("expected senderID 'signal:agentchat-mcp', got %q", msg.SenderID)
		}
		// Response should be the safe template, not raw signal content
		if msg.Content != "You received a signal to check your messages. Use the receive_messages or wait_for_message tool to check for new messages." {
			t.Errorf("unexpected response content: %q", msg.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal to be delivered to hub")
	}
}

func TestListenerDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	hub := chat.NewHub(10)
	registry := newTestRegistry()
	listener := NewListener(socketPath, hub, registry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go listener.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	sig := Signal{
		Source: "camera-script",
		Action: "motion_detected",
	}

	err := SendSignal(socketPath, sig)
	if err != nil {
		t.Fatalf("SendSignal failed: %v", err)
	}

	select {
	case msg := <-hub.In:
		if msg.Channel != "signal" {
			t.Errorf("expected default channel 'signal', got %q", msg.Channel)
		}
		if msg.ChatID != "default" {
			t.Errorf("expected default chatID 'default', got %q", msg.ChatID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal")
	}
}

func TestListenerMissingAction(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	hub := chat.NewHub(10)
	registry := newTestRegistry()
	listener := NewListener(socketPath, hub, registry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go listener.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// Signal with no action — should be rejected
	sig := Signal{
		Source: "test",
	}

	err := SendSignal(socketPath, sig)
	if err == nil {
		t.Log("missing action signal was accepted (server may not respond)")
	}

	select {
	case msg := <-hub.In:
		t.Errorf("expected no message for missing action, got: %+v", msg)
	case <-time.After(500 * time.Millisecond):
		// Expected: no message
	}
}

func TestListenerMissingSource(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	hub := chat.NewHub(10)
	registry := newTestRegistry()
	listener := NewListener(socketPath, hub, registry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go listener.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	sig := Signal{
		Action: "check_messages",
	}

	err := SendSignal(socketPath, sig)
	if err == nil {
		t.Log("missing source signal was accepted")
	}

	select {
	case msg := <-hub.In:
		t.Errorf("expected no message for missing source, got: %+v", msg)
	case <-time.After(500 * time.Millisecond):
		// Expected: no message
	}
}

func TestListenerUnknownAction(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	hub := chat.NewHub(10)
	registry := newTestRegistry()
	listener := NewListener(socketPath, hub, registry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go listener.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// Try to send a malicious signal with unknown action
	sig := Signal{
		Source: "attacker",
		Action: "delete_all_files",
	}

	err := SendSignal(socketPath, sig)
	if err == nil {
		t.Log("unknown action signal was accepted by server (expected rejection)")
	}

	select {
	case msg := <-hub.In:
		t.Errorf("expected no message for unknown action, got: %+v", msg)
	case <-time.After(500 * time.Millisecond):
		// Expected: rejected
	}
}

func TestListenerInvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	hub := chat.NewHub(10)
	registry := newTestRegistry()
	listener := NewListener(socketPath, hub, registry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go listener.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	conn.Write([]byte("not json at all"))

	select {
	case msg := <-hub.In:
		t.Errorf("expected no message for invalid JSON, got: %+v", msg)
	case <-time.After(500 * time.Millisecond):
		// Expected: no message
	}
}

func TestMCPRegistration(t *testing.T) {
	registry := newTestRegistry()

	// Register an MCP source
	registry.RegisterMCP("agentchat-mcp", []string{"check_messages", "new_mail"})

	if !registry.IsAllowed("new_mail") {
		t.Error("new_mail should be allowed after MCP registration")
	}

	// User-defined action still takes priority
	if registry.GetSource("check_messages") != "" {
		t.Error("check_messages should remain user-defined (empty source), not MCP")
	}

	// new_mail should be MCP-registered
	if registry.GetSource("new_mail") != "agentchat-mcp" {
		t.Errorf("expected new_mail source 'agentchat-mcp', got %q", registry.GetSource("new_mail"))
	}
}

func TestMCPRegistrationOverwrite(t *testing.T) {
	registry := newTestRegistry()

	// Register MCP source A
	registry.RegisterMCP("source-a", []string{"task_done"})
	if !registry.IsAllowed("task_done") {
		t.Error("task_done should be allowed")
	}

	// Re-register source A with different actions
	registry.RegisterMCP("source-a", []string{"task_complete"})
	if registry.IsAllowed("task_done") {
		t.Error("task_done should be removed after re-registration")
	}
	if !registry.IsAllowed("task_complete") {
		t.Error("task_complete should be allowed after re-registration")
	}
}

func TestUserDefinedOverridesMCP(t *testing.T) {
	registry := newTestRegistry()

	// motion_detected is user-defined
	resp := registry.GetResponse("motion_detected")
	if resp != "A motion sensor was triggered. You may want to check the camera feed." {
		t.Errorf("unexpected user-defined response: %q", resp)
	}

	// Register same action from MCP — should not overwrite
	registry.RegisterMCP("some-mcp", []string{"motion_detected"})
	resp = registry.GetResponse("motion_detected")
	if resp != "A motion sensor was triggered. You may want to check the camera feed." {
		t.Errorf("user-defined response should not be overwritten by MCP, got: %q", resp)
	}
}

func TestListenerCleanup(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	hub := chat.NewHub(10)
	registry := newTestRegistry()
	listener := NewListener(socketPath, hub, registry)

	ctx, cancel := context.WithCancel(context.Background())
	go listener.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Fatal("socket not created")
	}

	cancel()
	time.Sleep(200 * time.Millisecond)
}

func TestDefaultSocketPath(t *testing.T) {
	path := DefaultSocketPath("/home/user/workspace")
	expected := "/home/user/workspace/.picobot/signals.sock"
	if path != expected {
		t.Errorf("expected %q, got %q", expected, path)
	}
}

func TestSignalFormat(t *testing.T) {
	sig := Signal{
		Source:   "agentchat-mcp",
		Action:   "check_messages",
		Channel:  "telegram",
		ChatID:   "12345",
		Metadata: map[string]interface{}{"from": "agent-b"},
	}

	data, err := json.Marshal(sig)
	if err != nil {
		t.Fatalf("failed to marshal signal: %v", err)
	}

	var parsed Signal
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal signal: %v", err)
	}

	if parsed.Source != sig.Source {
		t.Errorf("source mismatch: %q != %q", parsed.Source, sig.Source)
	}
	if parsed.Action != sig.Action {
		t.Errorf("action mismatch: %q != %q", parsed.Action, sig.Action)
	}
	if parsed.Channel != sig.Channel {
		t.Errorf("channel mismatch: %q != %q", parsed.Channel, sig.Channel)
	}
}

func TestStaleSocketCleanup(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	staleConn, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create stale socket: %v", err)
	}
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		staleConn.Close()
		t.Fatal("stale socket not created")
	}
	staleConn.Close()

	hub := chat.NewHub(10)
	registry := newTestRegistry()
	listener := NewListener(socketPath, hub, registry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go listener.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Fatal("new socket not created after stale cleanup")
	}
}

func TestMultipleSignals(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	hub := chat.NewHub(50)
	registry := newTestRegistry()
	listener := NewListener(socketPath, hub, registry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go listener.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	for i := 0; i < 5; i++ {
		sig := Signal{
			Source:  "test-source",
			Action:  "check_messages",
			Channel: "test",
			ChatID:  fmt.Sprintf("%d", i),
		}
		err := SendSignal(socketPath, sig)
		if err != nil {
			t.Logf("SendSignal %d failed: %v", i, err)
		}
	}

	received := 0
	timeout := time.After(5 * time.Second)
	for received < 5 {
		select {
		case <-hub.In:
			received++
		case <-timeout:
			t.Fatalf("timed out after receiving %d/5 signals", received)
		}
	}
}
