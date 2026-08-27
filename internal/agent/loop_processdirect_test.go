package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// provider that issues a write_memory tool call on first Chat, and returns a final reply on second
type writeMemoryCallingProvider struct {
	calls int
}

func (p *writeMemoryCallingProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	p.calls++
	// verify tools include write_memory
	found := false
	for _, t := range tools {
		if t.Name == "write_memory" {
			found = true
			break
		}
	}
	if !found {
		return providers.LLMResponse{}, nil
	}

	if p.calls == 1 {
		args := map[string]interface{}{"target": "today", "content": "Test note", "append": true}
		tc := providers.ToolCall{ID: "1", Name: "write_memory", Arguments: args}
		return providers.LLMResponse{Content: "", HasToolCalls: true, ToolCalls: []providers.ToolCall{tc}}, nil
	}
	return providers.LLMResponse{Content: "Done", HasToolCalls: false}, nil
}
func (p *writeMemoryCallingProvider) GetDefaultModel() string { return "test" }
func (p *writeMemoryCallingProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

func TestProcessDirectExecutesToolCall(t *testing.T) {
	b := chat.NewHub(10)
	prov := &writeMemoryCallingProvider{}
	ag := NewAgentLoop(b, prov, prov.GetDefaultModel(), 5, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")

	resp, err := ag.ProcessDirect("please remember Test note", 2*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Done" {
		t.Fatalf("expected final response 'Done', got '%s'", resp)
	}

	// Verify memory was written to today's note
	mem := ag.memory
	td, err := mem.ReadToday()
	if err != nil {
		t.Fatalf("reading today failed: %v", err)
	}
	if td == "" || !contains(td, "Test note") {
		t.Fatalf("expected today's note to contain Test note, got: %s", td)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// toolLoopProvider always issues a tool call and never returns a final reply,
// so the turn only ends by hitting the max-iteration limit.
type toolLoopProvider struct {
	calls int
}

func (p *toolLoopProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	p.calls++
	args := map[string]interface{}{"query": "anything"}
	tc := providers.ToolCall{ID: fmt.Sprintf("c%d", p.calls), Name: "exec", Arguments: args}
	return providers.LLMResponse{Content: "", HasToolCalls: true, ToolCalls: []providers.ToolCall{tc}}, nil
}
func (p *toolLoopProvider) GetDefaultModel() string { return "test" }
func (p *toolLoopProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return 0, nil
}

func TestProcessDirectMaxIterationsNotice(t *testing.T) {
	b := chat.NewHub(10)
	prov := &toolLoopProvider{}
	ag := NewAgentLoop(b, prov, prov.GetDefaultModel(), 3, "", nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")

	resp, err := ag.ProcessDirect("keep working", 10*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp, "Max tool iterations reached") {
		t.Fatalf("expected limit notice, got %q", resp)
	}
	if !strings.Contains(resp, "continue") {
		t.Fatalf("notice should suggest continuing, got %q", resp)
	}
	if prov.calls != 3 {
		t.Fatalf("expected 3 LLM calls (maxIterations), got %d", prov.calls)
	}
}
