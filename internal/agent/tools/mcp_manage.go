package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// MCPRestartTool allows the agent to restart a specific MCP server on demand.
type MCPRestartTool struct {
	mu       sync.Mutex
	callback func(serverName string) (string, error)
}

// NewMCPRestartTool creates the tool. Call SetCallback after AgentLoop is constructed.
func NewMCPRestartTool() *MCPRestartTool {
	return &MCPRestartTool{}
}

// SetCallback wires the restart function from AgentLoop.
func (t *MCPRestartTool) SetCallback(cb func(serverName string) (string, error)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.callback = cb
}

func (t *MCPRestartTool) Name() string { return "mcp_restart" }
func (t *MCPRestartTool) Description() string {
	return "Restart a specific MCP server by name. Useful when a server is unresponsive or needs reconnection. Use mcp_list to see available servers."
}

func (t *MCPRestartTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"server": map[string]interface{}{
				"type":        "string",
				"description": "Name of the MCP server to restart (must match config key)",
			},
		},
		"required": []string{"server"},
	}
}

func (t *MCPRestartTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	serverRaw, ok := args["server"]
	if !ok {
		return "", fmt.Errorf("mcp_restart: 'server' argument required")
	}
	server, ok := serverRaw.(string)
	if !ok {
		return "", fmt.Errorf("mcp_restart: 'server' must be a string")
	}

	t.mu.Lock()
	cb := t.callback
	t.mu.Unlock()

	if cb == nil {
		return "", fmt.Errorf("mcp_restart: not initialized (no callback)")
	}

	return cb(server)
}

// MCPListTool lists all connected MCP servers and their tools.
type MCPListTool struct {
	mu       sync.Mutex
	callback func() string
}

// NewMCPListTool creates the tool. Call SetCallback after AgentLoop is constructed.
func NewMCPListTool() *MCPListTool {
	return &MCPListTool{}
}

// SetCallback wires the list function from AgentLoop.
func (t *MCPListTool) SetCallback(cb func() string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.callback = cb
}

func (t *MCPListTool) Name() string { return "mcp_list" }
func (t *MCPListTool) Description() string {
	return "List all connected MCP servers and their available tools."
}

func (t *MCPListTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func (t *MCPListTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	t.mu.Lock()
	cb := t.callback
	t.mu.Unlock()

	if cb == nil {
		return "No MCP servers configured.", nil
	}

	return cb(), nil
}

// formatMCPServerList builds a human-readable summary of connected MCP servers.
func FormatMCPServerList(clients []MCPClientInfo) string {
	if len(clients) == 0 {
		return "No MCP servers connected."
	}
	var sb strings.Builder
	for _, c := range clients {
		sb.WriteString(fmt.Sprintf("**%s** (%d tools): %s\n", c.Name, len(c.Tools), strings.Join(c.Tools, ", ")))
	}
	return sb.String()
}

// MCPClientInfo is a lightweight snapshot of a connected MCP server.
type MCPClientInfo struct {
	Name  string
	Tools []string
}
