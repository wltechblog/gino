package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// sigDupProvider behaves like the duplicate-work scenario's provider:
//   - main turn: blocks until released, then returns a completion reply
//   - signal turn: records that it was dispatched and returns immediately
//
// If the signal were allowed to run in PARALLEL with the active main turn
// (the pre-fix behavior), signalCalls would increment while mainCalls == 0
// still holds — the duplication race.
type sigDupProvider struct {
	mu sync.Mutex

	mainStarted   chan struct{} // closed when the main turn's provider call starts
	releaseMain   chan struct{} // closed by the test to let the main turn finish
	signalStarted chan struct{} // closed when a signal turn's provider call starts

	mainCalls   int
	signalCalls int

	// lastSignalPrompt is the most recent prompt the signal turn saw —
	// used to verify the signal turn was built with the MAIN session
	// history (task completion visible) instead of a blind namespace.
	lastSignalPrompt []providers.Message
}

func (p *sigDupProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	// Classify by the LAST user message only — signal turns now carry the
	// main history too (the blindness fix), so scanning every message
	// would misclassify. Background calls (auto-titler, turn-extract) have
	// no tools and are ignored entirely.
	isSignal := false
	if len(tools) > 0 {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				isSignal = containsStr(messages[i].Content, "new messages in your agentchat session") ||
					containsStr(messages[i].Content, "External signal")
				break
			}
		}
	} else {
		return providers.LLMResponse{Content: "title"}, nil // background call (auto-titler/turn-extract): ignore
	}

	p.mu.Lock()
	if isSignal {
		p.signalCalls++
		p.lastSignalPrompt = append([]providers.Message{}, messages...)
	} else {
		p.mainCalls++
	}
	p.mu.Unlock()

	if isSignal {
		if p.signalStarted != nil {
			closeOnce(&p.signalStarted)
		}
		return providers.LLMResponse{Content: "signal handled"}, nil
	}

	// Main turn: block until released.
	if p.mainStarted != nil {
		closeOnce(&p.mainStarted)
	}
	<-p.releaseMain
	return providers.LLMResponse{Content: "task completed"}, nil
}

func (p *sigDupProvider) GetDefaultModel() string { return "sig-dup-model" }

func (p *sigDupProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

func closeOnce(c *chan struct{}) {
	select {
	case <-*c:
	default:
		close(*c)
	}
}

func containsStr(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOfStr(haystack, needle) >= 0)
}

func indexOfStr(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func (p *sigDupProvider) stats() (mainCalls, signalCalls int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mainCalls, p.signalCalls
}

// TestSignalDeferredBehindActiveTurn pins the deferral contract:
// a signal arriving while the session's main turn is running must be
// queued, NOT dispatched in parallel. The duplicate-work incident:
// interactive turn completes a task, agentchat ripple fires check_messages,
// the signal turn — blind, running concurrently — re-executed the task
// and sent a second "completed" reply.
func TestSignalDeferredBehindActiveTurn(t *testing.T) {
	b := chat.NewHub(16)
	p := &sigDupProvider{
		mainStarted:   make(chan struct{}),
		releaseMain:   make(chan struct{}),
		signalStarted: make(chan struct{}),
	}

	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 5, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	defer ag.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	b.StartRouter(ctx)

	sub := b.Subscribe("telegram")

	// Start the main interactive turn.
	b.In <- chat.Inbound{
		Channel:   "telegram",
		ChatID:    "1",
		SenderID:  "user",
		Content:   "please do the thing",
		Timestamp: time.Now(),
	}

	// Wait until the main turn is mid-flight (provider called, blocking).
	select {
	case <-p.mainStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("main turn never started")
	}

	// Fire the signal WHILE the main turn is active.
	b.In <- chat.Inbound{
		Channel:   "telegram",
		ChatID:    "1",
		SenderID:  "signal:agentchat",
		Content:   "You have received new messages in your agentchat session (telegram:1). Use your agentchat tools (receive_messages or wait_for_message) to read and handle them.",
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"signal_source": "agentchat",
			"signal_action": "check_messages",
			"signal_silent": true,
		},
	}

	// The signal must NOT start while the main turn runs: give it every
	// chance to misbehave, then assert it never fired.
	select {
	case <-p.signalStarted:
		t.Fatal("signal turn ran in PARALLEL with the active main turn — duplication race")
	case <-time.After(300 * time.Millisecond):
	}

	// Signal must be sitting in the deferred queue.
	ag.mu.Lock()
	queued := len(ag.signalQueue["telegram:1"])
	ag.mu.Unlock()
	if queued != 1 {
		t.Fatalf("deferred signal queue = %d entries, want 1", queued)
	}

	// Let the main turn finish.
	close(p.releaseMain)

	// The queued signal is released after the turn ends; it now runs with
	// the main session history visible (no duplication blindness).
	select {
	case <-p.signalStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred signal was never released after turn completion")
	}

	mc, sc := p.stats()
	if mc != 1 {
		t.Fatalf("main calls = %d, want 1", mc)
	}
	if sc < 1 {
		t.Fatalf("signal calls = %d, want >= 1", sc)
	}

	// The signal turn must have seen the main conversation: its prompt
	// includes the main turn's task request and completion reply.
	p.mu.Lock()
	prompt := p.lastSignalPrompt
	p.mu.Unlock()
	sawTask := false
	sawCompletion := false
	for _, m := range prompt {
		if m.Role == "user" && containsStr(m.Content, "please do the thing") {
			sawTask = true
		}
		if m.Role == "assistant" && containsStr(m.Content, "task completed") {
			sawCompletion = true
		}
	}
	if !sawTask || !sawCompletion {
		t.Fatalf("signal turn prompt blind to main session: sawTask=%v sawCompletion=%v", sawTask, sawCompletion)
	}

	// Drain the reply so the hub buffer doesn't backpressure.
	select {
	case <-sub:
	case <-time.After(2 * time.Second):
	}
}

// TestSignalRunsWhenNoActiveTurn pins the common path: no turn active,
// signal dispatches immediately (deferral is only for concurrency).
func TestSignalRunsWhenNoActiveTurn(t *testing.T) {
	b := chat.NewHub(16)
	p := &sigDupProvider{
		mainStarted:   make(chan struct{}),
		releaseMain:   make(chan struct{}),
		signalStarted: make(chan struct{}),
	}

	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 5, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	defer ag.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	b.StartRouter(ctx)

	sub := b.Subscribe("telegram")

	// Fire a signal with NO active turn.
	b.In <- chat.Inbound{
		Channel:   "telegram",
		ChatID:    "1",
		SenderID:  "signal:agentchat",
		Content:   "You have received new messages in your agentchat session (telegram:1). Use your agentchat tools (receive_messages or wait_for_message) to read and handle them.",
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"signal_source": "agentchat",
			signalActionKey: "check_messages",
			"signal_silent": true,
		},
	}

	select {
	case <-p.signalStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("signal did not run when no turn was active")
	}

	select {
	case <-sub:
	case <-time.After(2 * time.Second):
	}
}

const signalActionKey = "signal_action"
