package providers

import (
	"context"
	"log"
	"sync"
	"time"
)

// FallbackEntry wraps a provider with its own model and recovery timer.
type FallbackEntry struct {
	Provider     LLMProvider
	Model        string
	Name         string
	RecoverAfter time.Duration
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
// Context handling:
//   - The primary gets the original context (may have a deadline).
//   - Each fallback gets a fresh context via detachDeadline() that inherits
//     cancellation (so /stop still works) but NOT the deadline. This ensures
//     fallbacks get their own time budget via per-attempt timeouts, even after
//     the primary exhausted the original deadline with retries.
type FallbackProvider struct {
	primary   LLMProvider
	entries   []FallbackEntry
	mu        sync.Mutex
	activeIdx int       // -1 = primary, 0+ = fallback index
	failSince time.Time // when we last switched away from primary
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

// GetModelContext delegates to the primary provider.
func (f *FallbackProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	return f.primary.GetModelContext(ctx, model)
}

// SetReasoningEffort applies a runtime reasoning override to the whole chain.
func (f *FallbackProvider) SetReasoningEffort(effort string) {
	SetReasoningEffort(f.primary, effort)

	for _, entry := range f.entries {
		SetReasoningEffort(entry.Provider, effort)
	}
}

// GetReasoningEffort reports the primary provider's current reasoning setting.
func (f *FallbackProvider) GetReasoningEffort() string {
	effort, _ := GetReasoningEffort(f.primary)
	return effort
}

// Chat sends messages to the active provider, with automatic fallback and recovery.
func (f *FallbackProvider) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string) (LLMResponse, error) {
	// Check if we should attempt recovery to primary
	f.mu.Lock()
	shouldTryPrimary := f.shouldRecoverLocked()
	f.mu.Unlock()

	if shouldTryPrimary {
		// Recovery attempt: give primary a fresh context too
		freshCtx, cancel := detachDeadline(ctx)
		defer cancel()
		resp, err := f.primary.Chat(freshCtx, messages, tools, model)
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
		// Primary failed — try fallbacks with fresh contexts
		log.Printf("LLM: primary failed: %v, trying fallbacks", err)
		return f.tryFallbacks(ctx, messages, tools, model, err)
	}

	// We're on a fallback, try it with a fresh context
	freshCtx, cancel := detachDeadline(ctx)
	defer cancel()
	resp, err := f.entries[idx].Provider.Chat(freshCtx, messages, tools, f.entries[idx].Model)
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
// Each fallback gets a fresh context with no inherited deadline so that
// per-attempt timeouts give it a clean time budget.
func (f *FallbackProvider) tryFallbacksFrom(ctx context.Context, messages []Message, tools []ToolDefinition, startIdx int, lastErr error) (LLMResponse, error) {
	for i := startIdx; i < len(f.entries); i++ {
		entry := f.entries[i]

		log.Printf("LLM: trying fallback %q (%s)", entry.Name, entry.Model)

		// Fresh context: inherits cancellation but NOT deadline
		freshCtx, cancel := detachDeadline(ctx)

		resp, err := entry.Provider.Chat(freshCtx, messages, tools, entry.Model)
		cancel()

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

// detachDeadline returns a context that inherits values and cancellation
// from the parent but does NOT inherit the parent's deadline.
//
// This is critical for fallback: when the primary provider exhausts the
// original deadline with retry attempts, fallback providers still get a
// fresh time budget (via per-attempt timeouts in OpenAIProvider).
//
// If the parent context is cancelled (e.g. /stop), the detached context
// is also cancelled, so user-initiated cancellation still propagates.
func detachDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	// context.WithoutCancel (Go 1.21+) strips both deadline and cancellation
	fresh := context.WithoutCancel(ctx)
	// Re-attach cancellation from parent so /stop still works
	freshCtx, cancel := context.WithCancel(fresh)
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-freshCtx.Done():
		}
	}()
	return freshCtx, cancel
}
