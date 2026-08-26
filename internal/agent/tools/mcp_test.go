package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/mcp"
)

// mcpTestPNG is a minimal valid 1x1 PNG.
var mcpTestPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="

func newTestMCPServer(t *testing.T) (*httptest.Server, *mcp.Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int64          `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		type rpcResp struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int64          `json:"id,omitempty"`
			Result  json.RawMessage `json:"result,omitempty"`
		}

		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(rpcResp{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"capabilities":{}}`)})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(rpcResp{
				JSONRPC: "2.0", ID: req.ID,
				Result: json.RawMessage(`{"tools":[{"name":"upper","description":"uppercases text","inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}},{"name":"snap","description":"takes a screenshot"}]}`),
			})
		case "tools/call":
			var params struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Name == "snap" {
				result := `{"content":[{"type":"text","text":"screenshot captured"}]}`
				if _, plain := params.Arguments["plain"]; !plain {
					result = `{"content":[{"type":"text","text":"screenshot captured"},{"type":"image","data":"` + mcpTestPNG + `","mimeType":"image/png"}]}`
				}
				_ = json.NewEncoder(w).Encode(rpcResp{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(result)})
				return
			}
			text := strings.ToUpper(params.Arguments["text"].(string))
			_ = json.NewEncoder(w).Encode(rpcResp{
				JSONRPC: "2.0", ID: req.ID,
				Result: json.RawMessage(`{"content":[{"type":"text","text":"` + text + `"}]}`),
			})
		}
	}))

	client, err := mcp.NewHTTPClient("testsvr", srv.URL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("NewHTTPClient: %v", err)
	}
	return srv, client
}

func TestMCPToolNameAndDescription(t *testing.T) {
	srv, client := newTestMCPServer(t)
	defer srv.Close()
	defer func() { _ = client.Close() }()

	tools := client.Tools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	var upper *mcp.Tool
	for i, tl := range tools {
		if tl.Name == "upper" {
			upper = &tools[i]
			break
		}
	}
	if upper == nil {
		t.Fatal("upper tool not found")
	}

	mcpTool := NewMCPTool(client, "testsvr", *upper)

	if got := mcpTool.Name(); got != "mcp_testsvr_upper" {
		t.Fatalf("expected name 'mcp_testsvr_upper', got %q", got)
	}
	if !strings.Contains(mcpTool.Description(), "[MCP: testsvr]") {
		t.Fatalf("description should contain server prefix, got %q", mcpTool.Description())
	}
	params := mcpTool.Parameters()
	if params == nil {
		t.Fatal("expected non-nil parameters")
	}
}

func TestMCPToolExecute(t *testing.T) {
	srv, client := newTestMCPServer(t)
	defer srv.Close()
	defer func() { _ = client.Close() }()

	tools := client.Tools()
	mcpTool := NewMCPTool(client, "testsvr", tools[0])

	result, err := mcpTool.Execute(context.Background(), map[string]interface{}{"text": "hello"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "HELLO" {
		t.Fatalf("expected 'HELLO', got %q", result)
	}
}

func TestMCPToolRegistration(t *testing.T) {
	srv, client := newTestMCPServer(t)
	defer srv.Close()
	defer func() { _ = client.Close() }()

	reg := NewRegistry()
	for _, tool := range client.Tools() {
		reg.Register(NewMCPTool(client, "testsvr", tool))
	}

	// Verify the tool is findable via the registry.
	tool := reg.Get("mcp_testsvr_upper")
	if tool == nil {
		t.Fatal("expected to find mcp_testsvr_upper in registry")
	}

	// Verify it shows up in definitions.
	defs := reg.Definitions()
	found := false
	for _, d := range defs {
		if d.Name == "mcp_testsvr_upper" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("mcp_testsvr_upper not found in definitions")
	}
}

func TestMCPToolImageSaveAndHint(t *testing.T) {
	srv, client := newTestMCPServer(t)
	defer srv.Close()
	defer func() { _ = client.Close() }()

	dir := t.TempDir()
	SetMCPImageDir(dir)

	// Get the "snap" tool registered by the fixture server.
	var snap *MCPTool
	for _, tl := range client.Tools() {
		if tl.Name == "snap" {
			snap = NewMCPTool(client, "testsvr", tl)
			break
		}
	}
	if snap == nil {
		t.Fatal("snap tool not found on test server")
	}

	result, err := snap.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "[image result(s) saved to disk") {
		t.Fatalf("expected image hint in result, got: %s", result)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected image file written to disk")
	}
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".png") {
			found = true
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read saved image: %v", err)
			}
			if len(data) < 8 || data[0] != 0x89 || data[1] != 'P' {
				t.Fatalf("saved file is not a PNG: %d bytes", len(data))
			}
		}
	}
	if !found {
		t.Fatalf("no .png written; entries: %v", entries)
	}
	if !strings.Contains(result, dir) {
		t.Fatalf("result should contain the save dir path %s: %s", dir, result)
	}

	// Text-only call: no hint, no file accumulation.
	before, _ := os.ReadDir(dir)
	result, err = snap.Execute(context.Background(), map[string]interface{}{"plain": true})
	if err != nil {
		t.Fatalf("Execute text-only: %v", err)
	}
	if strings.Contains(result, "[image result") {
		t.Fatal("text-only call must not contain image hint")
	}
	after, _ := os.ReadDir(dir)
	if len(after) != len(before) {
		t.Fatal("text-only call must not write image files")
	}
}
