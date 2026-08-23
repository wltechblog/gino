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

// NormalizeReasoningEffort validates the OpenAI-compatible reasoning levels.
func NormalizeReasoningEffort(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))

	switch value {
	case "none", "low", "medium", "high":
		return value, true
	default:
		return "", false
	}
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

// logVerboseJSON pretty-prints a labeled JSON payload to the log. Used by
// verbose mode to dump LLM traffic (requests, responses, analytics).
func logVerboseJSON(label string, payload interface{}) {
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("%s: <marshal error: %v>", label, err)
		return
	}
	log.Printf("%s: %s", label, b)
}
