package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
)

// writeFakeGino creates a stand-in "gino agent" binary that dumps its argv
// to $SPAWN_ARGS_FILE (if set) and prints a fixed marker.
func writeFakeGino(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-gino")
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gino: %v", err)
	}
	return path
}

func spawnTestTool(t *testing.T, binary string, ws string) *SpawnTool {
	t.Helper()
	cfg := config.SpawnConfig{
		Enabled:         true,
		Binary:          binary,
		DefaultTimeoutS: 15,
		MaxConcurrent:   2,
	}
	hub := chat.NewHub(10)
	return NewSpawnTool(cfg, t.TempDir(), ws, hub)
}

func TestSpawnSyncRunsChildAndReturnsOutput(t *testing.T) {
	binary := writeFakeGino(t, "echo HELLO-FROM-CHILD\necho LINE2\n")
	tk := spawnTestTool(t, binary, t.TempDir())

	out, err := tk.Execute(t.Context(), map[string]interface{}{
		"agent": "docs-research",
		"task":  "summarize the readme",
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(out, "HELLO-FROM-CHILD") || !strings.Contains(out, "LINE2") {
		t.Errorf("child output missing: %q", out)
	}
	if !strings.Contains(out, "completed in") {
		t.Errorf("completion line missing: %q", out)
	}
}

func TestSpawnDisabledWithoutConfigure(t *testing.T) {
	hub := chat.NewHub(10)
	tk := NewSpawnToolDisabled(t.TempDir(), t.TempDir(), hub)
	if _, err := tk.Execute(t.Context(), map[string]interface{}{"task": "x"}); err == nil {
		t.Fatal("expected error for disabled spawn tool")
	} else if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestSpawnChildArgv(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("SPAWN_ARGS_FILE", argsFile)
	binary := writeFakeGino(t, `printf '%s\n' "$@" > "$SPAWN_ARGS_FILE"`)
	tk := spawnTestTool(t, binary, t.TempDir())

	if _, err := tk.Execute(t.Context(), map[string]interface{}{
		"task": "do a thing",
	}); err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("child never wrote argv: %v", err)
	}
	argv := strings.Split(strings.TrimSpace(string(data)), "\n")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"agent", "-session", "sp:", "-disable-tools"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %v", want, argv)
		}
	}
	// find the -disable-tools value
	for i, a := range argv {
		if a == "-disable-tools" && i+1 < len(argv) {
			for _, d := range []string{"spawn", "message", "cron", "write_memory"} {
				if !strings.Contains(argv[i+1], d) {
					t.Errorf("child disable list missing %q: %q", d, argv[i+1])
				}
			}
		}
	}
}

func TestSpawnTimeoutReportsPartialOutput(t *testing.T) {
	binary := writeFakeGino(t, "echo PARTIAL-BEFORE-SLEEP\nsleep 30\n")
	tk := spawnTestTool(t, binary, t.TempDir())

	out, err := tk.Execute(t.Context(), map[string]interface{}{
		"task":     "slow task",
		"timeoutS": float64(1),
	})
	if err != nil {
		t.Fatalf("timeout should be reported as a result, not error: %v", err)
	}
	if !strings.Contains(out, "timed out") || !strings.Contains(out, "PARTIAL-BEFORE-SLEEP") {
		t.Errorf("timeout report missing: %q", out)
	}
}

func TestSpawnAsyncDeliversToHub(t *testing.T) {
	binary := writeFakeGino(t, "echo ASYNC-RESULT\n")
	tk := spawnTestTool(t, binary, t.TempDir())

	out, err := tk.Execute(t.Context(), map[string]interface{}{
		"agent": "bg",
		"task":  "async job",
		"wait":  false,
	})
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(out, "started:") || !strings.Contains(out, "will be delivered") {
		t.Errorf("unexpected start ack: %q", out)
	}

	// Wait for delivery on the hub.
	deadline := time.Now().Add(10 * time.Second)
	var delivered chat.Inbound
	for time.Now().Before(deadline) {
		select {
		case delivered = <-tk.hub.In:
			goto done
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("no hub delivery within 10s")
done:
	if !strings.Contains(delivered.Content, "ASYNC-RESULT") {
		t.Errorf("delivered content missing result: %q", delivered.Content)
	}
	if delivered.Metadata["signal_action"] != "spawn_task" {
		t.Errorf("metadata missing spawn_task action: %v", delivered.Metadata)
	}
}

func TestSpawnConcurrentCap(t *testing.T) {
	binary := writeFakeGino(t, "sleep 30\n")
	tk := spawnTestTool(t, binary, t.TempDir())
	tk.Configure(config.SpawnConfig{Enabled: true, Binary: binary, DefaultTimeoutS: 15, MaxConcurrent: 1}, "")

	started, err := tk.Execute(t.Context(), map[string]interface{}{"task": "one", "wait": false})
	if err != nil {
		t.Fatalf("first spawn failed: %v", err)
	}
	id := strings.TrimPrefix(strings.Fields(started)[1], "id=")

	if _, err := tk.Execute(t.Context(), map[string]interface{}{"task": "two"}); err == nil {
		t.Fatal("second spawn should have been rejected")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Errorf("wrong error: %v", err)
	}

	if err := tk.cancelTask(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// Give the goroutine a moment to remove the task, then confirm capacity freed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := tk.Execute(t.Context(), map[string]interface{}{"task": "three", "wait": false}); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("capacity not freed after cancel")
}

func TestSpawnListAndCancel(t *testing.T) {
	binary := writeFakeGino(t, "sleep 30\n")
	tk := spawnTestTool(t, binary, t.TempDir())

	if out, _ := tk.Execute(t.Context(), map[string]interface{}{"action": "list"}); !strings.Contains(out, "no running") {
		t.Errorf("empty list unexpected: %q", out)
	}
	started, err := tk.Execute(t.Context(), map[string]interface{}{"task": "listable task", "agent": "lab", "wait": false})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	id := strings.TrimPrefix(strings.Fields(started)[1], "id=")

	list, _ := tk.Execute(t.Context(), map[string]interface{}{"action": "list"})
	if !strings.Contains(list, id) || !strings.Contains(list, "lab") {
		t.Errorf("list missing task: %q", list)
	}
	// cancel returns ("", err) on failure; success returns ("", nil). Both are
	// ignored — the poll below verifies the task was actually removed.
	_, _ = tk.Execute(t.Context(), map[string]interface{}{"action": "cancel", "id": id})
	// wait for the task to be removed
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := tk.Execute(t.Context(), map[string]interface{}{"action": "list"}); strings.Contains(out, "no running") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("task still listed after cancel")
}

func TestSpawnSessionNameNormalization(t *testing.T) {
	tk := NewSpawnToolDisabled(t.TempDir(), t.TempDir(), nil)
	got := tk.normalizeSession("My Weird/Task!!", "irrelevant")
	if got != "My-Weird-Task" {
		t.Errorf("normalized to %q", got)
	}
	if len(tk.normalizeSession(strings.Repeat("a", 100), "x")) > spSessionMaxLen {
		t.Error("session name not length-capped")
	}
}

func TestSpawnValidation(t *testing.T) {
	binary := writeFakeGino(t, "true\n")
	tk := spawnTestTool(t, binary, t.TempDir())
	if _, err := tk.Execute(t.Context(), map[string]interface{}{}); err == nil || !strings.Contains(err.Error(), "'task' is required") {
		t.Errorf("missing task not rejected: %v", err)
	}
	big := strings.Repeat("a", 101*1024)
	if _, err := tk.Execute(t.Context(), map[string]interface{}{"task": big}); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("oversized task not rejected: %v", err)
	}
	if _, err := tk.Execute(t.Context(), map[string]interface{}{"action": "bogus"}); err == nil {
		t.Error("unknown action accepted")
	}
}
