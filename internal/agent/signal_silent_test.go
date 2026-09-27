package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// silentProvider returns a reply ending in the [silent] marker for signal
// turns (classified by the signal-handling instruction appended to the user
// content) and a normal reply otherwise. Toolless background calls
// (auto-titler, turn-extract) return a benign string and are not counted.
type silentProvider struct {
	signalReply string // what signal turns return (with or without marker)
	lastPrompt  []providers.Message
	signalCalls int
	normalCalls int
}

func (p *silentProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	if len(tools) == 0 {
		return providers.LLMResponse{Content: "title"}, nil
	}
	isSignal := false
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			isSignal = strings.Contains(messages[i].Content, "Signal handling mode")
			break
		}
	}
	if isSignal {
		p.signalCalls++
		p.lastPrompt = append([]providers.Message{}, messages...)
		if p.signalReply == "" {
			p.signalReply = "All messages handled — nothing to report. [silent]"
		}
		return providers.LLMResponse{Content: p.signalReply}, nil
	}
	p.normalCalls++
	return providers.LLMResponse{Content: "normal reply"}, nil
}

func (p *silentProvider) GetDefaultModel() string { return "silent-model" }

func (p *silentProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

// newSilentTestLoop builds an AgentLoop wired like the signal tests.
func newSilentTestLoop(t *testing.T, p providers.LLMProvider) (*AgentLoop, *chat.Hub) {
	t.Helper()
	b := chat.NewHub(16)
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 5, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	t.Cleanup(func() { ag.Close() })
	return ag, b
}

// sendSignal injects a silent signal message and drains the hub output for
// up to `wait` — returning every outbound message produced.
func sendSignal(t *testing.T, b *chat.Hub, silent bool) []chat.Outbound {
	t.Helper()
	meta := map[string]interface{}{
		"signal_source": "agentchat",
		"signal_action": "check_messages",
		"signal_silent": silent,
	}
	select {
	case b.In <- chat.Inbound{Channel: "telegram", SenderID: "signal:agentchat", ChatID: "111", Content: "[New messages in your agentchat session]", Metadata: meta}:
	default:
		t.Fatal("hub input full")
	}

	var outs []chat.Outbound
	deadline := time.After(3 * time.Second)
	for {
		var done bool
		select {
		case out := <-b.Out:
			outs = append(outs, out)
		case <-time.After(600 * time.Millisecond):
			done = true
		case <-deadline:
			done = true
		}
		if done {
			return outs
		}
	}
}

// TestSilentSignalMarkerSuppressesReply: a silent signal whose turn ends
// with the [silent] marker must produce ZERO channel output. This is the
// core fix — previously every check_messages wake-up ended in a courtesy
// "handled" message to the user.
func TestSilentSignalMarkerSuppressesReply(t *testing.T) {
	p := &silentProvider{}
	ag, b := newSilentTestLoop(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	outs := sendSignal(t, b, true)
	if len(outs) != 0 {
		t.Fatalf("silent signal produced %d channel messages, want 0: %+v", len(outs), outs)
	}
	if p.signalCalls == 0 {
		t.Fatal("signal turn never ran")
	}

	// The prompt contract must be present in the signal turn's user content.
	found := false
	for _, m := range p.lastPrompt {
		if m.Role == "user" && strings.Contains(m.Content, "Signal handling mode") {
			found = true
		}
	}
	if !found {
		t.Fatal("signal-handling instruction missing from signal turn prompt")
	}

	// The signal session history must record the marker (not an empty
	// assistant entry) so future signal turns see the wake-up was handled.
	sess := ag.sessions.Get("signal:telegram:111")
	if sess == nil {
		t.Fatal("signal session not created")
	}
	foundMarker := false
	for _, h := range sess.History {
		if strings.Contains(h, silentMarker) {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Fatalf("signal session history lacks %s marker: %v", silentMarker, sess.History)
	}
}

// TestSilentSignalNoMarkerStillDelivers: the model replying WITHOUT the
// marker (something genuinely worth saying) must still deliver normally —
// suppression is the model's choice, not mandatory.
func TestSilentSignalNoMarkerStillDelivers(t *testing.T) {
	p := &silentProvider{signalReply: "You have a new message from Josh: need the deploy done."}
	ag, b := newSilentTestLoop(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	outs := sendSignal(t, b, true)
	if len(outs) == 0 {
		t.Fatal("signal with substantive reply produced no output")
	}
	if !strings.Contains(outs[0].Content, "new message from Josh") {
		t.Fatalf("delivered content mismatch: %q", outs[0].Content)
	}
	if strings.Contains(outs[0].Content, silentMarker) {
		t.Fatalf("marker leaked into delivered reply: %q", outs[0].Content)
	}
}

// TestNonSilentSignalDelivers: non-silent signals keep current behavior —
// reply always delivers, no instruction appended.
func TestNonSilentSignalDelivers(t *testing.T) {
	p := &silentProvider{}
	ag, b := newSilentTestLoop(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	outs := sendSignal(t, b, false)
	if len(outs) == 0 {
		t.Fatal("non-silent signal produced no output")
	}
	found := false
	for _, m := range p.lastPrompt {
		if m.Role == "user" && strings.Contains(m.Content, "Signal handling mode") {
			found = true
		}
	}
	if found {
		t.Fatal("silence instruction must NOT be appended for non-silent signals")
	}
}
