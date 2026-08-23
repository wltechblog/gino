package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OpenAIProvider calls an OpenAI-compatible API (OpenAI, OpenRouter, or similar).
type OpenAIProvider struct {
	APIKey            string
	APIBase           string // e.g. https://api.openai.com/v1 or https://openrouter.ai/api/v1
	MaxTokens         int    // 0 means "let the API decide"
	MaxRetries        int    // number of retries on transient errors (default 2)
	RetryBaseWait     time.Duration
	reasoningMu       sync.RWMutex
	ReasoningEffort   string
	PerAttemptTimeout time.Duration // timeout per individual API call attempt
	Client            *http.Client
	Verbose           bool // log full request/response JSON when true
	Analytics         bool // log per-request token usage as JSON when true
}

func (p *OpenAIProvider) SetVerbose(v bool) { p.Verbose = v }

// SetAnalytics enables per-request token usage logging as JSON.
func (p *OpenAIProvider) SetAnalytics(v bool) { p.Analytics = v }

func NewOpenAIProvider(apiKey, apiBase string, timeoutSecs, maxTokens int) *OpenAIProvider {
	return NewOpenAIProviderWithRetry(apiKey, apiBase, timeoutSecs, maxTokens, 2, 2*time.Second)
}

func NewOpenAIProviderWithRetry(apiKey, apiBase string, timeoutSecs, maxTokens, maxRetries int, retryBaseWait time.Duration) *OpenAIProvider {
	if apiBase == "" {
		apiBase = "https://api.openai.com/v1" // sensible default; can be overridden
	}
	if timeoutSecs <= 1 {
		timeoutSecs = 60 // default 60 seconds
	}
	if maxRetries < 0 {
		maxRetries = 0
	}
	if retryBaseWait <= 0 {
		retryBaseWait = 2 * time.Second
	}
	return &OpenAIProvider{
		APIKey:            apiKey,
		APIBase:           strings.TrimRight(apiBase, "/"),
		MaxTokens:         maxTokens,
		MaxRetries:        maxRetries,
		RetryBaseWait:     retryBaseWait,
		PerAttemptTimeout: time.Duration(timeoutSecs) * time.Second,
		// Client has no Timeout — we use per-attempt context timeout instead
		Client: &http.Client{},
	}
}

func (p *OpenAIProvider) GetDefaultModel() string { return "gpt-4o-mini" }

func (p *OpenAIProvider) SetReasoningEffort(effort string) {
	p.reasoningMu.Lock()
	defer p.reasoningMu.Unlock()

	p.ReasoningEffort = effort
}

func (p *OpenAIProvider) GetReasoningEffort() string {
	p.reasoningMu.RLock()
	defer p.reasoningMu.RUnlock()

	return p.ReasoningEffort
}

// modelInfoResponse represents the /v1/models/{model} endpoint response.
type modelInfoResponse struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
	Context *struct {
		ContextWindow int `json:"context_window"`
	} `json:"context,omitempty"`
	// Some providers put it at the top level
	ContextWindow *int `json:"context_window,omitempty"`
	MaxTokens     *int `json:"max_tokens,omitempty"`
}

// GetModelContext queries the provider's /models/{model} endpoint for the
// model's context window size in tokens. Returns 0, nil if the endpoint
// doesn't provide this information (no error so caller can apply defaults).
func (p *OpenAIProvider) GetModelContext(ctx context.Context, model string) (int, error) {
	if model == "" {
		model = p.GetDefaultModel()
	}

	url := fmt.Sprintf("%s/models/%s", p.APIBase, model)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, nil // non-fatal
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.Client.Do(req)
	if err != nil {
		return 0, nil // non-fatal — network error
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, nil // non-fatal — API doesn't support this
	}

	var info modelInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return 0, nil // non-fatal
	}

	// Try nested context.context_window first (some providers)
	if info.Context != nil && info.Context.ContextWindow > 0 {
		return info.Context.ContextWindow, nil
	}
	// Then top-level context_window
	if info.ContextWindow != nil && *info.ContextWindow > 0 {
		return *info.ContextWindow, nil
	}
	// Some providers use max_tokens for the context limit
	if info.MaxTokens != nil && *info.MaxTokens > 0 {
		return *info.MaxTokens, nil
	}

	return 0, nil
}

// isRetryable reports whether an error or HTTP status code is transient and worth retrying.
func isRetryable(err error, statusCode int) bool {
	// Network errors (timeouts, connection refused, TLS handshake failure, etc.)
	if err != nil {
		return true
	}
	// Rate limit
	if statusCode == 429 {
		return true
	}
	// Server errors
	if statusCode >= 500 && statusCode < 600 {
		return true
	}
	return false
}

// Request/response shapes using the modern OpenAI "tools" format.
type chatRequest struct {
	Model           string        `json:"model"`
	Messages        []messageJSON `json:"messages"`
	Tools           []toolWrapper `json:"tools,omitempty"`
	MaxTokens       int           `json:"max_tokens,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

// toolWrapper is the OpenAI tools array element: {"type": "function", "function": {...}}
type toolWrapper struct {
	Type     string      `json:"type"`
	Function functionDef `json:"function"`
}

type functionDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// contentPart represents a single part of a multi-part content array (vision).
type contentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

type messageJSON struct {
	Role       string         `json:"role"`
	Content    interface{}    `json:"content"` // *string or []contentPart
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCallJSON `json:"tool_calls,omitempty"`
}

type toolCallJSON struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function toolCallFunctionJSON `json:"function"`
}

type toolCallFunctionJSON struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type messageResponseJSON struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []toolCallJSON `json:"tool_calls,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      messageResponseJSON `json:"message"`
		FinishReason string              `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageJSON `json:"usage"`
}

// usageJSON mirrors the OpenAI-compatible usage object. Details fields may be
// absent depending on the host; they default to zero.
type usageJSON struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// toUsage converts the wire format into the public Usage type.
func (u *usageJSON) toUsage() *Usage {
	if u == nil {
		return nil
	}
	out := &Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.PromptTokensDetails != nil {
		out.CachedPromptTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

// Chat calls an OpenAI-compatible chat completion endpoint and returns a simplified response.
// On transient errors (timeouts, 429, 5xx) it retries with exponential backoff up to MaxRetries times.
// Each attempt gets a fresh context timeout so a single hung request doesn't consume the entire budget.
func (p *OpenAIProvider) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string) (LLMResponse, error) {
	if model == "" {
		model = p.GetDefaultModel()
	}

	reqBody := chatRequest{
		Model:           model,
		Messages:        make([]messageJSON, 0, len(messages)),
		MaxTokens:       p.MaxTokens,
		ReasoningEffort: p.GetReasoningEffort(),
	}
	for _, m := range messages {
		mj := messageJSON{Role: m.Role, ToolCallID: m.ToolCallID}

		// If the message has images, build a content array for vision models.
		// Works for both "user" and "tool" roles — tool results may contain
		// images produced by tools (screenshots, downloads, etc.)
		if len(m.Images) > 0 && (m.Role == "user" || m.Role == "tool") {
			parts := make([]contentPart, 0, len(m.Images)+1)
			if m.Content != "" {
				parts = append(parts, contentPart{Type: "text", Text: m.Content})
			}
			for _, img := range m.Images {
				parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURLPart{URL: img}})
			}
			mj.Content = parts
		} else if len(m.ToolCalls) > 0 && m.Content == "" {
			mj.Content = nil
		} else {
			c := m.Content
			mj.Content = &c
		}
		// Convert provider ToolCall to JSON-serializable toolCallJSON
		for _, tc := range m.ToolCalls {
			argsBytes, _ := json.Marshal(tc.Arguments)
			mj.ToolCalls = append(mj.ToolCalls, toolCallJSON{
				ID:   tc.ID,
				Type: "function",
				Function: toolCallFunctionJSON{
					Name:      tc.Name,
					Arguments: string(argsBytes),
				},
			})
		}
		reqBody.Messages = append(reqBody.Messages, mj)
	}

	// Include tools in modern format if provided
	if len(tools) > 0 {
		reqBody.Tools = make([]toolWrapper, 0, len(tools))
		for _, t := range tools {
			params := t.Parameters
			if params == nil {
				params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
			}
			reqBody.Tools = append(reqBody.Tools, toolWrapper{
				Type: "function",
				Function: functionDef{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  params,
				},
			})
		}
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return LLMResponse{}, err
	}

	url := fmt.Sprintf("%s/chat/completions", p.APIBase)

	var lastErr error
	for attempt := 0; attempt <= p.MaxRetries; attempt++ {
		// Check if parent context is already cancelled before attempting
		if ctx.Err() != nil {
			return LLMResponse{}, ctx.Err()
		}

		if attempt > 0 {
			// Exponential backoff with jitter: base * 2^(attempt-1) + random jitter
			backoff := p.RetryBaseWait * time.Duration(1<<(attempt-1))
			jitter := time.Duration(rand.Int63n(int64(p.RetryBaseWait)))
			backoff += jitter
			log.Printf("LLM retry %d/%d after %v (last error: %v)", attempt, p.MaxRetries, backoff, lastErr)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return LLMResponse{}, ctx.Err()
			}
		}

		// Fresh per-attempt timeout — this is the key fix
		attemptCtx, cancel := context.WithTimeout(ctx, p.PerAttemptTimeout)
		req, err := http.NewRequestWithContext(attemptCtx, "POST", url, strings.NewReader(string(b)))
		if err != nil {
			cancel()
			return LLMResponse{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		if p.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		}

		log.Println("LLM request started")
		if p.Verbose {
			logVerboseJSON("LLM REQUEST", map[string]interface{}{
				"url":     url,
				"model":   model,
				"attempt": attempt + 1,
				"body":    json.RawMessage(b),
			})
		}
		resp, err := p.Client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			if isRetryable(err, 0) && attempt < p.MaxRetries {
				continue
			}
			return LLMResponse{}, err
		}

		log.Println("LLM request complete")

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			body := strings.TrimSpace(string(bodyBytes))
			apiErr := fmt.Errorf("OpenAI API error: %s - %s", resp.Status, body)
			if body == "" {
				apiErr = fmt.Errorf("OpenAI API error: %s", resp.Status)
			}
			lastErr = apiErr
			if isRetryable(nil, resp.StatusCode) && attempt < p.MaxRetries {
				continue
			}
			return LLMResponse{}, apiErr
		}

		var out chatResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			resp.Body.Close()
			cancel()
			return LLMResponse{}, err
		}
		resp.Body.Close()
		cancel()

		if p.Verbose {
			// Re-serialize the decoded response so verbose output shows exactly
			// what the API returned (including usage details).
			if rb, err := json.Marshal(out); err == nil {
				logVerboseJSON("LLM RESPONSE", map[string]interface{}{
					"status":   resp.Status,
					"response": json.RawMessage(rb),
				})
			}
		}

		if u := out.Usage.toUsage(); u != nil && u.TotalTokens > 0 {
			if p.Analytics {
				logVerboseJSON("LLM USAGE", map[string]interface{}{
					"model":  model,
					"cached": u.CachedPromptTokens > 0,
					"usage":  u,
				})
			}
		}

		if len(out.Choices) == 0 {
			return LLMResponse{}, errors.New("OpenAI API returned no choices")
		}

		msg := out.Choices[0].Message
		finishReason := out.Choices[0].FinishReason
		if finishReason == "length" {
			log.Printf("WARNING: LLM response truncated (finish_reason=length, %d tool calls attempted) — output was cut by max_tokens limit", len(msg.ToolCalls))
		} else if finishReason != "" && finishReason != "stop" {
			log.Printf("NOTE: LLM finish_reason=%s", finishReason)
		}
		// If the model requested tool calls, parse them
		if len(msg.ToolCalls) > 0 {
			var tcs []ToolCall
			skipped := 0
			for _, tc := range msg.ToolCalls {
				var parsed map[string]interface{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err != nil {
					// skip unparseable tool calls
					skipped++
					log.Printf("WARNING: skipping unparseable tool call %q (id=%s): %v — raw args: %s",
						tc.Function.Name, tc.ID, err, tc.Function.Arguments)
					continue
				}
				tcs = append(tcs, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: parsed})
			}
			if len(tcs) > 0 {
				if skipped > 0 {
					log.Printf("WARNING: %d/%d tool calls were unparseable and dropped", skipped, len(msg.ToolCalls))
				}
				return LLMResponse{Content: strings.TrimSpace(msg.Content), HasToolCalls: true, ToolCalls: tcs, FinishReason: finishReason, Usage: out.Usage.toUsage()}, nil
			}
			// All tool calls were unparseable — don't silently end the turn.
			// Signal the parse error so the loop can inject feedback to the LLM.
			if skipped > 0 {
				log.Printf("WARNING: all %d tool calls were unparseable — injecting error feedback to LLM", skipped)
				return LLMResponse{
					Content:       strings.TrimSpace(msg.Content),
					HasToolCalls:  false,
					HadParseError: true,
					FinishReason:  finishReason,
					Usage:         out.Usage.toUsage(),
				}, nil
			}
		}

		// No tool calls
		return LLMResponse{Content: strings.TrimSpace(msg.Content), HasToolCalls: false, FinishReason: finishReason, Usage: out.Usage.toUsage()}, nil
	}

	// All retries exhausted
	return LLMResponse{}, fmt.Errorf("LLM request failed after %d retries: %w", p.MaxRetries, lastErr)
}
