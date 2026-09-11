package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/config"
)

// TestSpawnAsyncSurvivesTurnCancel reproduces the field bug (report
// 011-spawn-researcher-context-canceled-at-first-llm-call): an async spawn
// (wait=false) was launched with runCtx derived from the TURN context, so
// when the parent turn finished and its context was canceled, the child
// died with "context canceled" mid-task. The async child must be detached —
// only its own timeout or an explicit spawn cancel may kill it.
func TestSpawnAsyncSurvivesTurnCancel(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "alive.txt")
	t.Setenv("SPAWN_ALIVE_FILE", marker)
	binary := writeFakeGino(t, `sleep 2; echo SURVIVED > "$SPAWN_ALIVE_FILE"; echo ASYNC-DONE`)
	tk := spawnTestTool(t, binary, t.TempDir())

	// Parent turn context — canceled right after spawn returns, exactly
	// like the agent loop tearing down a finished turn.
	turnCtx, turnCancel := context.WithCancel(context.Background())
	out, err := tk.Execute(turnCtx, map[string]interface{}{
		"agent": "bg",
		"task":  "long research task",
		"wait":  false,
	})
	turnCancel()
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(out, "started:") {
		t.Errorf("unexpected ack: %q", out)
	}

	// The child sleeps 2s then writes the marker. Old code: ctx cancel
	// killed it immediately, marker never appeared.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("child was killed by turn cancel (the field bug): %v", err)
	}

	// And the result still delivers on the hub.
	recv := time.After(10 * time.Second)
	for {
		select {
		case delivered := <-tk.hub.In:
			if strings.Contains(delivered.Content, "ASYNC-DONE") {
				return
			}
		case <-recv:
			t.Fatal("no hub delivery within 10s after surviving cancel")
		}
	}
}

// TestSpawnSyncKilledByTurnCancel pins the intentional sync semantics: a
// wait=true child is part of the turn, so canceling the turn kills it.
func TestSpawnSyncKilledByTurnCancel(t *testing.T) {
	binary := writeFakeGino(t, `sleep 30`)
	tk := spawnTestTool(t, binary, t.TempDir())

	turnCtx, turnCancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() {
		out, err := tk.Execute(turnCtx, map[string]interface{}{
			"agent": "bg",
			"task":  "sync task",
		})
		if err != nil {
			t.Errorf("sync spawn errored: %v", err)
		}
		done <- out
	}()

	time.Sleep(300 * time.Millisecond) // let the child start
	turnCancel()

	select {
	case out := <-done:
		if !strings.Contains(out, "✗") && !strings.Contains(out, "failed") {
			t.Errorf("expected failure report after turn cancel, got: %q", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sync spawn did not return after turn cancel")
	}
}

// TestSpawnAsyncCancelStillWorks ensures explicit cancel (spawn action=cancel)
// still terminates a detached async child — detachment must not make tasks
// uncancellable.
func TestSpawnAsyncCancelStillWorks(t *testing.T) {
	binary := writeFakeGino(t, "sleep 30\n")
	tk := spawnTestTool(t, binary, t.TempDir())
	hub := tk.hub
	_ = hub

	started, err := tk.Execute(context.Background(), map[string]interface{}{
		"agent": "bg",
		"task":  "cancellable task",
		"wait":  false,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	id := strings.TrimPrefix(strings.Fields(started)[1], "id=")

	if err := tk.cancelTask(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := tk.Execute(context.Background(), map[string]interface{}{"action": "list"}); strings.Contains(out, "no running") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("detached task not cancellable")
}

// spawnTestConfig builds a minimal enabled SpawnConfig for ad-hoc tools.
func spawnTestConfig(binary string) config.SpawnConfig {
	return config.SpawnConfig{
		Enabled:         true,
		Binary:          binary,
		DefaultTimeoutS: 15,
		MaxConcurrent:   2,
	}
}

// TestSpawnAsyncHubNilNoPanic guards the disabled-tool construction path
// (hub may be nil) when an async task finishes and tries to deliver.
func TestSpawnAsyncHubNilNoPanic(t *testing.T) {
	binary := writeFakeGino(t, "echo NIL-HUB-OUTPUT\n")
	tk := NewSpawnToolDisabled(t.TempDir(), t.TempDir(), nil)
	tk.Configure(spawnTestConfig(binary), "")

	if _, err := tk.Execute(context.Background(), map[string]interface{}{
		"task": "nil hub task",
		"wait": false,
	}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Give the goroutine time to finish and hit the nil-hub delivery path.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := tk.Execute(context.Background(), map[string]interface{}{"action": "list"}); strings.Contains(out, "no running") {
			return // completed without panic
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("task never completed")
}
