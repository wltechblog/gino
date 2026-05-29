package providers

import (
	"log"
	"time"

	"github.com/local/picobot/internal/config"
)

// NewProviderFromConfig creates a provider based on the configuration.
// If fallbacks are configured, wraps the primary in a FallbackProvider that
// automatically degrades to fallback models on failure and recovers to primary
// after each fallback's RecoverAfter duration.
func NewProviderFromConfig(cfg config.Config) LLMProvider {
	if cfg.Providers.OpenAI == nil || (cfg.Providers.OpenAI.APIKey == "" && cfg.Providers.OpenAI.APIBase == "") {
		return NewStubProvider()
	}

	primary := NewOpenAIProvider(
		cfg.Providers.OpenAI.APIKey,
		cfg.Providers.OpenAI.APIBase,
		cfg.Agents.Defaults.RequestTimeoutS,
		cfg.Agents.Defaults.MaxTokens,
	)

	if len(cfg.Providers.Fallbacks) == 0 {
		return primary
	}

	entries := make([]FallbackEntry, 0, len(cfg.Providers.Fallbacks))
	for i, fb := range cfg.Providers.Fallbacks {
		if fb.APIKey == "" && fb.APIBase == "" {
			log.Printf("Fallback %d (%q): no apiKey or apiBase, skipping", i, fb.Name)
			continue
		}
		if fb.Model == "" {
			log.Printf("Fallback %d (%q): no model specified, skipping", i, fb.Name)
			continue
		}

		recoverAfter := 5 * time.Minute // default: 5 minutes
		if fb.RecoverAfter != "" {
			if d, err := time.ParseDuration(fb.RecoverAfter); err == nil {
				recoverAfter = d
			} else {
				log.Printf("Fallback %d (%q): invalid recoverAfter %q, using default 5m", i, fb.Name, fb.RecoverAfter)
			}
		}

		maxTokens := cfg.Agents.Defaults.MaxTokens
		if fb.MaxTokens > 0 {
			maxTokens = fb.MaxTokens
		}

		provider := NewOpenAIProvider(
			fb.APIKey,
			fb.APIBase,
			cfg.Agents.Defaults.RequestTimeoutS,
			maxTokens,
		)

		name := fb.Name
		if name == "" {
			name = fb.Model
		}

		entries = append(entries, FallbackEntry{
			Provider:     provider,
			Model:        fb.Model,
			Name:         name,
			RecoverAfter: recoverAfter,
		})

		log.Printf("Fallback %q: %s (recover after %v)", name, fb.Model, recoverAfter)
	}

	if len(entries) == 0 {
		return primary
	}

	return NewFallbackProvider(primary, entries)
}
