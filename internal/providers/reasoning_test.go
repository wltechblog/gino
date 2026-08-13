package providers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/config"
)

func TestNormalizeReasoningEffort(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{in: "none", want: "none", wantOK: true},
		{in: "low", want: "low", wantOK: true},
		{in: "medium", want: "medium", wantOK: true},
		{in: "high", want: "high", wantOK: true},
		{in: " HIGH ", want: "high", wantOK: true},
		{in: "", wantOK: false},
		{in: "extreme", wantOK: false},
	}

	for _, tt := range tests {
		got, ok := NormalizeReasoningEffort(tt.in)
		if ok != tt.wantOK || got != tt.want {
			t.Errorf("NormalizeReasoningEffort(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestSetReasoningEffortUnsupportedProvider(t *testing.T) {
	p := NewStubProvider()
	if SetReasoningEffort(p, "high") {
		t.Fatal("stub provider should not support reasoning control")
	}
	if _, ok := GetReasoningEffort(p); ok {
		t.Fatal("stub provider should not report reasoning effort")
	}
}

func TestOpenAIRequestIncludesReasoningEffort(t *testing.T) {
	var gotEffort string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var payload struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		gotEffort = payload.ReasoningEffort
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer h.Close()

	p := NewOpenAIProvider("test-key", h.URL, 5, 0)
	p.Client = &http.Client{Timeout: 5 * time.Second}
	p.SetReasoningEffort("high")

	_, err := p.Chat(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, "model-x")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want high", gotEffort)
	}
}

func TestOpenAIRequestOmitsUnsetReasoningEffort(t *testing.T) {
	var raw map[string]any
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer h.Close()

	p := NewOpenAIProvider("test-key", h.URL, 5, 0)
	p.Client = &http.Client{Timeout: 5 * time.Second}

	_, err := p.Chat(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, "model-x")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, ok := raw["reasoning_effort"]; ok {
		t.Fatalf("unset reasoning effort should be omitted, payload=%v", raw)
	}
}

func TestNewProviderFromConfigAppliesReasoningEffort(t *testing.T) {
	cfg := config.Config{}
	cfg.Providers.OpenAI = &config.ProviderConfig{APIKey: "test", ReasoningEffort: "low"}
	p := NewProviderFromConfig(cfg)

	effort, ok := GetReasoningEffort(p)
	if !ok {
		t.Fatal("expected OpenAI provider to support reasoning control")
	}
	if effort != "low" {
		t.Fatalf("config reasoning effort = %q, want low", effort)
	}

	if !SetReasoningEffort(p, "high") {
		t.Fatal("runtime override should succeed")
	}
	effort, _ = GetReasoningEffort(p)
	if effort != "high" {
		t.Fatalf("runtime override = %q, want high", effort)
	}
	if cfg.Providers.OpenAI.ReasoningEffort != "low" {
		t.Fatalf("runtime override mutated config: %q", cfg.Providers.OpenAI.ReasoningEffort)
	}
}

func TestFallbackReasoningOverride(t *testing.T) {
	primary := NewOpenAIProvider("k", "http://example", 1, 0)
	fallback := NewOpenAIProvider("k", "http://example", 1, 0)
	primary.SetReasoningEffort("low")
	fallback.SetReasoningEffort("none")

	fp := NewFallbackProvider(primary, []FallbackEntry{{Provider: fallback, Model: "fb", Name: "fb"}})
	if !SetReasoningEffort(fp, "medium") {
		t.Fatal("fallback provider should support reasoning control")
	}
	if primary.GetReasoningEffort() != "medium" || fallback.GetReasoningEffort() != "medium" {
		t.Fatalf("primary=%q fallback=%q", primary.GetReasoningEffort(), fallback.GetReasoningEffort())
	}
}
