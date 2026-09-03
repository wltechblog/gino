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

// lateNSProvider blocks until its context is cancelled, then returns a
// successful reply — the exact orphaned-reply shape: TUI times out or the
// user hits /stop; StopTurn must find and cancel the turn under its
// namespaced key or the reply orphans into the next prompt.
type lateNSProvider struct {
	chatCalled chan struct{}
	once       sync.Once
}

func (p *lateNSProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	p.once.Do(func() { close(p.chatCalled) })
	<-ctx.Done()
	return providers.LLMResponse{Content: "late reply nobody is waiting for"}, nil
}

func (p *lateNSProvider) GetDefaultModel() string { return "ns-model" }

func (p *lateNSProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

// nsFixture wires an AgentLoop with an ACTIVE project, mirroring
// `gino chat -project book` (registry persisted in the profile dir).
type nsFixture struct {
	ag  *AgentLoop
	hub *chat.Hub
	p   *lateNSProvider
}

func newNSFixture(t *testing.T) *nsFixture {
	t.Helper()
	projWS := t.TempDir()
	profWS := t.TempDir()

	reg, err := LoadProjectRegistry(profWS)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if _, err := reg.Add("book", projWS); err != nil {
		t.Fatalf("add project: %v", err)
	}
	if err := reg.SetActive("book"); err != nil {
		t.Fatalf("set active: %v", err)
	}

	b := chat.NewHub(10)
	p := &lateNSProvider{chatCalled: make(chan struct{})}
	ag := NewAgentLoopWithProfileWorkspace(
		b, p, p.GetDefaultModel(), 5, projWS, profWS,
		nil, nil, nil, nil, nil, profWS, config.SandboxConfig{}, "",
		0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "",
	)
	return &nsFixture{ag: ag, hub: b, p: p}
}

// TestNamespacedKey covers the resolver: prefixed when a project is active,
// idempotent on already-prefixed keys, bare when no project is active.
func TestNamespacedKey(t *testing.T) {
	f := newNSFixture(t)
	defer f.ag.Close()

	bare := "cli:tui-123"
	want := "proj:book:" + bare
	if got := f.ag.namespacedKey(bare); got != want {
		t.Fatalf("namespacedKey = %q, want %q", got, want)
	}
	if got := f.ag.namespacedKey(want); got != want {
		t.Fatalf("namespacedKey not idempotent: %q", got)
	}

	profWS2 := t.TempDir()
	b2 := chat.NewHub(10)
	p2 := &lateNSProvider{chatCalled: make(chan struct{})}
	ag2 := NewAgentLoopWithProfileWorkspace(
		b2, p2, p2.GetDefaultModel(), 5, profWS2, profWS2,
		nil, nil, nil, nil, nil, profWS2, config.SandboxConfig{}, "",
		0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "",
	)
	defer ag2.Close()
	if got := ag2.namespacedKey(bare); got != bare {
		t.Fatalf("no-project namespacedKey = %q, want bare %q", got, bare)
	}
}

// TestStopTurnResolvesNamespacedKey reproduces the orphaned-reply incident:
// with a project active, the turn registers under proj:book:cli:tui-123 but
// the TUI calls StopTurn("cli:tui-123"). Before the fix, cancelActiveTurn
// found nothing (returned false), the turn ran to completion, and the reply
// was queued into the NEXT prompt's wait loop.
func TestStopTurnResolvesNamespacedKey(t *testing.T) {
	f := newNSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.ag.Run(ctx)
	defer f.ag.Close()

	sub := f.hub.Subscribe("cli")
	f.hub.StartRouter(ctx)

	f.hub.In <- chat.Inbound{
		Channel:   "cli",
		SenderID:  "tui-user",
		ChatID:    "tui-123",
		Content:   "trigger turn",
		Timestamp: time.Now(),
	}

	select {
	case <-f.p.chatCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never called; turn did not start")
	}

	// The turn must be cancellable via the BARE key the TUI holds.
	if !f.ag.StopTurn("cli:tui-123") {
		t.Fatal("StopTurn(bare key) returned false — turn not found under namespaced key")
	}

	// The late-success reply must be suppressed: nothing on the subscriber
	// within the grace window (router drops nothing; suppression is agent-side).
	select {
	case out, ok := <-sub:
		if ok {
			t.Fatalf("reply leaked to subscriber after StopTurn: %q", out.Content)
		}
	case <-time.After(2 * time.Second):
		// expected: suppressed
	}
}

// TestSessionHelpersResolveNamespacedKey covers the /title phantom-session
// bug: SetSessionTitle with a bare key must write the session the turns
// actually use (proj:namespaced), and CurrentSessionTitle must read it back.
func TestSessionHelpersResolveNamespacedKey(t *testing.T) {
	f := newNSFixture(t)
	defer f.ag.Close()

	bare := "cli:tui-456"
	nsKey := "proj:book:" + bare

	f.ag.SetSessionTitle(bare, "My Book Session")

	if got := f.ag.sessions.Get(nsKey); got == nil {
		t.Fatal("title written to bare key — namespaced session not created")
	} else if got.Title != "My Book Session" {
		t.Fatalf("namespaced session title = %q, want %q", got.Title, "My Book Session")
	}
	if got := f.ag.CurrentSessionTitle(bare); got != "My Book Session" {
		t.Fatalf("CurrentSessionTitle(bare) = %q, want %q", got, "My Book Session")
	}
	if got := f.ag.CurrentSessionTitle(nsKey); got != "My Book Session" {
		t.Fatalf("CurrentSessionTitle(namespaced) = %q, want %q", got, "My Book Session")
	}
}
