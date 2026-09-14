package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/signal"
)

// fakeMCPServer is a minimal stdio MCP server that declares signal actions
// in its initialize result. Written as a shell script so it works without
// any runtime dependencies beyond sh + printf (printf %s is POSIX).
const fakeMCPWithSignals = `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"fake"},"signals":{"actions":[{"name":"job_done","description":"A job finished"},{"name":"alert_raised","description":"Alert","silent":true}]}}}'
      ;;
    *'"method":"notifications/initialized"'*) ;; # fire-and-forget, no reply
    *'"method":"tools/list"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"tools":[]}}'
      ;;
  esac
done
`

// TestSetSignalRegistrySweepsConnectedClients pins the main.go ordering
// (loop constructed BEFORE the signal registry is wired): the sweep in
// SetSignalRegistry must pick up declarations from already-connected clients.
func TestSetSignalRegistrySweepsConnectedClients(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-mcp.sh")
	if err := os.WriteFile(script, []byte(fakeMCPWithSignals), 0o755); err != nil {
		t.Fatal(err)
	}

	b := chat.NewHub(64)
	prov := &toolLoopProvider{}
	mcpCfg := map[string]config.MCPServerConfig{
		"fake": {Command: "sh", Args: []string{script}},
	}
	ag := NewAgentLoop(b, prov, prov.GetDefaultModel(), 5, t.TempDir(), nil, mcpCfg, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	defer ag.Close()

	reg := signal.NewRegistry(nil)
	ag.SetSignalRegistry(reg)

	if !reg.IsAllowed("job_done", "fake") {
		t.Error("job_done should be registered from fake's initialize declaration after sweep")
	}
	if !reg.IsAllowed("alert_raised", "fake") {
		t.Error("alert_raised should be registered")
	}
	if !reg.GetSilentResponse("alert_raised") {
		t.Error("alert_raised declared silent:true")
	}
	if reg.IsAllowed("job_done", "other-server") {
		t.Error("cross-source enforcement broken")
	}
	if reg.GetResponse("job_done") == "" {
		t.Error("default response template should be generated")
	}
}
