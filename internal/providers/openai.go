package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// OpenAIProvider calls an OpenAI-compatible API (OpenAI, OpenRouter, or similar).
type OpenAIProvider struct {
	APIKey        string
	APIBase       string // e.g. https://api.openai.com/v1 or https://openrouter.ai/api/v1
	MaxTokens     int    // 0 means "let the API decide"
	MaxRetries    int    // number of retries on transient errors (default 2)
	RetryBaseWait time.Duration
	Client        *http.Client
}

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
		APIKey:        apiKey,
		APIBase:       strings.TrimRight(apiBase, "/"),
		MaxTokens:     maxTokens,
		MaxRetries:    maxRetries,
		RetryBaseWait: retryBaseWait,
		Client: &http.Client{
			Timeout: time.Duration(timeoutSecs) * time.Second,
		},
	}
}

func (p *OpenAIProvider) GetDefaultModel() string { return "gpt-4o-mini" }

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
	Model     string        `json:"model"`
	Messages  []messageJSON `json:"messages"`
	Tools     []toolWrapper `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
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

type messageJSON struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
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
		Message messageResponseJSON `json:"message"`
	} `json:"choices"`
}

// Chat calls an OpenAI-compatible chat completion endpoint and returns a simplified response.
// On transient errors (timeouts, 429, 5xx) it retries with exponential backoff up to MaxRetries times.
func (p *OpenAIProvider) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string) (LLMResponse, error) {
	if model == "" {
		model = p.GetDefaultModel()
	}

	reqBody := chatRequest{Model: model, Messages: make([]messageJSON, 0, len(messages)), MaxTokens: p.MaxTokens}
	for _, m := range messages {
		mj := messageJSON{Role: m.Role, ToolCallID: m.ToolCallID}
		if len(m.ToolCalls) > 0 && m.Content == "" {
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
		if attempt > 0 {
			backoff := p.RetryBaseWait * time.Duration(1<<(attempt-1))
			log.Printf("LLM retry %d/%d after %v (last error: %v)", attempt, p.MaxRetries, backoff, lastErr)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return LLMResponse{}, ctx.Err()
			}
		}

		// Re-create the request for each attempt (body is a reader, can't reuse)
		req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(b)))
		if err != nil {
			return LLMResponse{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		if p.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		}

		log.Println("LLM request started")
		resp, err := p.Client.Do(req)
		if err != nil {
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
			return LLMResponse{}, err
		}
		resp.Body.Close()

		if len(out.Choices) == 0 {
			return LLMResponse{}, errors.New("OpenAI API returned no choices")
		}

		msg := out.Choices[0].Message
		// If the model requested tool calls, parse them
		if len(msg.ToolCalls) > 0 {
			var tcs []ToolCall
			for _, tc := range msg.ToolCalls {
				var parsed map[string]interface{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err != nil {
					// skip unparseable tool calls
					continue
				}
				tcs = append(tcs, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: parsed})
			}
			if len(tcs) > 0 {
				return LLMResponse{Content: strings.TrimSpace(msg.Content), HasToolCalls: true, ToolCalls: tcs}, nil
			}
		}

		// No tool calls
		return LLMResponse{Content: strings.TrimSpace(msg.Content), HasToolCalls: false}, nil
	}

	// All retries exhausted
	return LLMResponse{}, fmt.Errorf("LLM request failed after %d retries: %w", p.MaxRetries, lastErr)
}
