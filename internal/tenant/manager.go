package tenant

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wltechblog/gino/internal/config"
)

// DefaultEvictionIdleTimeout is how long a user context stays cached after last activity.
const DefaultEvictionIdleTimeout = 30 * time.Minute

// DefaultEvictionInterval is how often the eviction sweep runs.
const DefaultEvictionInterval = 5 * time.Minute

// UserManager creates, caches, and evicates per-user contexts.
// It is the central registry that resolves an authenticated identity
// to a fully provisioned UserContext with isolated workspace, tools,
// and resource limits.
//
// For single-user deployments (YOLO mode, Telegram bot, etc.), the
// UserManager is configured with a single user that maps to the
// global workspace. The overhead is negligible.
type UserManager struct {
	mu sync.RWMutex

	// users maps user ID → cached UserContext
	users map[string]*UserContext

	// tokens maps auth token → user ID for O(1) token lookup
	tokens map[string]string

	// tiers maps tier name → tier definition
	tiers map[string]*Tier

	// workspaceRoot is the base directory for per-user workspaces.
	// User workspaces are created at {workspaceRoot}/{userID}.
	workspaceRoot string

	// evictionTimeout is how long after last activity a context is evicted.
	evictionTimeout time.Duration

	// evictionTicker controls how often the eviction sweep runs.
	evictionStop chan struct{}

	// UserFactory is an optional hook for custom user provisioning.
	// If set, it is called when a user is first encountered (not in cache).
	// Return an error to deny access.
	UserFactory func(userID string) (*UserConfig, error)

	// store is an optional persistence backend. When set, GetByToken
	// and Get fall back to the store to reload evicted non-permanent users.
	store *Store
}

// NewUserManager creates a new UserManager.
func NewUserManager(workspaceRoot string) *UserManager {
	return &UserManager{
		users:          make(map[string]*UserContext),
		tokens:         make(map[string]string),
		tiers:          make(map[string]*Tier),
		workspaceRoot:  workspaceRoot,
		evictionTimeout: DefaultEvictionIdleTimeout,
	}
}

// RegisterTier adds a tier definition.
func (m *UserManager) RegisterTier(t *Tier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tiers[t.Name] = t
}

// GetTier returns a tier by name, or nil if not found.
func (m *UserManager) GetTier(name string) *Tier {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tiers[name]
}

// RegisterUser adds or updates a user configuration.
// This creates the user's workspace directory if it doesn't exist.
func (m *UserManager) RegisterUser(cfg UserConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tier := m.tiers[cfg.Tier]
	if tier == nil {
		return fmt.Errorf("tenant: unknown tier %q for user %q", cfg.Tier, cfg.ID)
	}

	// Determine workspace path
	wsPath := cfg.WorkspaceOverride
	if wsPath == "" {
		wsPath = filepath.Join(m.workspaceRoot, cfg.ID)
	}

	// Create workspace directory
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		return fmt.Errorf("tenant: create workspace for %s: %w", cfg.ID, err)
	}

	// Create memory subdirectory
	memDir := filepath.Join(wsPath, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		return fmt.Errorf("tenant: create memory dir for %s: %w", cfg.ID, err)
	}

	// Create sessions subdirectory
	sessDir := filepath.Join(wsPath, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		return fmt.Errorf("tenant: create sessions dir for %s: %w", cfg.ID, err)
	}

	ctx := NewUserContext(cfg, tier, wsPath)
	m.users[cfg.ID] = ctx

	// Index token for fast lookup
	if cfg.Token != "" {
		m.tokens[cfg.Token] = cfg.ID
	}

	log.Printf("tenant: registered user %q (tier=%s, workspace=%s)", cfg.ID, cfg.Tier, wsPath)
	return nil
}

// Get retrieves a user context by ID. Returns nil if not found.
func (m *UserManager) Get(userID string) *UserContext {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ctx := m.users[userID]
	if ctx != nil {
		ctx.Touch()
	}
	return ctx
}

// GetByToken retrieves a user context by auth token.
// Returns (nil, "") if the token is not registered.
// If the user was evicted from cache but exists in the store, it is reloaded.
func (m *UserManager) GetByToken(token string) (*UserContext, string) {
	// Fast path: check cache
	m.mu.RLock()
	userID, ok := m.tokens[token]
	m.mu.RUnlock()
	if ok {
		m.mu.Lock()
		ctx := m.users[userID]
		m.mu.Unlock()
		if ctx != nil {
			ctx.Touch()
			return ctx, userID
		}
	}

	// Slow path: reload from store if available
	if m.store != nil {
		users, err := m.store.LoadUsers()
		if err == nil {
			for _, u := range users {
				if u.Token == token {
					// Re-register to rebuild cache + workspace
					if err := m.RegisterUser(u); err == nil {
						return m.GetByToken(token) // recursive but now cached
					}
					break
				}
			}
		}
	}

	return nil, ""
}

// GetByChannel retrieves a user context by their identity on a channel.
// e.g. channel="telegram", identity="8113382039"
// Returns (nil, "") if no user matches.
func (m *UserManager) GetByChannel(channel, identity string) (*UserContext, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, ctx := range m.users {
		if ctx.Config.Channels != nil && ctx.Config.Channels[channel] == identity {
			ctx.Touch()
			return ctx, ctx.Config.ID
		}
	}
	return nil, ""
}

// GetOrCreate retrieves a user context, provisioning it via UserFactory if needed.
func (m *UserManager) GetOrCreate(userID string) (*UserContext, error) {
	// Fast path: already cached
	if ctx := m.Get(userID); ctx != nil {
		return ctx, nil
	}

	// Slow path: provision via factory
	if m.UserFactory == nil {
		return nil, fmt.Errorf("tenant: user %q not registered and no factory configured", userID)
	}

	cfg, err := m.UserFactory(userID)
	if err != nil {
		return nil, err
	}

	if err := m.RegisterUser(*cfg); err != nil {
		return nil, err
	}

	return m.Get(userID), nil
}

// ActiveUsers returns the number of cached user contexts.
func (m *UserManager) ActiveUsers() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.users)
}

// StartEviction begins the background goroutine that evicts idle user contexts.
// Call StopEviction to clean up.
func (m *UserManager) StartEviction() {
	m.evictionStop = make(chan struct{})
	go m.evictionLoop()
}

// StopEviction stops the eviction goroutine.
func (m *UserManager) StopEviction() {
	if m.evictionStop != nil {
		close(m.evictionStop)
		m.evictionStop = nil
	}
}

func (m *UserManager) evictionLoop() {
	ticker := time.NewTicker(DefaultEvictionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.evictIdle()
		case <-m.evictionStop:
			return
		}
	}
}

func (m *UserManager) evictIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	evicted := 0

	for id, ctx := range m.users {
		if now.Sub(ctx.LastActivity()) > m.evictionTimeout {
			// Don't evict permanent users (config-seeded, admin)
			if ctx.Config.Permanent {
				continue
			}
			// Don't evict if there's an active turn
			if ctx.activeTurns > 0 {
				continue
			}
			delete(m.users, id)
			// Also clean up token mapping
			if ctx.Config.Token != "" {
				delete(m.tokens, ctx.Config.Token)
			}
			evicted++
			log.Printf("tenant: evicted idle user %q (idle > %s)", id, m.evictionTimeout)
		}
	}

	if evicted > 0 {
		log.Printf("tenant: evicted %d idle users, %d active remaining", evicted, len(m.users))
	}
}

// SetStore wires a persistence backend for user reload after eviction.
func (m *UserManager) SetStore(s *Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = s
}

// SetEvictionTimeout overrides the default idle eviction timeout.
func (m *UserManager) SetEvictionTimeout(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evictionTimeout = d
}

// RemoveUser removes a user from the active cache.
func (m *UserManager) RemoveUser(userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx, ok := m.users[userID]; ok {
		if ctx.Config.Token != "" {
			delete(m.tokens, ctx.Config.Token)
		}
		delete(m.users, userID)
		log.Printf("tenant: removed user %q", userID)
	}
}

// AllTiers returns all registered tier definitions.
func (m *UserManager) AllTiers() []config.TierConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]config.TierConfig, 0, len(m.tiers))
	for _, t := range m.tiers {
		result = append(result, tierToConfig(t))
	}
	return result
}

func tierToConfig(t *Tier) config.TierConfig {
	return config.TierConfig{
		Name:              t.Name,
		MaxToolIterations: t.MaxToolIterations,
		MaxContextTokens:  t.MaxContextTokens,
		AllowedTools:      t.AllowedTools,
		DisableTools:      t.DisableTools,
		RateLimitPerHour:  t.RateLimitPerHour,
		RateLimitPerDay:   t.RateLimitPerDay,
		MaxConcurrentTurns: t.MaxConcurrentTurns,
		MaxWorkspaceBytes: t.MaxWorkspaceBytes,
		MaxFileUploadBytes: t.MaxFileUploadBytes,
		Model:             t.Model,
		Sandbox:           t.Sandbox,
		AllowedMCP:        t.AllowedMCP,
	}
}

// AllUsers returns a snapshot of all registered user IDs and their tiers.
func (m *UserManager) AllUsers() []UserConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]UserConfig, 0, len(m.users))
	for _, ctx := range m.users {
		result = append(result, ctx.Config)
	}
	return result
}
