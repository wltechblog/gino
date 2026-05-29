package config

// Config holds picobot configuration (minimal for v0).
type Config struct {
	Agents     AgentsConfig               `json:"agents"`
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
	Channels   ChannelsConfig             `json:"channels"`
	Providers  ProvidersConfig            `json:"providers"`
	Brain      *BrainConfig               `json:"brain,omitempty"`
	Signal     SignalConfig               `json:"signal"`
}

// BrainConfig configures the optional knowledge brain subsystem.
// If nil or disabled, Picobot works exactly as before (flat-file memory only).
type BrainConfig struct {
	Enabled        bool   `json:"enabled"`
	EmbeddingModel string `json:"embeddingModel,omitempty"` // default: "nomic-embed-text"
	EmbeddingDims  int    `json:"embeddingDims,omitempty"`  // default: 768
	OllamaURL      string `json:"ollamaUrl,omitempty"`      // default: "http://localhost:11434"
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
	// Picobot also injects PICOBOT_SIGNAL_SOCKET and PICOBOT_MCP_ID automatically.
	Env map[string]string `json:"env,omitempty"`
}

// SignalConfig configures the external trigger listener.
// When enabled, Picobot listens on a Unix domain socket for external signals
// that can wake the agent and inject messages into the hub.
type SignalConfig struct {
	// Enabled controls whether the signal listener is active.
	Enabled bool `json:"enabled"`

	// SocketPath is the Unix domain socket path. If empty, defaults to
	// {workspace}/.picobot/signals.sock
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
}

func (sc SignalConfig) GetSocketPath(homeDir, workspace string) string {
	if sc.SocketPath != "" {
		return sc.SocketPath
	}
	// Default: {workspace}/.picobot/signals.sock
	return workspace + "/.picobot/signals.sock"
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
	EnableToolActivityIndicator *bool         `json:"enableToolActivityIndicator,omitempty"`
	EnableToolCallMessages     *bool         `json:"enableToolCallMessages,omitempty"`
	AllowedDirs                 []string      `json:"allowedDirs"`
	DisableTools                []string      `json:"disableTools"`
	Sandbox                     SandboxConfig `json:"sandbox"`
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
	Slack    SlackConfig    `json:"slack"`
	WhatsApp WhatsAppConfig `json:"whatsapp"`
}

type DiscordConfig struct {
	Enabled   bool     `json:"enabled"`
	Token     string   `json:"token"`
	AllowFrom []string `json:"allowFrom"`
}

type TelegramConfig struct {
	Enabled   bool     `json:"enabled"`
	Token     string   `json:"token"`
	AllowFrom []string `json:"allowFrom"`
}

type SlackConfig struct {
	Enabled       bool     `json:"enabled"`
	AppToken      string   `json:"appToken"`
	BotToken      string   `json:"botToken"`
	AllowUsers    []string `json:"allowUsers"`
	AllowChannels []string `json:"allowChannels"`
}

type WhatsAppConfig struct {
	Enabled   bool     `json:"enabled"`
	DBPath    string   `json:"dbPath"`
	AllowFrom []string `json:"allowFrom"`
}

type ProvidersConfig struct {
	OpenAI *ProviderConfig `json:"openai,omitempty"`
}

type ProviderConfig struct {
	APIKey  string `json:"apiKey"`
	APIBase string `json:"apiBase"`
}
