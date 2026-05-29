package providers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// mockProvider is a test provider that can be configured to succeed or fail.
type mockProvider struct {
	model      string
	calls      atomic.Int32
	shouldFail atomic.Bool
	response   string
}

func newMockProvider(model, response string) *mockProvider {
	return &mockProvider{model: model, response: response}
}

func (m *mockProvider) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string) (LLMResponse, error) {
	m.calls.Add(1)
	if m.shouldFail.Load() {
		return LLMResponse{}, errors.New("mock error")
	}
	return LLMResponse{Content: m.response}, nil
}

func (m *mockProvider) GetDefaultModel() string { return m.model }

func TestFallbackProvider_PrimaryOnly(t *testing.T) {
	primary := newMockProvider("primary-model", "primary response")
	fb := NewFallbackProvider(primary, nil)

	resp, err := fb.Chat(context.Background(), nil, nil, "primary-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "primary response" {
		t.Errorf("expected 'primary response', got %q", resp.Content)
	}
	if primary.calls.Load() != 1 {
		t.Errorf("expected 1 call to primary, got %d", primary.calls.Load())
	}
}

func TestFallbackProvider_FallsBackOnPrimaryFailure(t *testing.T) {
	primary := newMockProvider("primary-model", "")
	primary.shouldFail.Store(true)

	fallback := newMockProvider("fallback-model", "fallback response")

	fb := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fallback, Model: "fallback-model", Name: "cheap", RecoverAfter: 5 * time.Minute},
	})

	resp, err := fb.Chat(context.Background(), nil, nil, "primary-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "fallback response" {
		t.Errorf("expected 'fallback response', got %q", resp.Content)
	}
	if primary.calls.Load() != 1 {
		t.Errorf("expected 1 call to primary, got %d", primary.calls.Load())
	}
	if fallback.calls.Load() != 1 {
		t.Errorf("expected 1 call to fallback, got %d", fallback.calls.Load())
	}
}

func TestFallbackProvider_TriesNextOnFallbackFailure(t *testing.T) {
	primary := newMockProvider("primary-model", "")
	primary.shouldFail.Store(true)

	fb1 := newMockProvider("fb1-model", "")
	fb1.shouldFail.Store(true)

	fb2 := newMockProvider("fb2-model", "fb2 response")

	fb := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb1, Model: "fb1-model", Name: "fb1", RecoverAfter: 5 * time.Minute},
		{Provider: fb2, Model: "fb2-model", Name: "fb2", RecoverAfter: 5 * time.Minute},
	})

	resp, err := fb.Chat(context.Background(), nil, nil, "primary-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "fb2 response" {
		t.Errorf("expected 'fb2 response', got %q", resp.Content)
	}
}

func TestFallbackProvider_AllFail(t *testing.T) {
	primary := newMockProvider("primary-model", "")
	primary.shouldFail.Store(true)

	fallback := newMockProvider("fallback-model", "")
	fallback.shouldFail.Store(true)

	fb := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fallback, Model: "fallback-model", Name: "cheap", RecoverAfter: 5 * time.Minute},
	})

	_, err := fb.Chat(context.Background(), nil, nil, "primary-model")
	if err == nil {
		t.Fatal("expected error when all providers fail")
	}
}

func TestFallbackProvider_RecoversToPrimary(t *testing.T) {
	primary := newMockProvider("primary-model", "primary response")

	fallback := newMockProvider("fallback-model", "fallback response")

	fb := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fallback, Model: "fallback-model", Name: "cheap", RecoverAfter: 100 * time.Millisecond},
	})

	// First call: primary succeeds
	resp, err := fb.Chat(context.Background(), nil, nil, "primary-model")
	if err != nil {
		t.Fatalf("call 1: unexpected error: %v", err)
	}
	if resp.Content != "primary response" {
		t.Errorf("call 1: expected 'primary response', got %q", resp.Content)
	}

	// Fail the primary
	primary.shouldFail.Store(true)

	// Second call: primary fails, falls back
	resp, err = fb.Chat(context.Background(), nil, nil, "primary-model")
	if err != nil {
		t.Fatalf("call 2: unexpected error: %v", err)
	}
	if resp.Content != "fallback response" {
		t.Errorf("call 2: expected 'fallback response', got %q", resp.Content)
	}

	// Wait for RecoverAfter
	time.Sleep(150 * time.Millisecond)

	// Fix the primary
	primary.shouldFail.Store(false)

	// Third call: should recover to primary
	resp, err = fb.Chat(context.Background(), nil, nil, "primary-model")
	if err != nil {
		t.Fatalf("call 3: unexpected error: %v", err)
	}
	if resp.Content != "primary response" {
		t.Errorf("call 3: expected 'primary response' (recovered), got %q", resp.Content)
	}
}

func TestFallbackProvider_StayOnFallbackBeforeRecoverAfter(t *testing.T) {
	primary := newMockProvider("primary-model", "primary response")

	fallback := newMockProvider("fallback-model", "fallback response")

	fb := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fallback, Model: "fallback-model", Name: "cheap", RecoverAfter: 5 * time.Minute},
	})

	// Fail the primary
	primary.shouldFail.Store(true)

	// First call: falls back
	resp, err := fb.Chat(context.Background(), nil, nil, "primary-model")
	if err != nil {
		t.Fatalf("call 1: unexpected error: %v", err)
	}
	if resp.Content != "fallback response" {
		t.Errorf("call 1: expected 'fallback response', got %q", resp.Content)
	}

	// Fix the primary immediately
	primary.shouldFail.Store(false)

	// Second call: should still be on fallback (RecoverAfter not elapsed)
	resp, err = fb.Chat(context.Background(), nil, nil, "primary-model")
	if err != nil {
		t.Fatalf("call 2: unexpected error: %v", err)
	}
	if resp.Content != "fallback response" {
		t.Errorf("call 2: expected 'fallback response' (still on fallback), got %q", resp.Content)
	}

	// The primary should NOT have been called yet (RecoverAfter is 5 minutes)
	if primary.calls.Load() != 1 {
		t.Errorf("call 2: expected primary to have 1 call (initial failed attempt), got %d", primary.calls.Load())
	}
}

func TestFallbackProvider_AggressiveRecovery(t *testing.T) {
	// RecoverAfter = 0 means retry primary on every request
	primary := newMockProvider("primary-model", "primary response")

	fallback := newMockProvider("fallback-model", "fallback response")

	fb := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fallback, Model: "fallback-model", Name: "cheap", RecoverAfter: 0},
	})

	// Fail the primary
	primary.shouldFail.Store(true)

	// Falls back
	resp, err := fb.Chat(context.Background(), nil, nil, "primary-model")
	if err != nil {
		t.Fatalf("call 1: unexpected error: %v", err)
	}
	if resp.Content != "fallback response" {
		t.Errorf("call 1: expected 'fallback response', got %q", resp.Content)
	}

	// Fix primary
	primary.shouldFail.Store(false)

	// Next call should try primary first (aggressive recovery)
	resp, err = fb.Chat(context.Background(), nil, nil, "primary-model")
	if err != nil {
		t.Fatalf("call 2: unexpected error: %v", err)
	}
	if resp.Content != "primary response" {
		t.Errorf("call 2: expected 'primary response' (recovered), got %q", resp.Content)
	}
}

func TestFallbackProvider_ActiveProvider(t *testing.T) {
	primary := newMockProvider("primary-model", "primary response")
	fallback := newMockProvider("fallback-model", "fallback response")

	fb := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fallback, Model: "fallback-model", Name: "cheap", RecoverAfter: 5 * time.Minute},
	})

	if fb.ActiveProvider() != "primary" {
		t.Errorf("expected 'primary', got %q", fb.ActiveProvider())
	}

	primary.shouldFail.Store(true)
	fb.Chat(context.Background(), nil, nil, "primary-model")

	if fb.ActiveProvider() != "cheap" {
		t.Errorf("expected 'cheap', got %q", fb.ActiveProvider())
	}
}
