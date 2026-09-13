package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// metaEchoServer is a minimal stdio MCP server that records the _meta of
// every tools/call request to a file and answers with the same _meta echoed
// in the tool result text, proving the origin stamp reaches the wire.
const metaEchoServer = `
import json, sys, os

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

send({"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"meta-echo","version":"1"}}})

with open(os.environ["META_LOG"], "w") as f:
    f.write("")

for line in sys.stdin:
    req = json.loads(line)
    if req.get("method") == "tools/list":
        send({"jsonrpc":"2.0","id":req["id"],"result":{"tools":[{"name":"echo_meta","description":"echo _meta","inputSchema":{"type":"object"}}]}})
    elif req.get("method") == "tools/call":
        params = req.get("params") or {}
        with open(os.environ["META_LOG"], "a") as f:
            f.write(json.dumps({"meta": params.get("_meta")}) + "\n")
        send({"jsonrpc":"2.0","id":req["id"],"result":{"content":[{"type":"text","text":"ok"}]}})
`

// TestCallToolStampsMetaOrigin verifies that a context stamped with
// WithTurnOrigin produces a tools/call request carrying _meta with the
// channel/chat_id — the wire hook cooperating signal servers echo back.
func TestCallToolStampsMetaOrigin(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "server.py")
	if err := os.WriteFile(script, []byte(metaEchoServer), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "meta.log")

	client, err := NewStdioClientWithEnv("meta-echo", "python3", []string{script}, map[string]string{
		"META_LOG": logPath,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()

	if len(client.Tools()) == 0 {
		t.Fatal("no tools listed")
	}

	ctx := WithTurnOrigin(context.Background(), "discord", "thread-a")
	if _, _, err := client.CallToolWithImages(ctx, "echo_meta", nil); err != nil {
		t.Fatalf("call: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("no tools/call logged by server")
	}
	var rec struct {
		Meta map[string]interface{} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("parse log: %v", err)
	}
	if rec.Meta["channel"] != "discord" || rec.Meta["chat_id"] != "thread-a" {
		t.Errorf("_meta not stamped: %v", rec.Meta)
	}
}

// TestCallToolWithoutOriginOmitsMeta verifies no _meta is sent when the
// context has no origin (back-compat for plain callers).
func TestCallToolWithoutOriginOmitsMeta(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "server.py")
	if err := os.WriteFile(script, []byte(metaEchoServer), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "meta.log")

	client, err := NewStdioClientWithEnv("meta-echo", "python3", []string{script}, map[string]string{
		"META_LOG": logPath,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()

	if _, _, err := client.CallToolWithImages(context.Background(), "echo_meta", nil); err != nil {
		t.Fatalf("call: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Meta map[string]interface{} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("parse log: %v", err)
	}
	if rec.Meta != nil {
		t.Errorf("expected no _meta, got %v", rec.Meta)
	}
}
