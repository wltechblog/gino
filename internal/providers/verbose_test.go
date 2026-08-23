package providers

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestVerboseLogsRequestResponse verifies that verbose mode dumps the request
// body and the decoded response (including usage) as JSON log lines.
func TestVerboseLogsRequestResponse(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":50}}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL, 60, 0)
	p.SetVerbose(true)
	msgs := []Message{{Role: "user", Content: "hello"}}
	resp, err := p.Chat(context.Background(), msgs, nil, "test-model")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Usage == nil || resp.Usage.CachedPromptTokens != 50 {
		t.Fatalf("usage not decoded: %+v", resp.Usage)
	}
	out := logs.String()
	if !strings.Contains(out, "LLM REQUEST") || !strings.Contains(out, "LLM RESPONSE") {
		t.Fatalf("verbose labels missing:\n%s", out)
	}
	if !strings.Contains(out, `"model": "test-model"`) {
		t.Fatalf("request JSON missing model:\n%s", out)
	}
	if !strings.Contains(out, `"cached_tokens": 50`) {
		t.Fatalf("response JSON missing usage details:\n%s", out)
	}
}

// TestVerboseOffByDefault verifies no verbose dumps without the flag.
func TestVerboseOffByDefault(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL, 60, 0)
	if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, nil, "m"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Contains(logs.String(), "LLM REQUEST") {
		t.Fatalf("verbose dump present without flag:\n%s", logs.String())
	}
}
