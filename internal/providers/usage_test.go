package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUsageDecodedFromResponse verifies that the usage block (including
// prompt_tokens_details.cached_tokens) is decoded and surfaced on LLMResponse.
func TestUsageDecodedFromResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
			"usage": {
				"prompt_tokens": 12400,
				"completion_tokens": 850,
				"total_tokens": 13250,
				"prompt_tokens_details": {"cached_tokens": 10240},
				"completion_tokens_details": {"reasoning_tokens": 300}
			}
		}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL, 5, 16384)
	resp, err := p.Chat(context.Background(), nil, nil, "test-model")
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Usage == nil {
		t.Fatal("Usage is nil — not decoded from response")
	}
	if resp.Usage.PromptTokens != 12400 {
		t.Errorf("PromptTokens = %d, want 12400", resp.Usage.PromptTokens)
	}
	if resp.Usage.CachedPromptTokens != 10240 {
		t.Errorf("CachedPromptTokens = %d, want 10240", resp.Usage.CachedPromptTokens)
	}
	if resp.Usage.ReasoningTokens != 300 {
		t.Errorf("ReasoningTokens = %d, want 300", resp.Usage.ReasoningTokens)
	}
	if resp.Usage.CompletionTokens != 850 || resp.Usage.TotalTokens != 13250 {
		t.Errorf("CompletionTokens=%d TotalTokens=%d, want 850/13250", resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	}
}

// TestUsageAbsentIsNil verifies graceful handling when the host omits usage.
func TestUsageAbsentIsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}]}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL, 5, 16384)
	resp, err := p.Chat(context.Background(), nil, nil, "test-model")
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Usage != nil {
		t.Errorf("expected nil Usage when host omits it, got %+v", resp.Usage)
	}
}
