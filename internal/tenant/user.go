// Package tenant provides multi-user isolation for Gino.
//
// Each user gets a UserContext containing their own workspace, session
// manager, memory store, brain instance, and filtered tool registry.
// The UserManager creates, caches, and evicts user contexts.
//
// Tier definitions control per-user resource limits, allowed tools,
// and rate limits. This enables deployment models ranging from a
// single trusted operator (YOLO mode) to thousands of untrusted
// assistant users with restricted tool access.
package tenant

import (
	"sync"
	"time"
)

// Tier defines resource limits and permissions for a class of user.
// Tiers are defined in configuration and referenced by user records.
type Tier struct {
	// Name is the tier identifier (e.g. "free", "pro", "admin").
	Name string `json:"name"`

	// MaxToolIterations caps how many tool calls a single turn can make.
	// 0 means use the global default.
	MaxToolIterations int `json:"maxToolIterations,omitempty"`

	// MaxContextTokens limits the context window size for this tier.
	// 0 means use the global default.
	MaxContextTokens int `json:"maxContextTokens,omitempty"`

	// AllowedTools is the whitelist of tool names this tier may use.
	// If empty, all registered tools are allowed (subject to DisableTools).
	// The special value "*" allows all tools.
	AllowedTools []string `json:"allowedTools,omitempty"`

	// DisableTools is a blacklist applied after the whitelist.
	// Useful when AllowedTools is ["*"] but certain tools should still be blocked.
	DisableTools []string `json:"disableTools,omitempty"`

	// RateLimitPerHour caps the number of turns a user can initiate per hour.
	// 0 means unlimited.
	RateLimitPerHour int `json:"rateLimitPerHour,omitempty"`

	// RateLimitPerDay caps turns per day. 0 means unlimited.
	RateLimitPerDay int `json:"rateLimitPerDay,omitempty"`

	// MaxConcurrentTurns limits how many turns can run simultaneously for one user.
	// Almost always 1 (turns should be serialized per user). 0 means 1.
	MaxConcurrentTurns int `json:"maxConcurrentTurns,omitempty"`

	// MaxWorkspaceBytes limits the total disk usage of a user's workspace.
	// 0 means unlimited.
	MaxWorkspaceBytes int64 `json:"maxWorkspaceBytes,omitempty"`

	// MaxFileUploadBytes limits the size of a single file upload.
	MaxFileUploadBytes int64 `json:"maxFileUploadBytes,omitempty"`

	// Model overrides the default LLM model for this tier.
	// Empty means use the global default.
	Model string `json:"model,omitempty"`

	// Sandbox controls the exec sandbox mode for this tier.
	// For assistant tiers, should be "strict" or fully disabled.
	// For YOLO tiers, can be "yolo".
	Sandbox string `json:"sandbox,omitempty"`

	// AllowedMCP is a whitelist of MCP server names this tier may access.
	// If empty, all configured MCP servers are available.
	AllowedMCP []string `json:"allowedMcp,omitempty"`
}

// IsToolAllowed checks whether a tool name is permitted for this tier.
func (t *Tier) IsToolAllowed(name string, allTools []string) bool {
	// Check blacklist first
	for _, blocked := range t.DisableTools {
		if blocked == name {
			return false
		}
	}

	// No whitelist = all allowed (minus blacklist)
	if len(t.AllowedTools) == 0 {
		return true
	}

	for _, allowed := range t.AllowedTools {
		if allowed == "*" || allowed == name {
			return true
		}
	}
	return false
}

// AllowedToolNames returns the subset of allTools that this tier permits.
func (t *Tier) AllowedToolNames(allTools []string) []string {
	result := make([]string, 0, len(allTools))
	for _, name := range allTools {
		if t.IsToolAllowed(name, allTools) {
			result = append(result, name)
		}
	}
	return result
}

// UserConfig is the per-user configuration loaded from config or a user database.
type UserConfig struct {
	// ID is the unique user identifier (e.g. UUID, Telegram user ID, API token ID).
	ID string `json:"id"`

	// DisplayName is the human-readable name for logs and UI.
	DisplayName string `json:"displayName,omitempty"`

	// Tier is the tier name this user belongs to.
	Tier string `json:"tier"`

	// Token is the authentication token (API gateway only).
	// For Telegram/Discord users, this is unused.
	Token string `json:"token,omitempty"`

	// Channels maps channel types to the user's identity on that channel.
	// e.g. {"telegram": "8113382039", "discord": "123456789012345678"}
	Channels map[string]string `json:"channels,omitempty"`

	// WorkspaceOverride sets a custom workspace path.
	// If empty, defaults to {workspaceRoot}/{userID}.
	WorkspaceOverride string `json:"workspaceOverride,omitempty"`

	// Admin grants elevated privileges: access to /resetall, /reset across
	// all users, and visibility into all sessions. In single-tenant mode
	// (no UserManager), all users are effectively admin.
	Admin bool `json:"admin,omitempty"`

	// CreatedAt tracks when this user was first provisioned.
	CreatedAt time.Time `json:"createdAt,omitempty"`

	// LastSeen tracks the most recent activity.
	LastSeen time.Time `json:"lastSeen,omitempty"`

	// Permanent marks the user as non-evictable. Config-seeded users
	// and admin users should always be permanent so they survive the
	// idle eviction sweep.
	Permanent bool `json:"permanent,omitempty"`
}

// UserContext holds all per-user state needed to process a turn.
// Each field is isolated per user — no shared mutable state between users.
type UserContext struct {
	mu sync.Mutex

	// Config is the user's configuration record.
	Config UserConfig

	// Tier is the resolved tier definition.
	Tier *Tier

	// WorkspacePath is the absolute path to this user's workspace directory.
	WorkspacePath string

	// SessionKeyPrefix produces per-user session keys.
	// Convention: "channel:chatID" but scoped to the user.
	SessionKeyPrefix string

	// rateLimit tracks turn counts for rate limiting.
	rateLimit *rateLimitState

	// activeTurns tracks concurrent turns (normally 1).
	activeTurns int

	// lastActivity is updated on every interaction for eviction decisions.
	lastActivity time.Time
}

// NewUserContext creates a new per-user context.
func NewUserContext(cfg UserConfig, tier *Tier, workspacePath string) *UserContext {
	return &UserContext{
		Config:         cfg,
		Tier:           tier,
		WorkspacePath:  workspacePath,
		SessionKeyPrefix: cfg.ID,
		rateLimit:      newRateLimitState(),
		lastActivity:   time.Now(),
	}
}

// CanStartTurn checks rate limits and concurrency limits.
// Returns true if a new turn can begin, false otherwise (with reason).
func (uc *UserContext) CanStartTurn() (bool, string) {
	uc.mu.Lock()
	defer uc.mu.Unlock()

	maxConcurrent := uc.Tier.MaxConcurrentTurns
	if maxConcurrent == 0 {
		maxConcurrent = 1
	}
	if uc.activeTurns >= maxConcurrent {
		return false, "concurrent turn limit reached"
	}

	if !uc.rateLimit.canStart(uc.Tier.RateLimitPerHour, uc.Tier.RateLimitPerDay) {
		return false, "rate limit exceeded"
	}

	return true, ""
}

// BeginTurn marks a turn as started. Must be called after CanStartTurn returns true.
func (uc *UserContext) BeginTurn() {
	uc.mu.Lock()
	defer uc.mu.Unlock()
	uc.activeTurns++
	uc.rateLimit.record()
	uc.lastActivity = time.Now()
}

// EndTurn marks a turn as completed.
func (uc *UserContext) EndTurn() {
	uc.mu.Lock()
	defer uc.mu.Unlock()
	if uc.activeTurns > 0 {
		uc.activeTurns--
	}
	uc.lastActivity = time.Now()
}

// IsAdmin returns true if this user has admin privileges.
func (uc *UserContext) IsAdmin() bool {
	return uc.Config.Admin
}

// LastActivity returns the most recent activity time.
func (uc *UserContext) LastActivity() time.Time {
	uc.mu.Lock()
	defer uc.mu.Unlock()
	return uc.lastActivity
}

// Touch updates the last activity timestamp.
func (uc *UserContext) Touch() {
	uc.mu.Lock()
	defer uc.mu.Unlock()
	uc.lastActivity = time.Now()
}

// rateLimitState tracks per-user turn counts with sliding windows.
type rateLimitState struct {
	hourBuckets []time.Time // timestamps of turns in the last hour
	dayBuckets  []time.Time // timestamps of turns in the last day
}

func newRateLimitState() *rateLimitState {
	return &rateLimitState{}
}

func (rl *rateLimitState) canStart(perHour, perDay int) bool {
	now := time.Now()

	// Prune old entries
	rl.prune(now)

	if perHour > 0 && len(rl.hourBuckets) >= perHour {
		return false
	}
	if perDay > 0 && len(rl.dayBuckets) >= perDay {
		return false
	}
	return true
}

func (rl *rateLimitState) record() {
	now := time.Now()
	rl.hourBuckets = append(rl.hourBuckets, now)
	rl.dayBuckets = append(rl.dayBuckets, now)
}

func (rl *rateLimitState) prune(now time.Time) {
	hourAgo := now.Add(-time.Hour)
	dayAgo := now.Add(-24 * time.Hour)

	// Keep only timestamps within the window
	hourFiltered := rl.hourBuckets[:0]
	for _, t := range rl.hourBuckets {
		if t.After(hourAgo) {
			hourFiltered = append(hourFiltered, t)
		}
	}
	rl.hourBuckets = hourFiltered

	dayFiltered := rl.dayBuckets[:0]
	for _, t := range rl.dayBuckets {
		if t.After(dayAgo) {
			dayFiltered = append(dayFiltered, t)
		}
	}
	rl.dayBuckets = dayFiltered
}
