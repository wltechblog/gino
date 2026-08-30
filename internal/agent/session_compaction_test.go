package agent

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/wltechblog/gino/internal/agent/tools"
	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// summaryProvider records summarize calls and returns a fixed summary.
type summaryProvider struct {
	mu    sync.Mutex
	calls int
	last  []providers.Message
}

func (p *summaryProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.last = messages
	return providers.LLMResponse{Content: "COMPACTED: user worked on backup pool dedup"}, nil
}
func (p *summaryProvider) GetDefaultModel() string                            { return "test" }
func (p *summaryProvider) GetModelContext(ctx context.Context, m string) (int, error) { return 0, nil }

func newLoopForCompaction(t *testing.T, prov providers.LLMProvider) *AgentLoop {
	t.Helper()
	b := chat.NewHub(10)
	ag := NewAgentLoop(b, prov, "test", 5, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	t.Cleanup(func() { ag.Close() })
	return ag
}

func TestMaybeCompactSessionTriggersBackgroundSummary(t *testing.T) {
	prov := &summaryProvider{}
	ag := newLoopForCompaction(t, prov)
	enabled := true
	ag.SetSessionCompaction(&config.SessionCompactionConfig{
		Enabled:    &enabled,
		CompactAt:  5,
		KeepRecent: 2,
	})
	if ag.sessComp == nil {
		t.Fatal("SetSessionCompaction did not install compactor")
	}

	s := ag.sessions.GetOrCreate("comp:test")
	for i := 0; i < 3; i++ {
		s.AddMessage("user", "question"+string(rune('0'+i)))
		s.AddMessage("assistant", "answer"+string(rune('0'+i)))
	}
	ag.sessions.Save(s)

	ag.maybeCompactSession("comp:test")
	ag.bgWG.Wait()

	n, ok := ag.sessions.CheckHistoryLength("comp:test")
	if !ok {
		t.Fatal("session disappeared")
	}
	if n != 3 { // summary + 2 kept
		t.Fatalf("history length %d, want 3", n)
	}
	got := ag.sessions.Get("comp:test")
	if !strings.Contains(got.History[0], "COMPACTED") {
		t.Errorf("summary entry missing: %q", got.History[0])
	}
	if got.History[len(got.History)-1] != "assistant: answer2" {
		t.Errorf("recent tail not preserved: %q", got.History[len(got.History)-1])
	}
	if prov.calls != 1 {
		t.Errorf("summarizer called %d times, want 1", prov.calls)
	}
}

func TestMaybeCompactSessionNoopWhenShort(t *testing.T) {
	prov := &summaryProvider{}
	ag := newLoopForCompaction(t, prov)
	enabled := true
	ag.SetSessionCompaction(&config.SessionCompactionConfig{Enabled: &enabled, CompactAt: 40, KeepRecent: 16})

	s := ag.sessions.GetOrCreate("short:session")
	s.AddMessage("user", "hi")
	ag.sessions.Save(s)

	ag.maybeCompactSession("short:session")
	ag.bgWG.Wait()
	if prov.calls != 0 {
		t.Errorf("summarizer called for short session (%d calls)", prov.calls)
	}
}

func TestSessionCompactorDisabledByDefault(t *testing.T) {
	if c := newSessionCompactor(nil, "m", config.SessionCompactionConfig{}); c != nil {
		t.Error("nil Enabled should disable compaction")
	}
	off := false
	if c := newSessionCompactor(nil, "m", config.SessionCompactionConfig{Enabled: &off}); c != nil {
		t.Error("Enabled=false should disable compaction")
	}
}

func TestOverflowToolResultSavesToFile(t *testing.T) {
	ag := newLoopForCompaction(t, &summaryProvider{})
	ws := t.TempDir()
	fst, err := tools.NewFilesystemTool(ws, nil, config.SandboxConfig{})
	if err != nil {
		t.Fatalf("filesystem tool: %v", err)
	}
	ag.fsTool = fst
	ag.maxToolResultChars = 100

	big := strings.Repeat("X", 100) + strings.Repeat("Y", 9000) // 9100 chars
	got := ag.overflowToolResult(big)

	if !strings.HasPrefix(got, "XXXXXXXXXX") {
		t.Error("head missing")
	}
	if !strings.Contains(got, "overflowed the 100-char context budget") {
		t.Errorf("overflow notice missing: %q", got[len(got)-200:])
	}
	marker := "Full output saved to: "
	idx := strings.LastIndex(got, marker)
	if idx < 0 {
		t.Fatalf("path missing from result: %q", got[len(got)-200:])
	}
	rest := got[idx+len(marker):]
	end := strings.Index(rest, " —")
	if end < 0 {
		t.Fatalf("path terminator missing: %q", rest)
	}
	path := rest[:end]
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("overflow file unreadable: %v", err)
	}
	if string(data) != big {
		t.Errorf("overflow file length %d, want %d (content mismatch)", len(data), len(big))
	}
}

func TestOverflowToolResultPassthrough(t *testing.T) {
	ag := newLoopForCompaction(t, &summaryProvider{})
	ag.fsTool = nil
	ag.maxToolResultChars = 100

	small := "tiny result"
	if got := ag.overflowToolResult(small); got != small {
		t.Errorf("small result modified: %q", got)
	}
	// nil fsTool falls back to plain truncation, never errors
	big := strings.Repeat("Z", 500)
	got := ag.overflowToolResult(big)
	if !strings.Contains(got, "... [truncated 400 chars]") {
		t.Errorf("truncation fallback missing: %q", got[:60])
	}
}

func TestTruncateToolResultUnchanged(t *testing.T) {
	if got := truncateToolResult("abc", 10); got != "abc" {
		t.Errorf("got %q", got)
	}
}
