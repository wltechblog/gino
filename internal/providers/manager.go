package providers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ProviderConfigDef is a stored provider configuration.
// It represents a single LLM provider (e.g. an OpenAI-compatible API endpoint)
// with credentials and model selection.
type ProviderConfigDef struct {
	ID          int64  `json:"id,omitempty"`
	Name        string `json:"name"`          // human-readable name (e.g. "primary", "cheap-fast")
	APIBase     string `json:"apiBase"`       // e.g. https://api.openai.com/v1
	APIKey      string `json:"apiKey"`        // secret key
	IsPrimary   bool   `json:"isPrimary"`     // marks the default provider
	MaxTokens   int    `json:"maxTokens,omitempty"`
	MaxRetries  int    `json:"maxRetries,omitempty"`
	RetryBaseWaitS int `json:"retryBaseWaitS,omitempty"`
	TimeoutS    int    `json:"timeoutS,omitempty"`
	Fallback    bool   `json:"fallback,omitempty"`    // is this a fallback provider?
	RecoverAfter string `json:"recoverAfter,omitempty"` // for fallbacks
	FallbackOrder int  `json:"fallbackOrder,omitempty"`
	// Models lists models available on this provider.
	Models []ModelDef `json:"models,omitempty"`
}

// ModelDef is a model offered by a provider.
type ModelDef struct {
	Name   string `json:"name"`           // model identifier (e.g. "glm-5.2")
	Label  string `json:"label,omitempty"` // display name
	Vision bool   `json:"vision,omitempty"`
	Default bool  `json:"default,omitempty"` // is this the default model?
}

// ProviderManager manages LLM provider configurations.
// It stores configs in SQLite and builds/caches LLMProvider instances.
// All operations are safe for concurrent use.
type ProviderManager struct {
	mu       sync.RWMutex
	db       *sql.DB
	cache    map[string]*cachedProvider // keyed by provider name
	primary  string                      // name of the primary provider
	fallback []string                    // ordered fallback provider names
	modelDefault string                  // global default model
}

type cachedProvider struct {
	config ProviderConfigDef
	impl   LLMProvider
	createdAt time.Time
}

// NewProviderManager creates a new ProviderManager using the given SQLite database.
// The database must already have the provider tables (call MigrateDB).
func NewProviderManager(db *sql.DB) *ProviderManager {
	return &ProviderManager{
		db:    db,
		cache: make(map[string]*cachedProvider),
	}
}

// MigrateDB creates the provider tables if they don't exist.
func MigrateDB(db *sql.DB) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS providers (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			name         TEXT NOT NULL UNIQUE,
			api_base     TEXT NOT NULL DEFAULT '',
			api_key      TEXT NOT NULL DEFAULT '',
			is_primary   INTEGER NOT NULL DEFAULT 0,
			max_tokens   INTEGER NOT NULL DEFAULT 0,
			max_retries  INTEGER NOT NULL DEFAULT 0,
			retry_base_wait_s INTEGER NOT NULL DEFAULT 0,
			timeout_s    INTEGER NOT NULL DEFAULT 0,
			is_fallback  INTEGER NOT NULL DEFAULT 0,
			recover_after TEXT NOT NULL DEFAULT '',
			fallback_order INTEGER NOT NULL DEFAULT 0,
			updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS provider_models (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			provider_id  INTEGER NOT NULL,
			name         TEXT NOT NULL,
			label        TEXT NOT NULL DEFAULT '',
			vision       INTEGER NOT NULL DEFAULT 0,
			is_default   INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY(provider_id) REFERENCES providers(id) ON DELETE CASCADE,
			UNIQUE(provider_id, name)
		)`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			return fmt.Errorf("provider migrate: %w", err)
		}
	}
	return nil
}

// LoadFromDB loads all provider configs from the database and builds cached instances.
func (pm *ProviderManager) LoadFromDB() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	rows, err := pm.db.Query(`SELECT id, name, api_base, api_key, is_primary, max_tokens, max_retries, retry_base_wait_s, timeout_s, is_fallback, recover_after, fallback_order FROM providers ORDER BY is_primary DESC, fallback_order ASC`)
	if err != nil {
		return fmt.Errorf("provider load: %w", err)
	}
	defer rows.Close()

	pm.cache = make(map[string]*cachedProvider)
	pm.fallback = pm.fallback[:0]
	pm.primary = ""

	var fallbackNames []string

	for rows.Next() {
		var def ProviderConfigDef
		var isPrimary, isFallback int
		if err := rows.Scan(&def.ID, &def.Name, &def.APIBase, &def.APIKey, &isPrimary, &def.MaxTokens, &def.MaxRetries, &def.RetryBaseWaitS, &def.TimeoutS, &isFallback, &def.RecoverAfter, &def.FallbackOrder); err != nil {
			return fmt.Errorf("provider scan: %w", err)
		}
		def.IsPrimary = isPrimary != 0
		def.Fallback = isFallback != 0

		// Load models for this provider
		models, err := pm.loadModelsLocked(def.ID)
		if err != nil {
			log.Printf("provider: warning: could not load models for %q: %v", def.Name, err)
		}
		def.Models = models

		impl := buildProvider(def)
		pm.cache[def.Name] = &cachedProvider{config: def, impl: impl, createdAt: time.Now()}

		if def.IsPrimary {
			pm.primary = def.Name
			for _, m := range models {
				if m.Default {
					pm.modelDefault = m.Name
				}
			}
		} else if def.Fallback {
			fallbackNames = append(fallbackNames, def.Name)
		}
	}
	pm.fallback = fallbackNames

	if pm.primary == "" && len(pm.cache) > 0 {
		log.Printf("provider: no primary set, using first provider")
	}

	log.Printf("provider: loaded %d providers (primary=%q, fallbacks=%d)", len(pm.cache), pm.primary, len(pm.fallback))
	return rows.Err()
}

func (pm *ProviderManager) loadModelsLocked(providerID int64) ([]ModelDef, error) {
	rows, err := pm.db.Query(`SELECT name, label, vision, is_default FROM provider_models WHERE provider_id = ?`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var models []ModelDef
	for rows.Next() {
		var m ModelDef
		var vision, isDefault int
		if err := rows.Scan(&m.Name, &m.Label, &vision, &isDefault); err != nil {
			return nil, err
		}
		m.Vision = vision != 0
		m.Default = isDefault != 0
		models = append(models, m)
	}
	return models, rows.Err()
}

// SaveProvider creates or updates a provider config and rebuilds the cached instance.
func (pm *ProviderManager) SaveProvider(def ProviderConfigDef) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	maxRetries := def.MaxRetries
	if maxRetries == 0 {
		maxRetries = 2
	}
	retryWait := def.RetryBaseWaitS
	if retryWait == 0 {
		retryWait = 2
	}
	timeout := def.TimeoutS
	if timeout == 0 {
		timeout = 60
	}

	result, err := pm.db.Exec(`INSERT INTO providers (name, api_base, api_key, is_primary, max_tokens, max_retries, retry_base_wait_s, timeout_s, is_fallback, recover_after, fallback_order, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(name) DO UPDATE SET
			api_base = excluded.api_base,
			api_key = excluded.api_key,
			is_primary = excluded.is_primary,
			max_tokens = excluded.max_tokens,
			max_retries = excluded.max_retries,
			retry_base_wait_s = excluded.retry_base_wait_s,
			timeout_s = excluded.timeout_s,
			is_fallback = excluded.is_fallback,
			recover_after = excluded.recover_after,
			fallback_order = excluded.fallback_order,
			updated_at = datetime('now')`,
		def.Name, def.APIBase, def.APIKey, boolToInt(def.IsPrimary), def.MaxTokens, maxRetries, retryWait, timeout, boolToInt(def.Fallback), def.RecoverAfter, def.FallbackOrder)
	if err != nil {
		return fmt.Errorf("provider save: %w", err)
	}

	// Get the ID
	id, _ := result.LastInsertId()
	if id == 0 {
		// UPDATE path — fetch existing ID
		pm.db.QueryRow(`SELECT id FROM providers WHERE name = ?`, def.Name).Scan(&id)
	}
	if id > 0 {
		def.ID = id
		// Delete old models and re-insert
		pm.db.Exec(`DELETE FROM provider_models WHERE provider_id = ?`, id)
		for _, m := range def.Models {
			pm.db.Exec(`INSERT INTO provider_models (provider_id, name, label, vision, is_default) VALUES (?, ?, ?, ?, ?)`,
				id, m.Name, m.Label, boolToInt(m.Vision), boolToInt(m.Default))
		}
	}

	// Rebuild cache entry
	impl := buildProvider(def)
	pm.cache[def.Name] = &cachedProvider{config: def, impl: impl, createdAt: time.Now()}

	// Update primary/fallback tracking
	pm.rebuildIndicesLocked()

	log.Printf("provider: saved %q (primary=%v, fallback=%v, models=%d)", def.Name, def.IsPrimary, def.Fallback, len(def.Models))
	return nil
}

// DeleteProvider removes a provider config and its cached instance.
func (pm *ProviderManager) DeleteProvider(name string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if _, err := pm.db.Exec(`DELETE FROM providers WHERE name = ?`, name); err != nil {
		return fmt.Errorf("provider delete: %w", err)
	}

	delete(pm.cache, name)
	if pm.primary == name {
		pm.primary = ""
	}
	// Rebuild fallback list
	pm.fallback = pm.fallback[:0]
	for n, cp := range pm.cache {
		if cp.config.Fallback {
			pm.fallback = append(pm.fallback, n)
		}
	}

	log.Printf("provider: deleted %q", name)
	return nil
}

// IsEmpty returns true if no providers are configured.
func (pm *ProviderManager) IsEmpty() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.cache) == 0
}

// ListProviders returns all stored provider configs (without API keys).
func (pm *ProviderManager) ListProviders() []ProviderConfigDef {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	result := make([]ProviderConfigDef, 0, len(pm.cache))
	for _, cp := range pm.cache {
		def := cp.config
		def.APIKey = "" // never expose keys
		result = append(result, def)
	}
	return result
}

// GetProvider returns the cached provider implementation by name.
func (pm *ProviderManager) GetProvider(name string) LLMProvider {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if cp, ok := pm.cache[name]; ok {
		return cp.impl
	}
	return nil
}

// GetPrimaryProvider returns the primary provider, or nil if none configured.
func (pm *ProviderManager) GetPrimaryProvider() LLMProvider {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if pm.primary != "" {
		if cp, ok := pm.cache[pm.primary]; ok {
			return cp.impl
		}
	}
	// Fallback: return first available
	for _, cp := range pm.cache {
		return cp.impl
	}
	return nil
}

// GetDefaultModel returns the global default model.
func (pm *ProviderManager) GetDefaultModel() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if pm.modelDefault != "" {
		return pm.modelDefault
	}
	// Try to get from primary provider
	if pm.primary != "" {
		if cp, ok := pm.cache[pm.primary]; ok {
			for _, m := range cp.config.Models {
				if m.Default {
					return m.Name
				}
			}
			if len(cp.config.Models) > 0 {
				return cp.config.Models[0].Name
			}
		}
	}
	return ""
}

// BuildLLMProvider builds the full provider chain (primary + fallbacks) for the AgentLoop.
// This is used at startup to provide the a.provider field.
// It also returns a function to hot-swap the provider later.
func (pm *ProviderManager) BuildLLMProvider() (LLMProvider, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	if len(pm.cache) == 0 {
		return NewStubProvider(), nil
	}

	// Find primary
	var primaryImpl LLMProvider
	primaryName := pm.primary
	if primaryName == "" {
		// Use first non-fallback
		for name, cp := range pm.cache {
			if !cp.config.Fallback {
				primaryName = name
				break
			}
		}
	}
	if primaryName != "" {
		if cp, ok := pm.cache[primaryName]; ok {
			primaryImpl = cp.impl
		}
	}
	if primaryImpl == nil {
		// Use any
		for _, cp := range pm.cache {
			primaryImpl = cp.impl
			break
		}
	}
	if primaryImpl == nil {
		return NewStubProvider(), nil
	}

	// Build fallback entries
	var entries []FallbackEntry
	for _, name := range pm.fallback {
		cp, ok := pm.cache[name]
		if !ok {
			continue
		}
		recoverAfter := 5 * time.Minute
		if cp.config.RecoverAfter != "" {
			if d, err := time.ParseDuration(cp.config.RecoverAfter); err == nil {
				recoverAfter = d
			}
		}
		// Use the first model defined for this fallback provider
		model := ""
		if len(cp.config.Models) > 0 {
			model = cp.config.Models[0].Name
		}
		entries = append(entries, FallbackEntry{
			Provider:     cp.impl,
			Model:        model,
			Name:         name,
			RecoverAfter: recoverAfter,
		})
	}

	if len(entries) == 0 {
		return primaryImpl, nil
	}
	return NewFallbackProvider(primaryImpl, entries), nil
}

// AvailableModels returns all models across all providers, for the admin UI.
func (pm *ProviderManager) AvailableModels() []ModelDef {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	var models []ModelDef
	for _, cp := range pm.cache {
		models = append(models, cp.config.Models...)
	}
	return models
}

// HasProviders returns true if at least one provider is configured.
func (pm *ProviderManager) HasProviders() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.cache) > 0
}

// GetProviderForModel finds which provider offers the given model.
// Returns the provider name and implementation, or ("", nil) if not found.
func (pm *ProviderManager) GetProviderForModel(model string) (string, LLMProvider) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for name, cp := range pm.cache {
		for _, m := range cp.config.Models {
			if m.Name == model {
				return name, cp.impl
			}
		}
	}
	return "", nil
}

// rebuildIndicesLocked recalculates primary and fallback list from cache.
// Must be called with pm.mu held.
func (pm *ProviderManager) rebuildIndicesLocked() {
	pm.fallback = pm.fallback[:0]
	pm.primary = ""
	pm.modelDefault = ""

	for name, cp := range pm.cache {
		if cp.config.IsPrimary {
			pm.primary = name
			for _, m := range cp.config.Models {
				if m.Default {
					pm.modelDefault = m.Name
				}
			}
		}
	}
	for name, cp := range pm.cache {
		if cp.config.Fallback {
			pm.fallback = append(pm.fallback, name)
		}
	}
}

// buildProvider creates an LLMProvider implementation from a config definition.
func buildProvider(def ProviderConfigDef) LLMProvider {
	maxRetries := def.MaxRetries
	if maxRetries == 0 {
		maxRetries = 2
	}
	retryWait := time.Duration(def.RetryBaseWaitS) * time.Second
	if retryWait == 0 {
		retryWait = 2 * time.Second
	}
	return NewOpenAIProviderWithRetry(
		def.APIKey,
		def.APIBase,
		def.TimeoutS,
		def.MaxTokens,
		maxRetries,
		retryWait,
	)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Close closes the database connection.
func (pm *ProviderManager) Close() error {
	if pm.db != nil {
		return pm.db.Close()
	}
	return nil
}

// SeedFromConfig seeds providers from a legacy config.json structure.
// This is used when migrating from env-var-based configuration to database-backed.
func (pm *ProviderManager) SeedFromConfig(apiKey, apiBase string, fallbacks []struct {
	Name, APIKey, APIBase, Model, RecoverAfter string
	MaxTokens int
}) error {
	// Save primary
	primary := ProviderConfigDef{
		Name:      "primary",
		APIBase:   apiBase,
		APIKey:    apiKey,
		IsPrimary: true,
	}
	if err := pm.SaveProvider(primary); err != nil {
		return err
	}

	for _, fb := range fallbacks {
		def := ProviderConfigDef{
			Name:         fb.Name,
			APIBase:      fb.APIBase,
			APIKey:       fb.APIKey,
			Fallback:     true,
			RecoverAfter: fb.RecoverAfter,
			MaxTokens:    fb.MaxTokens,
		}
		if err := pm.SaveProvider(def); err != nil {
			return err
		}
	}

	return pm.LoadFromDB()
}

// ResolveForTier returns the best provider and model for a given tier.
// Priority: tier's provider config → tier's model override → global defaults.
func (pm *ProviderManager) ResolveForTier(tierModel string) (LLMProvider, string) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	// If tier specifies a model, try to find a provider for it
	if tierModel != "" {
		for _, cp := range pm.cache {
			for _, m := range cp.config.Models {
				if m.Name == tierModel {
					return cp.impl, tierModel
				}
			}
		}
		// Model not found in any provider — use primary with the model name
		// (the API endpoint might accept it even if we don't track it)
		if pm.primary != "" {
			if cp, ok := pm.cache[pm.primary]; ok {
				return cp.impl, tierModel
			}
		}
	}

	// Use primary provider with default model
	if pm.primary != "" {
		if cp, ok := pm.cache[pm.primary]; ok {
			model := pm.modelDefault
			if model == "" && len(cp.config.Models) > 0 {
				model = cp.config.Models[0].Name
			}
			return cp.impl, model
		}
	}

	// No primary — use any available provider
	for _, cp := range pm.cache {
		model := ""
		if len(cp.config.Models) > 0 {
			model = cp.config.Models[0].Name
		}
		return cp.impl, model
	}

	return NewStubProvider(), ""
}

// GetModelsForTier returns the models available to a tier.
// If the tier has no explicit model list, returns all models from the primary provider.
func (pm *ProviderManager) GetModelsForTier(tierModels []string) []ModelDef {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	// If tier specifies models, return those from any provider
	if len(tierModels) > 0 {
		var result []ModelDef
		for _, tmName := range tierModels {
			for _, cp := range pm.cache {
				for _, m := range cp.config.Models {
					if m.Name == tmName {
						result = append(result, m)
					}
				}
			}
		}
		return result
	}

	// Default: return primary provider's models
	if pm.primary != "" {
		if cp, ok := pm.cache[pm.primary]; ok {
			return cp.config.Models
		}
	}

	// No primary — return all models
	var all []ModelDef
	for _, cp := range pm.cache {
		all = append(all, cp.config.Models...)
	}
	return all
}

// MarshalForAPI returns the full config as JSON for the admin API.
// API keys are masked.
func (pm *ProviderManager) MarshalForAPI() ([]byte, error) {
	providers := pm.ListProviders()
	for i := range providers {
		if providers[i].APIKey != "" {
			providers[i].APIKey = "***"
		}
	}
	return json.Marshal(providers)
}
