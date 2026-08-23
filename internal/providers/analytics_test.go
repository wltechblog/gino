package providers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestAnalyticsLogsUsageJSON verifies that analytics mode emits the per-request
// token usage as a JSON log line (LLM USAGE) including cache info.
func TestAnalyticsLogsUsageJSON(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12400,"completion_tokens":850,"total_tokens":13250,"prompt_tokens_details":{"cached_tokens":10240},"completion_tokens_details":{"reasoning_tokens":300}}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL, 60, 0)
	p.SetAnalytics(true)
	resp, err := p.Chat(context.Background(), nil, nil, "test-model")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Usage == nil || resp.Usage.CachedPromptTokens != 10240 {
		t.Fatalf("usage not decoded: %+v", resp.Usage)
	}

	out := logs.String()
	idx := strings.Index(out, "LLM USAGE")
	if idx < 0 {
		t.Fatalf("LLM USAGE line missing:\n%s", out)
	}
	// Extract the JSON object after the label.
	jsonStr := out[idx:]
	start := strings.Index(jsonStr, "{")
	if start < 0 {
		t.Fatalf("LLM USAGE payload has no JSON object:\n%s", jsonStr)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr[start:]), &payload); err != nil {
		t.Fatalf("LLM USAGE payload is not valid JSON: %v\n%s", err, jsonStr)
	}
	if payload["cached"] != true {
		t.Errorf("cached = %v, want true (cached_tokens=10240)", payload["cached"])
	}
	if payload["model"] != "test-model" {
		t.Errorf("model = %v, want test-model", payload["model"])
	}
	usage, ok := payload["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("usage missing from payload: %v", payload)
	}
	if usage["promptTokens"].(float64) != 12400 {
		t.Errorf("promptTokens = %v, want 12400", usage["promptTokens"])
	}
	if usage["cachedPromptTokens"].(float64) != 10240 {
		t.Errorf("cachedPromptTokens = %v, want 10240", usage["cachedPromptTokens"])
	}
	if usage["reasoningTokens"].(float64) != 300 {
		t.Errorf("reasoningTokens = %v, want 300", usage["reasoningTokens"])
	}
}

// TestAnalyticsOffByDefault verifies no usage log line without the flag.
func TestAnalyticsOffByDefault(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105}}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL, 60, 0)
	if _, err := p.Chat(context.Background(), nil, nil, "test-model"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Contains(logs.String(), "LLM USAGE") {
		t.Fatalf("analytics leaked without flag:\n%s", logs.String())
	}
}

// TestAnalyticsCacheMissFlag verifies the cached boolean flips to false when
// cached_tokens is 0 (cache miss).
func TestAnalyticsCacheMissFlag(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":500,"completion_tokens":10,"total_tokens":510,"prompt_tokens_details":{"cached_tokens":0}}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL, 60, 0)
	p.SetAnalytics(true)
	if _, err := p.Chat(context.Background(), nil, nil, "test-model"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	out := logs.String()
	idx := strings.Index(out, "LLM USAGE")
	if idx < 0 {
		t.Fatalf("LLM USAGE line missing:\n%s", out)
	}
	var payload map[string]interface{}
	jsonStr := out[idx:]
	start := strings.Index(jsonStr, "{")
	if start < 0 {
		t.Fatalf("LLM USAGE payload has no JSON object:\n%s", jsonStr)
	}
	if err := json.Unmarshal([]byte(jsonStr[start:]), &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload["cached"] != false {
		t.Errorf("cached = %v, want false on cache miss", payload["cached"])
	}
}
