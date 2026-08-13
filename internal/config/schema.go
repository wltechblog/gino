package config

// Config holds gino configuration.
type Config struct {
	Agents     AgentsConfig               `json:"agents"`
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
	Channels   ChannelsConfig             `json:"channels"`
	Providers  ProvidersConfig            `json:"providers"`
	Brain      *BrainConfig               `json:"brain,omitempty"`
	Signal     SignalConfig               `json:"signal"`
	Tenant     *TenantConfig              `json:"tenant,omitempty"`
	Audit      *AuditConfig               `json:"audit,omitempty"`
}

// BrainConfig configures the optional knowledge brain subsystem.
// If nil or disabled, Gino works exactly as before (flat-file memory only).
type BrainConfig struct {
	Enabled        bool   `json:"enabled"`
	EmbeddingModel string `json:"embeddingModel,omitempty"` // default: "nomic-embed-text"
	EmbeddingDims  int    `json:"embeddingDims,omitempty"`  // default: 768
	OllamaURL      string `json:"ollamaBaseURL,omitempty"`  // default: "http://localhost:11434"
	RemoteAPIBase  string `json:"remoteApiBase,omitempty"`  // fallback remote API base URL
	RemoteAPIKey   string `json:"remoteApiKey,omitempty"`   // fallback remote API key
	RemoteModel    string `json:"remoteModel,omitempty"`    // fallback remote model name
}

// MCPServerConfig describes a single MCP server connection.
// Use Command+Args for stdio transport, or URL+Headers for HTTP transport.
type MCPServerConfig struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Env is additional environment variables to inject into the child process (stdio transport only).
	// Gino also injects GINO_SIGNAL_SOCKET and GINO_MCP_ID automatically.
	Env map[string]string `json:"env,omitempty"`
}

// SignalConfig configures the external trigger listener.
// When enabled, Gino listens on a Unix domain socket for external signals
// that can wake the agent and inject messages into the hub.
type SignalConfig struct {
	// Enabled controls whether the signal listener is active.
	Enabled bool `json:"enabled"`

	// SocketPath is the Unix domain socket path. If empty, defaults to
	// {workspace}/.gino/signals.sock
	SocketPath string `json:"socketPath,omitempty"`

	// DefaultChannel is the fallback channel for signals that don't specify one
	// and when no previous real channel has been recorded yet.
	// Typically "telegram", "discord", etc.
	DefaultChannel string `json:"defaultChannel,omitempty"`

	// DefaultChatID is the fallback chatID for signals that don't specify one
	// and when no previous real chatID has been recorded yet.
	DefaultChatID string `json:"defaultChatID,omitempty"`

	// Actions defines user-defined signal actions that external sources can send.
	// The key is the action name (e.g., "motion_detected"), the value describes
	// what response to inject when that action is received.
	// MCP servers self-declare their own actions at startup.
	Actions map[string]SignalActionConfig `json:"actions,omitempty"`
}

// SignalActionConfig defines a single signal action and its safe response template.
type SignalActionConfig struct {
	// Description is a human-readable description of what this signal means.
	Description string `json:"description"`

	// Response is the message text injected into the agent when this signal fires.
	// This is the ONLY text the agent sees — the raw signal payload is never exposed.
	// Supports Go template variables: {{.Source}}, {{.Timestamp}}
	Response string `json:"response"`

	// Silent controls whether the agent's response is suppressed from the channel.
	// When true, the agent still processes the signal (runs tools, updates state, etc.)
	// but only sends a reply to the channel if it has something genuinely useful to report.
	// Useful for background triggers like check_messages that shouldn't spam the user
	// with "no new messages" acknowledgments.
	Silent bool `json:"silent,omitempty"`
}

func (sc SignalConfig) GetSocketPath(homeDir, workspace string) string {
	if sc.SocketPath != "" {
		return sc.SocketPath
	}
	// Default: {workspace}/.gino/signals.sock
	return workspace + "/.gino/signals.sock"
}

type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
}

type AgentDefaults struct {
	Workspace                   string        `json:"workspace"`
	Model                       string        `json:"model"`
	MaxTokens                   int           `json:"maxTokens"`
	Temperature                 float64       `json:"temperature"`
	MaxToolIterations           int           `json:"maxToolIterations"`
	HeartbeatIntervalS          int           `json:"heartbeatIntervalS"`
	RequestTimeoutS             int           `json:"requestTimeoutS"`
	MaxRetries                  int           `json:"maxRetries,omitempty"`    // retries per provider attempt (default: 2)
	RetryBaseWaitS              int           `json:"retryBaseWaitS,omitempty"` // base wait between retries in seconds (default: 2)
	EnableToolActivityIndicator *bool         `json:"enableToolActivityIndicator,omitempty"`
	EnableToolCallMessages     *bool         `json:"enableToolCallMessages,omitempty"`
	EnableToolErrorMessages    *bool         `json:"enableToolErrorMessages,omitempty"`
	VisionModel                string        `json:"visionModel,omitempty"`
	AllowedDirs                 []string      `json:"allowedDirs"`
	DisableTools                []string      `json:"disableTools"`
	Sandbox                     SandboxConfig `json:"sandbox"`
	MaxTurnMessages             int           `json:"maxTurnMessages,omitempty"`
	MaxToolResultChars          int           `json:"maxToolResultChars,omitempty"`

	// Web controls the built-in web fetch tool.
	Web WebConfig `json:"web"`

	// Search controls the web search provider.
	Search SearchConfig `json:"search"`

	// Compaction controls LLM-based context summarization.
	// When enabled, old messages are summarized by the LLM instead of dropped.
	Compaction *CompactionConfig `json:"compaction,omitempty"`
}

// WebConfig configures the web fetch tool.
type WebConfig struct {
	// TimeoutS is the maximum time in seconds for an HTTP request.
	// Default: 30
	TimeoutS int `json:"timeoutS,omitempty"`

	// MaxResponseBytes limits the size of the response body read.
	// Default: 1048576 (1 MB)
	MaxResponseBytes int `json:"maxResponseBytes,omitempty"`

	// UserAgent is the User-Agent header sent with requests.
	// Default: "GinoAI https://github.com/wltechblog/gino"
	UserAgent string `json:"userAgent,omitempty"`
}

// SearchConfig configures the web search tool.
// By default Gino uses DuckDuckGo (no API key needed).
// Set provider to "brave" and provide a Brave API key for full web search.
type SearchConfig struct {
	// Provider selects the search backend: "duckduckgo" (default) or "brave".
	Provider string `json:"provider,omitempty"`

	// BraveAPIKey is the API key for Brave Search.
	// Get one free at https://brave.com/search/api/
	// Can also be set via GINO_BRAVE_SEARCH_API_KEY env var.
	BraveAPIKey string `json:"braveApiKey,omitempty"`
}

// CompactionConfig configures LLM-based context compaction.
// When messages exceed the trigger threshold, older messages are summarized
// by a separate LLM call into a structured checkpoint rather than silently dropped.
type CompactionConfig struct {
	// Enabled turns on LLM-based compaction. When false (or nil), the legacy
	// trimTurnMessages slicer is used instead.
	Enabled bool `json:"enabled"`

	// MaxContextTokens is the estimated context window size in tokens.
	// When total message tokens approach this limit, compaction fires.
	// If left as 0 (default), Gino auto-detects the context window by
	// querying the provider's /models/{model} endpoint.
	// Fallback default if detection fails: 128000.
	MaxContextTokens int `json:"maxContextTokens,omitempty"`

	// ReserveTokens is the token budget reserved for the summarization prompt
	// and the LLM's response. Compaction fires when usage > MaxContextTokens - ReserveTokens.
	// If left as 0, defaults to 15% of the context window (min 8192).
	ReserveTokens int `json:"reserveTokens,omitempty"`

	// KeepRecentTokens is the number of tokens of recent messages to keep intact
	// (not summarized). Older messages beyond this window are summarized.
	// If left as 0, defaults to 15% of the context window (min 10000).
	KeepRecentTokens int `json:"keepRecentTokens,omitempty"`

	// MaxSummaryTokens caps the length of the generated summary to prevent
	// it from growing unboundedly across iterative compactions.
	// If left as 0, defaults to 5% of the context window (min 2000).
	MaxSummaryTokens int `json:"maxSummaryTokens,omitempty"`
}

// SandboxConfig controls the exec tool's security level.
//   - Mode "strict":     current behavior — array-only commands, no absolute paths, full blacklist (default)
//   - Mode "permissive": block truly dangerous commands (dd, mkfs, shutdown), allow absolute paths, array-only
//   - Mode "yolo":       no restrictions — string commands allowed, no path validation, no blacklist
type SandboxConfig struct {
	// Mode is "strict", "permissive", or "yolo". Defaults to "strict" if empty.
	Mode string `json:"mode"`

	// AllowedCommands, if non-empty, is a whitelist of allowed program names.
	// Only used in "strict" and "permissive" modes. In "yolo" mode, this is ignored.
	// If empty, all non-blocked programs are allowed.
	AllowedCommands []string `json:"allowedCommands,omitempty"`

	// BlockedCommands is an additional list of blocked program names, on top of the defaults.
	// In "yolo" mode, this is ignored.
	BlockedCommands []string `json:"blockedCommands,omitempty"`

	// AllowAbsolutePaths overrides the default behavior for absolute paths in arguments.
	// In "strict" mode, defaults to false. In "permissive" mode, defaults to true.
	// In "yolo" mode, all paths are allowed regardless.
	AllowAbsolutePaths *bool `json:"allowAbsolutePaths,omitempty"`

	// AllowStringCommands enables shell string commands (e.g., {"cmd": "ls -la"}).
	// Only effective in "yolo" mode (forced false otherwise).
	AllowStringCommands bool `json:"allowStringCommands,omitempty"`
}

func (s SandboxConfig) GetMode() string {
	if s.Mode == "" {
		return "strict"
	}
	return s.Mode
}

func (s SandboxConfig) IsYolo() bool {
	return s.GetMode() == "yolo"
}

func (s SandboxConfig) IsPermissive() bool {
	return s.GetMode() == "permissive"
}

func (s SandboxConfig) AllowsAbsolutePaths() bool {
	if s.IsYolo() {
		return true
	}
	if s.AllowAbsolutePaths != nil {
		return *s.AllowAbsolutePaths
	}
	// default: permissive allows, strict doesn't
	return s.IsPermissive()
}

func (s SandboxConfig) AllowsStringCommands() bool {
	return s.IsYolo() && s.AllowStringCommands
}

type ChannelsConfig struct {
	Telegram TelegramConfig `json:"telegram"`
	Discord  DiscordConfig  `json:"discord"`
	API      APIConfig      `json:"api"`
}

// APIConfig configures the HTTP/SSE API gateway.
// This enables mobile, desktop, and web apps to communicate with Gino
// without relying on Telegram or Discord.
type APIConfig struct {
	// Enabled controls whether the API server starts.
	Enabled bool `json:"enabled"`

	// Addr is the listen address. Default: ":8443"
	// Behind a reverse proxy (Caddy, nginx), use ":8080" or similar.
	Addr string `json:"addr,omitempty"`

	// Tokens maps API bearer tokens to user identifiers.
	// e.g., {"abc123secret": "josh", "tok2": "alice"}
	// Each key is a secret token; the value is the user ID for session scoping.
	Tokens map[string]string `json:"tokens,omitempty"`

	// AllowAnon skips token validation (development only).
	AllowAnon bool `json:"allowAnon,omitempty"`

	// RequestTimeoutS is the max seconds for sync chat requests. Default: 120
	RequestTimeoutS int `json:"requestTimeoutS,omitempty"`
}

type DiscordConfig struct {
	Enabled   bool     `json:"enabled"`
	Token     string   `json:"token"`
	AllowFrom []string `json:"allowFrom"`
	AllowDMs  bool     `json:"allowDMs"`

	// MonitorChannels are guild channel IDs where the bot engages on every
	// message without requiring an @mention. The bot replies directly in the
	// channel (no thread creation).
	MonitorChannels []string `json:"monitorChannels,omitempty"`

	// SendAttachments controls whether the bot sends file attachments (e.g.,
	// generated files, screenshots) back to the Discord channel. When false,
	// the bot only sends text messages.
	SendAttachments bool `json:"sendAttachments,omitempty"`

	// AdminRoleID is a Discord role ID whose members can send messages in
	// any thread created by the bot, not just the thread owner. This allows
	// server admins/moderators to correct the bot or assist users.
	AdminRoleID string `json:"adminRoleID,omitempty"`

	// Rate limiting (0 = unlimited)
	RateLimitPerMinute int `json:"rateLimitPerMinute,omitempty"` // max messages per user per minute
	RateLimitPerHour   int `json:"rateLimitPerHour,omitempty"`   // max messages per user per hour
	RateLimitTotalHour int `json:"rateLimitTotalHour,omitempty"` // max total messages per hour (across all users)
}

type TelegramConfig struct {
	Enabled   bool     `json:"enabled"`
	Token     string   `json:"token"`
	AllowFrom []string `json:"allowFrom"`

	// MonitorGroups are group/supergroup chat IDs where the bot responds to
	// @mentions from any user. Non-owner users are treated as unprivileged.
	// Each user gets their own session (telegram:<groupID>:<userID>).
	MonitorGroups []string `json:"monitorGroups,omitempty"`
}

type ProvidersConfig struct {
	OpenAI    *ProviderConfig  `json:"openai,omitempty"`
	Fallbacks []FallbackConfig `json:"fallbacks,omitempty"`
}

type ProviderConfig struct {
	APIKey          string `json:"apiKey"`
	APIBase         string `json:"apiBase"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// FallbackConfig defines a fallback LLM provider to use when the primary fails.
// Fallbacks are tried in order. Each has its own RecoverAfter timer that controls
// when to retry the primary provider.
type FallbackConfig struct {
	// Name is a human-readable label for logging (e.g., "cheap-fast", "backup").
	Name string `json:"name"`

	// APIKey for this fallback provider.
	APIKey string `json:"apiKey"`

	// APIBase is the OpenAI-compatible API base URL.
	APIBase string `json:"apiBase"`

	// Model is the model identifier to use for this fallback.
	Model string `json:"model"`

	// MaxTokens overrides the default max tokens for this fallback (0 = use default).
	MaxTokens int `json:"maxTokens,omitempty"`

	// ReasoningEffort controls reasoning for OpenAI-compatible providers.
	// For Ollama, "none" disables thinking.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`

	// RecoverAfter controls how long to stay on this fallback before retrying
	// the primary provider. Defaults to 5m. Set to "0s" to retry primary on
	// every request (aggressive recovery).
	RecoverAfter string `json:"recoverAfter,omitempty"`
}

// TenantConfig configures multi-tenant mode.
// When enabled, Gino resolves each incoming message to a per-user context
// with isolated workspace, tools, and resource limits.
// When nil or disabled, Gino operates in single-user mode (backward compatible).
type TenantConfig struct {
	// Enabled turns on multi-tenant mode.
	Enabled bool `json:"enabled"`

	// WorkspaceRoot is the base directory for per-user workspaces.
	// User workspaces are created at {workspaceRoot}/{userID}.
	// Default: {agents.defaults.workspace}/users
	WorkspaceRoot string `json:"workspaceRoot,omitempty"`

	// DefaultTier is the tier assigned to users without an explicit tier.
	// Default: "default"
	DefaultTier string `json:"defaultTier,omitempty"`

	// EvictionTimeoutMinutes controls how long after last activity a user
	// context is evicted from memory. Default: 30.
	EvictionTimeoutMinutes int `json:"evictionTimeoutMinutes,omitempty"`

	// Tiers defines the available tier levels.
	Tiers []TierConfig `json:"tiers,omitempty"`

	// Users defines known users. For dynamic provisioning (e.g., API tokens
	// that map to users), use a UserFactory in the UserManager instead.
	Users []TenantUserConfig `json:"users,omitempty"`
}

// TierConfig defines a single tier level from configuration.
type TierConfig struct {
	Name                 string         `json:"name"`
	MaxToolIterations    int            `json:"maxToolIterations,omitempty"`
	MaxContextTokens     int            `json:"maxContextTokens,omitempty"`
	AllowedTools         []string       `json:"allowedTools,omitempty"`
	DisableTools         []string       `json:"disableTools,omitempty"`
	RateLimitPerHour     int            `json:"rateLimitPerHour,omitempty"`
	RateLimitPerDay      int            `json:"rateLimitPerDay,omitempty"`
	MaxConcurrentTurns   int            `json:"maxConcurrentTurns,omitempty"`
	MaxWorkspaceBytes    int64          `json:"maxWorkspaceBytes,omitempty"`
	MaxFileUploadBytes   int64          `json:"maxFileUploadBytes,omitempty"`
	Model                string         `json:"model,omitempty"`
	Models               []ModelOption  `json:"models,omitempty"`   // available models for this tier
	Sandbox              string         `json:"sandbox,omitempty"`
	AllowedMCP           []string       `json:"allowedMcp,omitempty"`
	Providers            *TierProviders `json:"providers,omitempty"` // per-tier LLM providers
}

// ModelOption defines a selectable model for a tier.
type ModelOption struct {
	Name   string `json:"name"`           // model identifier (e.g. "glm-5.2")
	Label  string `json:"label,omitempty"` // display name (e.g. "GLM-5.2 Turbo")
	Vision bool   `json:"vision,omitempty"`
}

// TierProviders allows per-tier LLM provider configuration.
// If set, users in this tier use these providers instead of the global ones.
type TierProviders struct {
	Primary   *TierProviderEntry  `json:"primary,omitempty"`
	Fallbacks []TierProviderEntry `json:"fallbacks,omitempty"`
}

// TierProviderEntry is a single provider for a tier.
type TierProviderEntry struct {
	APIBase   string `json:"apiBase"`
	APIKey    string `json:"apiKey"`
	Model     string `json:"model"`
	MaxTokens int    `json:"maxTokens,omitempty"`
}

// TenantUserConfig defines a single user in multi-tenant configuration.
type TenantUserConfig struct {
	ID                string            `json:"id"`
	DisplayName       string            `json:"displayName,omitempty"`
	Tier              string            `json:"tier"`
	Token             string            `json:"token,omitempty"`
	Channels          map[string]string `json:"channels,omitempty"`
	WorkspaceOverride string            `json:"workspaceOverride,omitempty"`
	Admin             bool              `json:"admin,omitempty"`
}

// AuditConfig controls the audit trail system.
type AuditConfig struct {
	// Enabled turns on audit logging of all messages and token usage.
	Enabled bool `json:"enabled"`

	// DBPath is the SQLite database path. Default: {homeDir}/audit.db
	DBPath string `json:"dbPath,omitempty"`

	// MessageRetentionDays controls how long message logs are kept.
	// Default: 7. Set to 0 to disable message logging.
	MessageRetentionDays int `json:"messageRetentionDays,omitempty"`

	// MaxContentLen truncates stored message content. Default: 4096.
	MaxContentLen int `json:"maxContentLen,omitempty"`

	// UsageRetentionDays controls how long usage/cost records are kept.
	// Default: 365. Set to 0 for indefinite.
	UsageRetentionDays int `json:"usageRetentionDays,omitempty"`
}
