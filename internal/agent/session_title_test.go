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

// titleProvider returns a fixed title string for any LLM call, recording
// that it was invoked.
type titleProvider struct {
	called int
}

func (p *titleProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	p.called++
	return providers.LLMResponse{Content: `"Gino Session Management Design"`, HasToolCalls: false}, nil
}
func (p *titleProvider) GetDefaultModel() string                                    { return "fake-model" }
func (p *titleProvider) GetModelContext(ctx context.Context, m string) (int, error) { return 0, nil }

func newTitleTestLoop(t *testing.T) (*AgentLoop, *titleProvider, *chat.Hub) {
	t.Helper()
	b := chat.NewHub(10)
	p := &titleProvider{}
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 5, t.TempDir(), nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	t.Cleanup(func() { ag.Close() })
	return ag, p, b
}

func TestAutoTitleUntitledSession(t *testing.T) {
	ag, p, _ := newTitleTestLoop(t)
	key := "telegram:456"
	ag.sessions.GetOrCreate(key)

	ag.maybeAutoTitleSession(key, "Our sessions are hard to tell apart", "Let's add titles.")

	ag.bgWG.Wait()

	s := ag.sessions.Get(key)
	if s == nil || s.Title != "Gino Session Management Design" {
		t.Fatalf("expected LLM title, got %+v", s)
	}
	if s.TitleSource != "llm" {
		t.Fatalf("expected title_source llm, got %q", s.TitleSource)
	}
	if p.called != 1 {
		t.Fatalf("expected 1 provider call, got %d", p.called)
	}
}

func TestAutoTitleDoesNotClobberManual(t *testing.T) {
	ag, p, _ := newTitleTestLoop(t)
	key := "telegram:456"
	ag.sessions.GetOrCreate(key)
	ag.sessions.SetTitle(key, "My manual title", "manual")

	ag.maybeAutoTitleSession(key, "hello", "hi")

	ag.bgWG.Wait()

	s := ag.sessions.Get(key)
	if s.Title != "My manual title" {
		t.Fatalf("manual title was clobbered: %q", s.Title)
	}
	if p.called != 0 {
		t.Fatalf("provider should not have been called for titled session, called %d", p.called)
	}
}

func TestAutoTitleDisabled(t *testing.T) {
	ag, p, _ := newTitleTestLoop(t)
	key := "telegram:456"
	ag.sessions.GetOrCreate(key)
	ag.SetSessionAutoTitle(false)

	ag.maybeAutoTitleSession(key, "hello", "hi")

	ag.bgWG.Wait()

	s := ag.sessions.Get(key)
	if s.Title != "" {
		t.Fatalf("expected no title when disabled, got %q", s.Title)
	}
	if p.called != 0 {
		t.Fatalf("provider should not have been called when disabled, called %d", p.called)
	}
}

func TestSetTitleManualStickyAtManager(t *testing.T) {
	ag, _, _ := newTitleTestLoop(t)
	key := "telegram:456"
	ag.sessions.GetOrCreate(key)

	// llm title first, then manual wins, then llm can't clobber manual
	ag.sessions.SetTitle(key, "LLM title", "llm")
	ag.sessions.SetTitle(key, "Manual title", "manual")
	ag.sessions.SetTitle(key, "Another LLM try", "llm")

	s := ag.sessions.Get(key)
	if s.Title != "Manual title" || s.TitleSource != "manual" {
		t.Fatalf("expected sticky manual title, got %q (%q)", s.Title, s.TitleSource)
	}
}

func TestArchivePreservesTitleAndRestoreCarriesSource(t *testing.T) {
	ag, _, _ := newTitleTestLoop(t)
	key := "telegram:456"
	ag.sessions.GetOrCreate(key)
	cur := ag.sessions.Get(key)
	cur.History = []string{"user: fix the wire protocol", "assistant: done"}
	ag.sessions.SetTitle(key, "Wire Protocol Fix", "manual")

	ag.archiveSession(key)

	archived := ag.sessions.ListByPrefix(key + ":archive:")
	if len(archived) != 1 {
		t.Fatalf("expected 1 archived session, got %d", len(archived))
	}
	if archived[0].Title != "Wire Protocol Fix" || archived[0].TitleSource != "manual" {
		t.Fatalf("archive lost title/source: %+v", archived[0])
	}
}

func TestGenerateSessionSummaryFallback(t *testing.T) {
	cases := []struct {
		name    string
		history []string
		want    string
	}{
		{"first user line", []string{"user: fix the flux capacitor\nmore detail", "assistant: ok"}, "fix the flux capacitor"},
		{"long line truncated", []string{"user: " + strings.Repeat("x", 80)}, strings.Repeat("x", 50) + "..."},
		{"no user entry", []string{"assistant: hello"}, "Untitled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := generateSessionSummary(tc.history)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestSetArchivedSessionTitleIndex(t *testing.T) {
	ag, _, _ := newTitleTestLoop(t)
	key := "telegram:456"
	ag.sessions.GetOrCreate(key)
	cur := ag.sessions.Get(key)
	cur.History = []string{"user: one"}
	ag.sessions.SetTitle(key, "First", "manual")
	ag.archiveSession(key)

	ag.sessions.GetOrCreate(key)
	cur2 := ag.sessions.Get(key)
	cur2.History = []string{"user: two"}
	ag.sessions.SetTitle(key, "Second", "manual")
	ag.archiveSession(key)

	// Display order is newest-first, so index 0 (display #1) is "Second"
	// (the most recently archived session). Index is 0-based.
	if old, ok := ag.SetArchivedSessionTitle(key, 0, "Renamed Second"); !ok {
		t.Fatal("expected rename to succeed")
	} else if old != "Second" {
		t.Fatalf("expected old title 'Second', got %q", old)
	}

	// Out of range must fail.
	if _, ok := ag.SetArchivedSessionTitle(key, 9, "Nope"); ok {
		t.Fatal("expected out-of-range rename to fail")
	}

	archived := ag.sessions.ListByPrefix(key + ":archive:")
	if len(archived) != 2 {
		t.Fatalf("expected 2 archived sessions, got %d", len(archived))
	}
	found := false
	for _, s := range archived {
		if s.Title == "Renamed Second" {
			found = true
		}
	}
	if !found {
		t.Fatal("renamed archive not found")
	}
}

func TestTitleCommandViaHub(t *testing.T) {
	ag, p, b := newTitleTestLoop(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go ag.Run(ctx)

	key := "telegram:456"

	// Title the current session.
	select {
	case b.In <- chat.Inbound{Channel: "telegram", SenderID: "u", ChatID: "456", Content: "/title Session Management Overhaul"}:
	default:
		t.Fatal("couldn't send /title")
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case out := <-b.Out:
			if strings.Contains(out.Content, "Current session titled") {
				goto titled
			}
		case <-deadline:
			t.Fatal("no confirmation for /title")
		}
	}
titled:
	s := ag.sessions.Get(key)
	if s == nil || s.Title != "Session Management Overhaul" || s.TitleSource != "manual" {
		t.Fatalf("expected manual title on current session, got %+v", s)
	}

	// The /title command must NOT have invoked the LLM.
	if p.called != 0 {
		t.Fatalf("LLM was called for a /title command: %d calls", p.called)
	}
}
