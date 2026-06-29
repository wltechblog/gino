package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Tool describes a tool exposed by an MCP server.
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"inputSchema,omitempty"`
}

// Client connects to a single MCP server and exposes its tools.
type Client struct {
	name      string
	transport transport
	nextID    atomic.Int64
	tools     []Tool
	// oauth is non-nil when the HTTP transport has an OAuth manager.
	oauth *oauthManager
	// url is the server URL (set for HTTP transports, used for token storage).
	url string
}

// NewStdioClient creates a client that spawns a child process and communicates via stdin/stdout.
func NewStdioClient(name, command string, args []string) (*Client, error) {
	return NewStdioClientWithEnv(name, command, args, nil)
}

// NewStdioClientWithEnv creates a client that spawns a child process with additional environment variables.
func NewStdioClientWithEnv(name, command string, args []string, extraEnv map[string]string) (*Client, error) {
	t, err := newStdioTransport(command, args, extraEnv)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	c := &Client{name: name, transport: t}
	if err := c.initialize(); err != nil {
		_ = t.close()
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	if err := c.loadTools(); err != nil {
		_ = t.close()
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	return c, nil
}

// NewHTTPClient creates a client that communicates via Streamable HTTP.
func NewHTTPClient(name, url string, headers map[string]string) (*Client, error) {
	t := newHTTPTransport(url, headers)
	c := &Client{name: name, transport: t, url: url}
	if err := c.initialize(); err != nil {
		_ = t.close()
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	if err := c.loadTools(); err != nil {
		_ = t.close()
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	return c, nil
}

// NewHTTPClientWithOAuth creates an HTTP client with OAuth support.
// tokenStore is used to cache tokens; if nil, no OAuth support is available.
func NewHTTPClientWithOAuth(name, serverURL string, headers map[string]string, tokenStore *TokenStore) (*Client, error) {
	if tokenStore != nil {
		// Check if we already have a cached token
		if token, ok := tokenStore.GetToken(serverURL); ok {
			if token.IsExpired() {
				// Try refresh
				om := newOAuthManager(serverURL, name, tokenStore)
				newToken, err := om.refreshToken(&token)
				if err != nil {
					log.Printf("mcp %s: token refresh failed: %v", name, err)
					tokenStore.DeleteToken(serverURL)
				} else {
					tokenStore.SetToken(serverURL, *newToken)
					// Merge token into headers
					if headers == nil {
						headers = make(map[string]string)
					}
					headers["Authorization"] = "Bearer " + newToken.AccessToken
				}
			} else {
				// Use cached token
				if headers == nil {
					headers = make(map[string]string)
				}
				headers["Authorization"] = "Bearer " + token.AccessToken
			}
		}
	}

	t := newHTTPTransport(serverURL, headers)
	c := &Client{name: name, transport: t, url: serverURL}
	if tokenStore != nil {
		c.oauth = newOAuthManager(serverURL, name, tokenStore)
	}

	if err := c.initialize(); err != nil {
		// If this is an OAuth-required error and we have an OAuth manager, begin the OAuth flow
		var reqErr *ErrOAuthRequired
		if errors.As(err, &reqErr) && c.oauth != nil {
			authURL, beginErr := c.oauth.beginAuth()
			if beginErr == nil {
				return nil, &ErrOAuthRequired{
					ServerName: name,
					AuthURL:    authURL,
					ServerKey:  serverURL,
				}
			}
			// beginAuth failed — fall through to return the original error
			log.Printf("mcp %s: OAuth beginAuth failed: %v", name, beginErr)
		}
		_ = t.close()
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	if err := c.loadTools(); err != nil {
		_ = t.close()
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	return c, nil
}

// CompleteOAuthAuth completes a pending OAuth flow by exchanging the redirect URL
// for a token. This is called after the user has authenticated and pasted back
// the redirect URL. It returns a new connected Client.
func CompleteOAuthAuth(name, serverURL, redirectURL string, headers map[string]string, tokenStore *TokenStore) (*Client, error) {
	om := newOAuthManager(serverURL, name, tokenStore)
	if err := om.completeAuth(redirectURL); err != nil {
		return nil, err
	}
	// Now connect with the new token
	return NewHTTPClientWithOAuth(name, serverURL, headers, tokenStore)
}

// ServerURL returns the URL of the server (empty for stdio transports).
func (c *Client) ServerURL() string { return c.url }

// Name: returns the server name.
func (c *Client) Name() string { return c.name }

// Tools: returns the tools discovered from this server.
func (c *Client) Tools() []Tool { return c.tools }

// CallTool: invokes a tool on the MCP server and returns the text result.
func (c *Client) CallTool(_ context.Context, toolName string, arguments map[string]interface{}) (string, error) {
	params := map[string]interface{}{
		"name":      toolName,
		"arguments": arguments,
	}
	result, err := c.request("tools/call", params)
	if err != nil {
		return "", err
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
		IsError bool `json:"isError,omitempty"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", fmt.Errorf("parse tools/call: %w", err)
	}
	var sb strings.Builder
	for _, item := range resp.Content {
		if item.Type == "text" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(item.Text)
		}
	}
	text := sb.String()
	if resp.IsError {
		return "", fmt.Errorf("tool error: %s", text)
	}
	return text, nil
}

// Close shuts down the MCP server connection.
func (c *Client) Close() error { return c.transport.close() }

/*** internal helpers ***/

func (c *Client) request(method string, params interface{}) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	req := rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.transport.roundTrip(b)
	if err != nil {
		return nil, err
	}
	var rr rpcResponse
	if err := json.Unmarshal(resp, &rr); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}
	if rr.Error != nil {
		return nil, rr.Error
	}
	return rr.Result, nil
}

// Version is the MCP client version reported during initialization.
// Override this from main.go via mcp.Version = version.
var Version = "0.4.0"

func (c *Client) initialize() error {
	params := map[string]interface{}{
		"protocolVersion": "2025-03-26",
		"clientInfo": map[string]interface{}{
			"name":    "gino",
			"version": Version,
		},
		"capabilities": map[string]interface{}{},
	}
	if _, err := c.request("initialize", params); err != nil {
		// If this is a 401/403 and we have an OAuth manager, return ErrOAuthRequired
		var oauthErr *oauthHTTPError
		if errors.As(err, &oauthErr) && c.oauth != nil {
			return &ErrOAuthRequired{
				ServerName: c.name,
				ServerKey:  c.url,
			}
		}
		return fmt.Errorf("initialize: %w", err)
	}
	// Send the required initialized notification (fire-and-forget).
	notif := rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}
	b, _ := json.Marshal(notif)
	return c.transport.notify(b)
}

func (c *Client) loadTools() error {
	result, err := c.request("tools/list", nil)
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	var resp struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return fmt.Errorf("parse tools/list: %w", err)
	}
	c.tools = resp.Tools
	return nil
}

/*** JSON-RPC 2.0 types ***/

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int64      `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

/*** transport interface ***/

type transport interface {
	roundTrip(req []byte) ([]byte, error) // send request, read response
	notify(req []byte) error              // fire-and-forget notification
	close() error
}

/*** Stdio transport ***/

type stdioTransport struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	rdr   *bufio.Reader
	mu    sync.Mutex
}

func newStdioTransport(command string, args []string, extraEnv map[string]string) (*stdioTransport, error) {
	cmd := exec.Command(command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	// Inject extra environment variables for the child process
	if len(extraEnv) > 0 {
		cmd.Env = os.Environ()
		for k, v := range extraEnv {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}

	scanner := bufio.NewReaderSize(stdout, 1<<20)

	return &stdioTransport{cmd: cmd, stdin: stdin, rdr: scanner}, nil
}

func (t *stdioTransport) roundTrip(req []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, err := t.stdin.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	for {
		line, err := t.rdr.ReadBytes('\n')
		if err != nil {
			if len(line) > 0 {
				log.Printf("[mcp] partial response from server (%d bytes): %s", len(line), truncate(string(line), 2000))
			}
			return nil, fmt.Errorf("read: %w", err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var probe struct {
			ID *json.RawMessage `json:"id"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.ID != nil {
			if len(line) > 1<<20 {
				log.Printf("[mcp] large response from server (%d bytes): %s", len(line), truncate(string(line), 2000))
			}
			return append([]byte(nil), line...), nil
		}
	}
}

func (t *stdioTransport) notify(req []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err := t.stdin.Write(append(req, '\n'))
	return err
}

func (t *stdioTransport) close() error {
	_ = t.stdin.Close()
	if t.cmd.Process != nil {
		return t.cmd.Process.Kill()
	}
	return nil
}

/*** HTTP transport (Streamable HTTP) ***/

type httpTransport struct {
	url       string
	headers   map[string]string
	client    *http.Client
	sessionID string
	mu        sync.Mutex
}

func newHTTPTransport(url string, headers map[string]string) *httpTransport {
	return &httpTransport{
		url:     url,
		headers: headers,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (t *httpTransport) roundTrip(req []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.doPost(req)
}

func (t *httpTransport) notify(req []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err := t.doPost(req)
	return err
}

func (t *httpTransport) doPost(body []byte) ([]byte, error) {
	httpReq, err := http.NewRequest("POST", t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		httpReq.Header.Set(k, v)
	}
	if t.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", t.sessionID)
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.sessionID = sid
	}

	if resp.StatusCode == http.StatusAccepted {
		return []byte("{}"), nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		// Return an oauthError to signal that OAuth is needed.
		// The caller (Client) can check for this and initiate the OAuth flow.
		return nil, &oauthHTTPError{
			StatusCode:    resp.StatusCode,
			WWWAuthenticate: resp.Header.Get("WWW-Authenticate"),
			Body:          string(b),
		}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return parseSSE(resp.Body)
	}
	return io.ReadAll(resp.Body)
}

// oauthHTTPError wraps a 401/403 response so the caller can detect OAuth requirement.
type oauthHTTPError struct {
	StatusCode      int
	WWWAuthenticate string
	Body            string
}

func (e *oauthHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

func (t *httpTransport) close() error { return nil }

// parseSSE extracts the first JSON-RPC response from an SSE stream.
func parseSSE(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var probe struct {
				ID *json.RawMessage `json:"id"`
			}
			if json.Unmarshal([]byte(data), &probe) == nil && probe.ID != nil {
				return []byte(data), nil
			}
		}
	}
	return nil, fmt.Errorf("no response in SSE stream")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("... [%d bytes truncated]", len(s)-n)
}
