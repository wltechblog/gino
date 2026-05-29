package providers

import (
	"context"
	"log"
	"sync"
	"time"
)

// FallbackEntry wraps a provider with its own model and recovery timer.
type FallbackEntry struct {
	Provider      LLMProvider
	Model         string
	Name          string
	RecoverAfter  time.Duration
	failedAt      time.Time // when the PRIMARY failed, causing us to activate this fallback
}

// FallbackProvider wraps a primary provider with optional fallback providers.
// When the primary fails (after its own retries), it tries each fallback in order.
// Once on a fallback, it automatically retries the primary after RecoverAfter.
//
// Recovery behavior:
//   - After primary fails → immediately switch to first fallback
//   - After RecoverAfter on a fallback → try primary again on next request
//   - If primary succeeds → stay on primary
//   - If primary fails again → back to fallback, reset RecoverAfter timer
//
// This ensures we never get "stuck" on a fallback longer than RecoverAfter.
type FallbackProvider struct {
	primary    LLMProvider
	entries    []FallbackEntry
	mu         sync.Mutex
	activeIdx  int       // -1 = primary, 0+ = fallback index
	failSince  time.Time // when we last switched away from primary
}

// NewFallbackProvider creates a provider chain with automatic primary recovery.
// If entries is empty, it acts as a passthrough to the primary.
func NewFallbackProvider(primary LLMProvider, entries []FallbackEntry) *FallbackProvider {
	return &FallbackProvider{
		primary:   primary,
		entries:   entries,
		activeIdx: -1, // start on primary
	}
}

// GetDefaultModel returns the primary provider's default model.
func (f *FallbackProvider) GetDefaultModel() string {
	return f.primary.GetDefaultModel()
}

// Chat sends messages to the active provider, with automatic fallback and recovery.
func (f *FallbackProvider) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string) (LLMResponse, error) {
	// Check if we should attempt recovery to primary
	f.mu.Lock()
	shouldTryPrimary := f.shouldRecoverLocked()
	f.mu.Unlock()

	if shouldTryPrimary {
		resp, err := f.primary.Chat(ctx, messages, tools, model)
		if err == nil {
			// Primary recovered!
			f.mu.Lock()
			f.activeIdx = -1
			f.failSince = time.Time{}
			f.mu.Unlock()
			log.Printf("LLM: recovered to primary provider")
			return resp, nil
		}
		// Primary still failing, log and continue to fallback
		log.Printf("LLM: primary still failing (recovery attempt): %v", err)
	}

	// Try current active provider first
	f.mu.Lock()
	idx := f.activeIdx
	f.mu.Unlock()

	if idx == -1 {
		// We're on primary, try it
		resp, err := f.primary.Chat(ctx, messages, tools, model)
		if err == nil {
			return resp, nil
		}
		// Primary failed — try fallbacks
		log.Printf("LLM: primary failed: %v, trying fallbacks", err)
		return f.tryFallbacks(ctx, messages, tools, model, err)
	}

	// We're on a fallback, try it
	resp, err := f.entries[idx].Provider.Chat(ctx, messages, tools, f.entries[idx].Model)
	if err == nil {
		return resp, nil
	}

	// Current fallback failed, try the next ones
	log.Printf("LLM: fallback %q failed: %v, trying next", f.entries[idx].Name, err)
	return f.tryFallbacksFrom(ctx, messages, tools, idx+1, err)
}

// shouldRecoverLocked reports whether we should try the primary again.
// Must be called with f.mu held.
func (f *FallbackProvider) shouldRecoverLocked() bool {
	if f.activeIdx == -1 {
		return false // already on primary
	}
	if len(f.entries) == 0 {
		return false
	}

	entry := f.entries[f.activeIdx]
	if entry.RecoverAfter == 0 {
		return true // "0s" = retry on every request
	}
	return time.Since(f.failSince) >= entry.RecoverAfter
}

// tryFallbacks tries each fallback in order starting from index 0.
func (f *FallbackProvider) tryFallbacks(ctx context.Context, messages []Message, tools []ToolDefinition, model string, primaryErr error) (LLMResponse, error) {
	return f.tryFallbacksFrom(ctx, messages, tools, 0, primaryErr)
}

// tryFallbacksFrom tries each fallback starting from the given index.
func (f *FallbackProvider) tryFallbacksFrom(ctx context.Context, messages []Message, tools []ToolDefinition, startIdx int, lastErr error) (LLMResponse, error) {
	for i := startIdx; i < len(f.entries); i++ {
		entry := f.entries[i]

		log.Printf("LLM: trying fallback %q (%s)", entry.Name, entry.Model)
		resp, err := entry.Provider.Chat(ctx, messages, tools, entry.Model)
		if err == nil {
			// Success — switch to this fallback
			f.mu.Lock()
			f.activeIdx = i
			if f.failSince.IsZero() {
				f.failSince = time.Now()
			}
			f.mu.Unlock()
			log.Printf("LLM: switched to fallback %q (will retry primary after %v)",
				entry.Name, entry.RecoverAfter)
			return resp, nil
		}
		log.Printf("LLM: fallback %q failed: %v", entry.Name, err)
		lastErr = err
	}

	// All fallbacks exhausted
	return LLMResponse{}, lastErr
}

// ActiveProvider returns the name of the currently active provider for logging.
func (f *FallbackProvider) ActiveProvider() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activeIdx == -1 {
		return "primary"
	}
	return f.entries[f.activeIdx].Name
}
