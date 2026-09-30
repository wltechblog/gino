package signal

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
)

// dialProbe sends a builtin probe signal on a raw connection and returns
// the listener's response line.
func dialProbe(t *testing.T, socketPath string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte(`{"source":"supervisor","action":"probe"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && line == "" {
		// Response has no trailing newline; read what's buffered.
		t.Fatalf("read: %v", err)
	}
	return line
}

func TestProbeAnsweredWithBusyState(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "probe.sock")
	hub := chat.NewHub(4)
	listener := NewListener(socketPath, hub, newTestRegistry(), "", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = listener.Start(ctx) }()
	waitForSocket(t, socketPath)

	// Default (no probe fn): busy false.
	if got := dialProbe(t, socketPath); got != `{"status":"ok","busy":false,"proto":2}` {
		t.Fatalf("default probe response = %q", got)
	}

	// Armed probe: busy true.
	listener.SetBusyProbe(func() bool { return true })
	if got := dialProbe(t, socketPath); got != `{"status":"ok","busy":true,"proto":2}` {
		t.Fatalf("armed probe response = %q", got)
	}
}

func TestProbeNeedsNoRegistryEntry(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "probe.sock")
	hub := chat.NewHub(4)
	listener := NewListener(socketPath, hub, NewRegistry(nil), "", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = listener.Start(ctx) }()
	waitForSocket(t, socketPath)

	// The probe is builtin: an empty registry (every other action would be
	// rejected as unknown) still answers it.
	if got := dialProbe(t, socketPath); got != `{"status":"ok","busy":false,"proto":2}` {
		t.Fatalf("empty-registry probe response = %q", got)
	}
}

func TestProbeDoesNotInjectIntoHub(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "probe.sock")
	hub := chat.NewHub(4)
	listener := NewListener(socketPath, hub, newTestRegistry(), "test", "123")
	listener.SetBusyProbe(func() bool { return true })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = listener.Start(ctx) }()
	waitForSocket(t, socketPath)

	dialProbe(t, socketPath)

	// Probes are pure liveness checks: nothing may reach the agent.
	select {
	case msg := <-hub.In:
		t.Fatalf("probe injected into hub: %+v", msg)
	case <-time.After(300 * time.Millisecond):
	}
}

// waitForSocket polls until the listener's socket file exists.
func waitForSocket(t *testing.T, socketPath string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(socketPath); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("socket never appeared")
}
