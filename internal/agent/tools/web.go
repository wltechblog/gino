package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WebTool fetches text content from http(s) URLs.
// Args: {"url": "https://..."}

const (
	defaultWebTimeoutS       = 30
	defaultWebMaxBytes       = 1 << 20 // 1 MB
	defaultWebUserAgent      = "GinoAI https://github.com/wltechblog/gino"
	maxAllowedResponseBytes  = 100 << 20 // hard cap: 100 MB regardless of config
)

// textContentTypes are MIME types whose response bodies can be meaningfully
// returned as strings to the LLM. Binary formats should use curl/wget instead.
var textContentPrefixes = []string{
	"text/",
	"application/json",
	"application/xml",
	"application/javascript",
	"application/xhtml+xml",
	"application/x-yaml",
	"application/ld+json",
	"application/atom+xml",
	"application/rss+xml",
	"application/x-www-form-urlencoded",
}

type WebTool struct {
	timeout        time.Duration
	maxBytes       int64
	userAgent      string
	client         *http.Client
}

// NewWebTool creates a WebTool with default settings.
func NewWebTool() *WebTool {
	return NewWebToolWithConfig(0, 0, "")
}

// NewWebToolWithConfig creates a WebTool with configurable options.
// timeoutS=0 or maxBytes=0 use defaults; empty userAgent uses default.
func NewWebToolWithConfig(timeoutS, maxBytes int, userAgent string) *WebTool {
	if timeoutS <= 0 {
		timeoutS = defaultWebTimeoutS
	}
	if maxBytes <= 0 {
		maxBytes = defaultWebMaxBytes
	}
	if maxBytes > maxAllowedResponseBytes {
		maxBytes = maxAllowedResponseBytes
	}
	if userAgent == "" {
		userAgent = defaultWebUserAgent
	}
	return &WebTool{
		timeout:   time.Duration(timeoutS) * time.Second,
		maxBytes:  int64(maxBytes),
		userAgent: userAgent,
		client: &http.Client{
			Timeout: time.Duration(timeoutS) * time.Second,
		},
	}
}

func (t *WebTool) Name() string        { return "web" }
func (t *WebTool) Description() string { return "Fetch web content from a URL" }

func (t *WebTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{
				"type":        "string",
				"description": "The URL to fetch (must be http or https)",
			},
		},
		"required": []string{"url"},
	}
}

func (t *WebTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	u, ok := args["url"].(string)
	if !ok || u == "" {
		return "", fmt.Errorf("web: 'url' argument required")
	}

	// Enforce http/https scheme
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("web: only http and https URLs are supported")
	}

	// Normalize unicode escape sequences some LLMs emit
	u = strings.ReplaceAll(u, `\u0026`, "&")
	u = strings.ReplaceAll(u, `\u003d`, "=")

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", t.userAgent)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Check content type — only return text-based responses
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !isTextContentType(ct) {
		return "", fmt.Errorf("web: unsupported content type %q — only text formats (HTML, JSON, XML, plain text, etc.) are supported; use curl or wget for binary downloads", ct)
	}

	// Read with size limit
	body, err := io.ReadAll(io.LimitReader(resp.Body, t.maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(body)) > t.maxBytes {
		return string(body[:t.maxBytes]) + fmt.Sprintf("\n\n[response truncated at %d bytes]", t.maxBytes), nil
	}
	return string(body), nil
}

// isTextContentType checks whether a Content-Type header indicates a text-based format.
func isTextContentType(ct string) bool {
	ct = strings.ToLower(ct)
	// Strip parameters like "; charset=utf-8"
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	for _, prefix := range textContentPrefixes {
		if strings.HasPrefix(ct, prefix) {
			return true
		}
	}
	return false
}
