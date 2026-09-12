package providers

import (
	"context"
	"encoding/json"
	"log"
	"strings"
)

// Message represents a chat message to/from the LLM.
type Message struct {
	Role       string     `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content    string     `json:"content"`
	Images     []string   `json:"images,omitempty"`       // base64-encoded image data URLs (data:image/...;base64,...) for vision models
	ToolCallID string     `json:"tool_call_id,omitempty"` // set when Role == "tool"
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // set on assistant msgs with tool calls
}

// ToolDefinition is a lightweight description of a tool available to the model.
type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// ToolCall represents a request from the LLM to invoke a tool.
type ToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// LLMResponse is a normalized response from a provider.
type LLMResponse struct {
	Content       string     `json:"content"`
	HasToolCalls  bool       `json:"hasToolCalls"`
	ToolCalls     []ToolCall `json:"toolCalls,omitempty"`
	HadParseError bool       `json:"hadParseError,omitempty"` // tool calls were present but all failed to parse
	FinishReason  string     `json:"finishReason,omitempty"`  // "stop", "length", "content_filter", etc.
	Usage         *Usage     `json:"usage,omitempty"`         // token usage reported by the provider (may be nil)
}

// Usage holds token accounting as reported by OpenAI-compatible APIs.
// CachedTokens counts prompt tokens served from the provider's prompt cache
// (billed at a discount or free depending on the host).
type Usage struct {
	PromptTokens       int `json:"promptTokens"`
	CompletionTokens   int `json:"completionTokens"`
	TotalTokens        int `json:"totalTokens"`
	CachedPromptTokens int `json:"cachedPromptTokens"` // subset of PromptTokens served from cache
	ReasoningTokens    int `json:"reasoningTokens"`    // subset of CompletionTokens spent on reasoning
}

// ReasoningEffortController is optionally implemented by providers that
// support changing reasoning effort at runtime.
type ReasoningEffortController interface {
	SetReasoningEffort(string)
	GetReasoningEffort() string
}

// DefaultReasoningLevels is the default reasoning-effort vocabulary:
// the OpenAI standard levels plus "minimal" (used by newer OpenAI models).
var DefaultReasoningLevels = []string{"none", "minimal", "low", "medium", "high"}

// NormalizeReasoningEffort validates a reasoning level against the default
// vocabulary. See NormalizeReasoningEffortIn.
func NormalizeReasoningEffort(value string) (string, bool) {
	return NormalizeReasoningEffortIn(value, nil)
}

// NormalizeReasoningEffortIn validates a reasoning level against an explicit
// vocabulary. When levels is empty, DefaultReasoningLevels applies. Values are
// compared case-insensitively and trimmed; the canonical (lowercased) form is
// returned. When levels is non-empty and the value is not in it, the value is
// rejected (operators own their vocabulary — no silent remapping).
func NormalizeReasoningEffortIn(value string, levels []string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", false
	}
	if len(levels) == 0 {
		levels = DefaultReasoningLevels
	}
	for _, lvl := range levels {
		if value == strings.ToLower(strings.TrimSpace(lvl)) {
			return value, true
		}
	}
	return "", false
}

// ReasoningLevelsOf returns the effective reasoning vocabulary for a
// provider (DefaultReasoningLevels when unsupported).
func ReasoningLevelsOf(provider LLMProvider) []string {
	if c, ok := provider.(interface{ GetReasoningLevels() []string }); ok {
		return c.GetReasoningLevels()
	}
	return DefaultReasoningLevels
}

// ReasoningEffortAllowedOn reports whether effort is in provider's vocabulary.
func ReasoningEffortAllowedOn(provider LLMProvider, effort string) bool {
	if c, ok := provider.(interface{ ReasoningEffortAllowed(string) bool }); ok {
		return c.ReasoningEffortAllowed(effort)
	}
	_, ok := NormalizeReasoningEffortIn(effort, nil)
	return ok
}

// SetReasoningEffort changes reasoning effort when the provider supports it.
func SetReasoningEffort(provider LLMProvider, effort string) bool {
	controller, ok := provider.(ReasoningEffortController)
	if !ok {
		return false
	}

	controller.SetReasoningEffort(effort)
	return true
}

// GetReasoningEffort returns the provider's current reasoning effort.
func GetReasoningEffort(provider LLMProvider) (string, bool) {
	controller, ok := provider.(ReasoningEffortController)
	if !ok {
		return "", false
	}

	return controller.GetReasoningEffort(), true
}

// LLMProvider is the interface used by the agent loop to call LLMs.
type LLMProvider interface {
	// Chat sends messages to the model and returns a normalized response.
	Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string) (LLMResponse, error)

	// GetDefaultModel returns the provider's default model string.
	GetDefaultModel() string

	// GetModelContext queries the provider for the model's context window size
	// in tokens. Returns 0 and a nil error if unknown (caller applies defaults).
	GetModelContext(ctx context.Context, model string) (int, error)
}

// logVerboseJSON emits a labeled single-line JSON payload to the log. Used by
// verbose and analytics modes to dump LLM traffic (requests, responses,
// usage stats). Single-line output keeps log lines extractable with tools
// like grep/jq without needing multi-line record assembly.
func logVerboseJSON(label string, payload interface{}) {
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("%s: <marshal error: %v>", label, err)
		return
	}
	log.Printf("%s: %s", label, b)
}
