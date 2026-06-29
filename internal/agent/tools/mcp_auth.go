package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// MCPAuthCallback provides the functions needed by the mcp_auth tool.
type MCPAuthCallback interface {
	// ListPendingOAuth returns server names and auth URLs for servers needing OAuth.
	ListPendingOAuth() map[string]string

	// CompleteOAuth completes the OAuth flow for a server by exchanging the redirect URL.
	CompleteOAuth(serverName, redirectURL string) error

	// ReconnectAfterAuth attempts to reconnect the MCP server after OAuth is complete.
	ReconnectAfterAuth(serverName string) error
}

// MCPAuthTool allows the agent to manage OAuth authentication for MCP servers.
// It can list servers needing auth, and complete the auth flow when the user
// provides the redirect URL from their browser.
type MCPAuthTool struct {
	mu       sync.Mutex
	callback MCPAuthCallback
}

// NewMCPAuthTool creates the tool. Call SetCallback after AgentLoop is constructed.
func NewMCPAuthTool() *MCPAuthTool {
	return &MCPAuthTool{}
}

// SetCallback wires the callback from AgentLoop.
func (t *MCPAuthTool) SetCallback(cb MCPAuthCallback) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.callback = cb
}

func (t *MCPAuthTool) Name() string { return "mcp_auth" }

func (t *MCPAuthTool) Description() string {
	return "Manage OAuth authentication for MCP servers. Actions: 'status' shows servers needing auth, 'complete' finishes OAuth when user provides the redirect URL from their browser. After authenticating, the user will be redirected to a URL like http://localhost:1/callback?code=... — they should copy and paste that full URL back."
}

func (t *MCPAuthTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"description": "'status' to list servers needing auth, 'complete' to finish the flow",
				"enum":        []string{"status", "complete"},
			},
			"server": map[string]interface{}{
				"type":        "string",
				"description": "MCP server name (required for 'complete' action)",
			},
			"redirect_url": map[string]interface{}{
				"type":        "string",
				"description": "The full redirect URL the user pasted from their browser (for 'complete' action)",
			},
		},
		"required": []string{"action"},
	}
}

func (t *MCPAuthTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	t.mu.Lock()
	cb := t.callback
	t.mu.Unlock()

	if cb == nil {
		return "", fmt.Errorf("mcp_auth: not initialized")
	}

	action, _ := args["action"].(string)

	switch action {
	case "status":
		pending := cb.ListPendingOAuth()
		if len(pending) == 0 {
			return "No MCP servers currently require authentication.", nil
		}
		var sb strings.Builder
		sb.WriteString("The following MCP servers need OAuth authentication:\n\n")
		for name, authURL := range pending {
			sb.WriteString(fmt.Sprintf("**%s**:\n%s\n\n", name, authURL))
			sb.WriteString("Ask the user to open this URL, authenticate, then paste the redirect URL from their browser.\n\n")
		}
		return sb.String(), nil

	case "complete":
		serverName, ok := args["server"].(string)
		if !ok || serverName == "" {
			return "", fmt.Errorf("mcp_auth: 'server' is required for 'complete' action")
		}
		redirectURL, ok := args["redirect_url"].(string)
		if !ok || redirectURL == "" {
			return "", fmt.Errorf("mcp_auth: 'redirect_url' is required for 'complete' action")
		}

		if err := cb.CompleteOAuth(serverName, redirectURL); err != nil {
			return "", fmt.Errorf("OAuth completion failed: %w", err)
		}

		// Reconnect the server with the new token
		if err := cb.ReconnectAfterAuth(serverName); err != nil {
			return fmt.Sprintf("OAuth token stored successfully, but reconnection failed: %v. The server will connect on next restart.", err), nil
		}

		return fmt.Sprintf("MCP server %q authenticated and reconnected successfully.", serverName), nil

	default:
		return "", fmt.Errorf("mcp_auth: unknown action %q (use 'status' or 'complete')", action)
	}
}
