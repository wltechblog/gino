package agent

import (
	"testing"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// newIdleLoop builds a loop with nil maps un-initialized like production
// NewAgentLoop does, then exercises the Idle()/BackgroundJobs() surface.
func newIdleLoop(t *testing.T) *AgentLoop {
	t.Helper()
	b := chat.NewHub(10)
	p := providers.NewStubProvider()
	return NewAgentLoop(b, p, p.GetDefaultModel(), 5, t.TempDir(), nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
}

func TestIdleFreshLoopIsIdle(t *testing.T) {
	ag := newIdleLoop(t)
	if !ag.Idle() {
		t.Fatal("fresh loop with no work must report Idle()=true")
	}
	if ag.BackgroundJobs() != 0 {
		t.Fatalf("BackgroundJobs on loop without background tool: got %d, want 0", ag.BackgroundJobs())
	}
}

func TestIdleWithQueuedSignal(t *testing.T) {
	ag := newIdleLoop(t)
	// Seed a deferred signal entry exactly as the deferral path does.
	ag.mu.Lock()
	ag.signalQueue["telegram:1"] = append(ag.signalQueue["telegram:1"],
		chat.Inbound{Channel: "signal", Content: "wake"})
	ag.mu.Unlock()
	if ag.Idle() {
		t.Fatal("deferred signal in queue must keep the loop busy (an exit here would strand the wake-up)")
	}
}

func TestIdleWithPausedTurn(t *testing.T) {
	ag := newIdleLoop(t)
	ag.mu.Lock()
	ag.paused["telegram:1"] = &pausedTurn{}
	ag.mu.Unlock()
	if ag.Idle() {
		t.Fatal("iteration-paused turn awaiting \"continue\" must keep the loop busy (exit would orphan the paused turn)")
	}
}

func TestIdleWithPendingMsg(t *testing.T) {
	ag := newIdleLoop(t)
	ag.mu.Lock()
	ag.pending["telegram:1"] = append(ag.pending["telegram:1"], pendingMsg{content: "hi"})
	ag.mu.Unlock()
	if ag.Idle() {
		t.Fatal("pending message queued for an active turn must keep the loop busy (exit would eat the user's message)")
	}
}

func TestIdleWithActiveTurn(t *testing.T) {
	ag := newIdleLoop(t)
	ag.mu.Lock()
	ag.active["telegram:1"] = &activeTurn{done: make(chan struct{})}
	ag.mu.Unlock()
	if ag.Idle() {
		t.Fatal("active turn must keep the loop busy")
	}
}
