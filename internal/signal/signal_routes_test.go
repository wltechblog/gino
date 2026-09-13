package signal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
)

// startTestListener boots a listener and waits for its socket to appear.
func startTestListener(t *testing.T, hub *chat.Hub, registry *Registry, defaultCh, defaultID, persistPath string) (*Listener, context.CancelFunc) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	l := NewListener(socketPath, hub, registry, defaultCh, defaultID)
	if persistPath != "" {
		l.SetPersistencePath(persistPath)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = l.Start(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return l, cancel
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("listener socket never appeared")
	return nil, cancel
}

// TestSourceBindingBeatsLastTarget reproduces the multi-session bug: two
// Discord threads live, thread B messaged last (global last-known), but the
// trigger was armed from thread A via a tool call on server "joinery". The
// signal from joinery must route to A, not to most-recently-active B.
func TestSourceBindingBeatsLastTarget(t *testing.T) {
	hub := chat.NewHub(10)
	l, cancel := startTestListener(t, hub, newTestRegistry(), "", "", "")
	defer cancel()

	// Thread B messaged most recently — what the old routing keyed on.
	l.SetLastTarget("discord", "thread-b")

	// Thread A armed the trigger by calling a joinery tool.
	l.BindSource("joinery", "discord", "thread-a")

	if err := SendSignal(l.SocketPath(), Signal{Source: "joinery", Action: "check_messages"}); err != nil {
		t.Fatalf("SendSignal: %v", err)
	}

	select {
	case msg := <-hub.In:
		if msg.Channel != "discord" || msg.ChatID != "thread-a" {
			t.Errorf("signal routed to %s:%s, want discord:thread-a (binding), not thread-b (last target)", msg.Channel, msg.ChatID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal delivery")
	}
}

// TestExplicitChannelStillWins checks signal-provided channel/chatID (the
// _meta echo path) outranks the per-source binding.
func TestExplicitChannelStillWins(t *testing.T) {
	hub := chat.NewHub(10)
	l, cancel := startTestListener(t, hub, newTestRegistry(), "", "", "")
	defer cancel()

	l.BindSource("joinery", "discord", "thread-a")

	if err := SendSignal(l.SocketPath(), Signal{
		Source: "joinery", Action: "check_messages",
		Channel: "discord", ChatID: "thread-b",
	}); err != nil {
		t.Fatalf("SendSignal: %v", err)
	}

	select {
	case msg := <-hub.In:
		if msg.ChatID != "thread-b" {
			t.Errorf("explicit chatID thread-b should win over binding, got %q", msg.ChatID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal delivery")
	}
}

// TestNoBindingFallsBackToLastTarget verifies unchanged legacy behavior when
// no per-source binding exists.
func TestNoBindingFallsBackToLastTarget(t *testing.T) {
	hub := chat.NewHub(10)
	l, cancel := startTestListener(t, hub, newTestRegistry(), "", "", "")
	defer cancel()

	l.SetLastTarget("telegram", "999")

	if err := SendSignal(l.SocketPath(), Signal{Source: "joinery", Action: "check_messages"}); err != nil {
		t.Fatalf("SendSignal: %v", err)
	}

	select {
	case msg := <-hub.In:
		if msg.ChatID != "999" {
			t.Errorf("expected fallback to last target 999, got %q", msg.ChatID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal delivery")
	}
}

// TestBindingPersistenceRoundTrip verifies bindings survive a restart via
// signal_routes.json.
func TestBindingPersistenceRoundTrip(t *testing.T) {
	persistPath := filepath.Join(t.TempDir(), "signal_routes.json")

	hub1 := chat.NewHub(10)
	l1, c1 := startTestListener(t, hub1, newTestRegistry(), "", "", persistPath)
	l1.BindSource("joinery", "discord", "thread-a")
	c1()
	time.Sleep(50 * time.Millisecond) // let persist settle

	hub2 := chat.NewHub(10)
	l2, c2 := startTestListener(t, hub2, newTestRegistry(), "", "", persistPath)
	defer c2()

	l2.SetLastTarget("discord", "thread-b") // newer activity must NOT win

	if err := SendSignal(l2.SocketPath(), Signal{Source: "joinery", Action: "check_messages"}); err != nil {
		t.Fatalf("SendSignal: %v", err)
	}

	select {
	case msg := <-hub2.In:
		if msg.ChatID != "thread-a" {
			t.Errorf("persisted binding lost: routed to %q, want thread-a", msg.ChatID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal delivery")
	}
}

// TestRebindingLastCallerWins documents the contention semantics: a later
// session calling tools on the same server takes over the binding.
func TestRebindingLastCallerWins(t *testing.T) {
	hub := chat.NewHub(10)
	l, cancel := startTestListener(t, hub, newTestRegistry(), "", "", "")
	defer cancel()

	l.BindSource("joinery", "discord", "thread-a")
	l.BindSource("joinery", "discord", "thread-b") // B later queries the same server

	if err := SendSignal(l.SocketPath(), Signal{Source: "joinery", Action: "check_messages"}); err != nil {
		t.Fatalf("SendSignal: %v", err)
	}

	select {
	case msg := <-hub.In:
		if msg.ChatID != "thread-b" {
			t.Errorf("expected last caller thread-b to hold binding, got %q", msg.ChatID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal delivery")
	}
}

// TestUnbindSource removes a binding so signals fall back to last-known.
func TestUnbindSource(t *testing.T) {
	hub := chat.NewHub(10)
	l, cancel := startTestListener(t, hub, newTestRegistry(), "", "", "")
	defer cancel()

	l.BindSource("joinery", "discord", "thread-a")
	l.UnbindSource("joinery")
	l.SetLastTarget("telegram", "777")

	if err := SendSignal(l.SocketPath(), Signal{Source: "joinery", Action: "check_messages"}); err != nil {
		t.Fatalf("SendSignal: %v", err)
	}

	select {
	case msg := <-hub.In:
		if msg.ChatID != "777" {
			t.Errorf("after unbind expected last-target 777, got %q", msg.ChatID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal delivery")
	}
}
