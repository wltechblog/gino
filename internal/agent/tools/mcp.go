package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wltechblog/gino/internal/mcp"
)

// MCPTool wraps a single MCP server tool to implement the gino Tool interface.
type MCPTool struct {
	client     *mcp.Client
	serverName string
	tool       mcp.Tool
}

// mcpImageCounter provides unique file names for MCP image results.
var mcpImageCounter atomic.Int64

// mcpImageDir is where MCP tool image results are written. It is configured
// once at startup from the agent workspace (uploads/mcp) and falls back to
// os.TempDir when unset (e.g. in tests that never call SetMCPImageDir).
var mcpImageDir atomic.Value // string

// SetMCPImageDir sets the directory where MCP tool image results are saved.
func SetMCPImageDir(dir string) { mcpImageDir.Store(dir) }

// saveMCPImages writes images returned by an MCP tool call to disk and
// returns the list of written file paths. Images that fail to save are noted
// in the returned errors slice; callers append them to the tool result so the
// agent knows something went wrong without failing the whole call.
func saveMCPImages(images []mcp.ToolImage) ([]string, string) {
	if len(images) == 0 {
		return nil, ""
	}
	dir, _ := mcpImageDir.Load().(string)
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "gino-mcp-images")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Sprintf(" (failed to create image dir: %v)", err)
	}
	var paths []string
	var problems []string
	for i, img := range images {
		name := fmt.Sprintf("mcp-img-%s-%06d-%02d%s", time.Now().Format("20060102-150405"), mcpImageCounter.Add(1), i+1, img.Ext)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, img.Data, 0o644); err != nil {
			problems = append(problems, fmt.Sprintf("image %d: %v", i+1, err))
			continue
		}
		paths = append(paths, path)
	}
	var note string
	if len(problems) > 0 {
		note = fmt.Sprintf(" (failed to save %d image(s): %s)", len(problems), joinStrings(problems, "; "))
	}
	return paths, note
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// NewMCPTool creates a Tool that delegates execution to an MCP server.
func NewMCPTool(client *mcp.Client, serverName string, tool mcp.Tool) *MCPTool {
	return &MCPTool{client: client, serverName: serverName, tool: tool}
}

func (t *MCPTool) Name() string {
	return fmt.Sprintf("mcp_%s_%s", t.serverName, t.tool.Name)
}

func (t *MCPTool) Description() string {
	desc := t.tool.Description
	if desc == "" {
		desc = fmt.Sprintf("MCP tool %s from server %s", t.tool.Name, t.serverName)
	}
	return fmt.Sprintf("[MCP: %s] %s", t.serverName, desc)
}

func (t *MCPTool) Parameters() map[string]interface{} {
	return t.tool.InputSchema
}

func (t *MCPTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	text, images, err := t.client.CallToolWithImages(ctx, t.tool.Name, args)
	if err != nil {
		return "", err
	}
	if len(images) == 0 {
		return text, nil
	}
	paths, note := saveMCPImages(images)
	var sb strings.Builder
	if text != "" {
		sb.WriteString(text)
		sb.WriteString("\n")
	}
	if len(paths) > 0 {
		sb.WriteString("[image result(s) saved to disk — use the vision tool on these file paths to analyse them:")
		for _, p := range paths {
			sb.WriteString("\n- " + p)
		}
		sb.WriteString("]")
	}
	if note != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(strings.TrimPrefix(strings.TrimLeft(note, " "), "\n"))
	}
	return sb.String(), nil
}
