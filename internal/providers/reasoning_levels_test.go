package providers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeReasoningEffortIn(t *testing.T) {
	// Custom vocabulary
	if _, ok := NormalizeReasoningEffortIn("minimal", []string{"minimal", "low", "high"}); !ok {
		t.Error("minimal should be allowed in custom vocabulary")
	}
	if _, ok := NormalizeReasoningEffortIn("medium", []string{"minimal", "low", "high"}); ok {
		t.Error("medium should be rejected when not in custom vocabulary")
	}
	if v, ok := NormalizeReasoningEffortIn(" HIGH ", []string{"low", "high"}); !ok || v != "high" {
		t.Errorf("trim+lowercase failed: %q %v", v, ok)
	}
	if _, ok := NormalizeReasoningEffortIn("high", []string{}); !ok {
		t.Error("empty levels slice should fall back to defaults")
	}
	// Defaults now include minimal
	if _, ok := NormalizeReasoningEffort("minimal"); !ok {
		t.Error("minimal should be in the default vocabulary")
	}
	if _, ok := NormalizeReasoningEffort("extreme"); ok {
		t.Error("extreme should never be valid")
	}
}

func TestSetReasoningLevels(t *testing.T) {
	p := NewOpenAIProvider("k", "http://localhost:1", 30, 0)

	if got := p.GetReasoningLevels(); len(got) == 0 || got[0] != "none" {
		t.Errorf("default vocabulary expected, got %v", got)
	}
	if !p.ReasoningEffortAllowed("minimal") {
		t.Error("minimal should be allowed under defaults")
	}

	p.SetReasoningLevels([]string{" Turbo ", "", "EXTREME"})
	got := p.GetReasoningLevels()
	if len(got) != 2 || got[0] != "turbo" || got[1] != "extreme" {
		t.Errorf("levels not normalized: %v", got)
	}
	if !p.ReasoningEffortAllowed("TURBO") {
		t.Error("case-insensitive membership failed")
	}
	if p.ReasoningEffortAllowed("high") {
		t.Error("high should not be allowed after vocabulary override")
	}

	p.SetReasoningLevels(nil)
	if got := p.GetReasoningLevels(); len(got) != len(DefaultReasoningLevels) {
		t.Errorf("nil should restore defaults, got %v", got)
	}
}

func TestFallbackProviderReasoningLevelsForward(t *testing.T) {
	primary := NewOpenAIProvider("k", "http://localhost:1", 30, 0)
	primary.SetReasoningLevels([]string{"think", "think_hard"})
	fb := NewFallbackProvider(primary, nil)

	levels := fb.GetReasoningLevels()
	if len(levels) != 2 || levels[0] != "think" {
		t.Errorf("fallback did not forward primary vocabulary: %v", levels)
	}
	if !fb.ReasoningEffortAllowed("think_hard") {
		t.Error("fallback membership check failed")
	}
	if fb.ReasoningEffortAllowed("medium") {
		t.Error("medium should not be allowed through custom vocabulary")
	}
}

func TestStubProviderLevelsDefaults(t *testing.T) {
	// Unsupported providers fall back to the default vocabulary via the
	// package-level helpers.
	p := NewStubProvider()
	if got := ReasoningLevelsOf(p); len(got) == 0 || got[0] != "none" {
		t.Errorf("ReasoningLevelsOf(stub) should return defaults, got %v", got)
	}
	if !ReasoningEffortAllowedOn(p, "high") {
		t.Error("high should be allowed on stub via defaults")
	}
	if strings.Join(ReasoningLevelsOf(p), ",") == "" {
		t.Error("empty vocabulary")
	}
}

func TestReasoningEffortReachesRequestBodyWithCustomLevels(t *testing.T) {
	var gotEffort string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode: %v", err)
		}
		gotEffort = payload.ReasoningEffort
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer h.Close()

	p := NewOpenAIProvider("k", h.URL, 30, 0)
	p.SetReasoningLevels([]string{"turbo", " Ludicrous "})
	// SetReasoningEffort itself does not gate (caller validates); the wire
	// carries whatever was set — this test pins the contract end to end.
	p.SetReasoningEffort("ludicrous")

	if _, err := p.Chat(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, "test-model"); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gotEffort != "ludicrous" {
		t.Errorf("reasoning_effort not carried on the wire: %q", gotEffort)
	}
}
