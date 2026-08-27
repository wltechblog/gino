package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
)

// ── Test harness ────────────────────────────────────────────────────────────

// bgSink captures messages injected into hub.In.
type bgSink struct {
	mu   sync.Mutex
	msgs []chat.Inbound
}

// drain consumes hub.In in the background, mirroring the agent loop's reader.
func (s *bgSink) drain(in <-chan chat.Inbound) {
	go func() {
		for m := range in {
			s.mu.Lock()
			s.msgs = append(s.msgs, m)
			s.mu.Unlock()
		}
	}()
}

func bgYoloSandbox() config.SandboxConfig {
	return config.SandboxConfig{Mode: "yolo", AllowStringCommands: true}
}

func newBGTestEnv(t *testing.T) (*BackgroundTool, *chat.Hub, *bgSink) {
	t.Helper()
	hub := chat.NewHub(64)
	sink := &bgSink{}
	sink.drain(hub.In)

	// yolo-mode exec tool so any test command passes validation
	execTool := NewExecToolWithSandbox(30, t.TempDir(), nil, bgYoloSandbox())
	bt := NewBackgroundTool(hub, execTool)
	bt.minInterval = time.Second // allow 1s polls in tests
	return bt, hub, sink
}

func (s *bgSink) waitForMsg(t *testing.T, substr string, timeout time.Duration) chat.Inbound {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for i, m := range s.msgs {
			if strings.Contains(m.Content, substr) {
				// consume everything up to and including the match
				s.msgs = append(s.msgs[:i:i], s.msgs[i+1:]...)
				s.mu.Unlock()
				return m
			}
		}
		s.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for message containing %q", substr)
	return chat.Inbound{}
}

func (s *bgSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

func bgArgs(pairs ...interface{}) map[string]interface{} {
	m := make(map[string]interface{})
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i].(string)] = pairs[i+1]
	}
	return m
}

// ── One-shot jobs ───────────────────────────────────────────────────────────

func TestBackgroundStartCompletesAndNotifies(t *testing.T) {
	bt, _, sink := newBGTestEnv(t)
	bt.SetContext("telegram", "123")

	out, err := bt.Execute(context.TODO(), bgArgs("action", "start", "name", "quick", "cmd", []interface{}{"echo", "hello-result"}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(out, "started") {
		t.Fatalf("unexpected start reply: %s", out)
	}

	msg := sink.waitForMsg(t, "hello-result", 10*time.Second)
	if msg.Channel != "telegram" || msg.ChatID != "123" {
		t.Errorf("notification routed wrong: %s:%s", msg.Channel, msg.ChatID)
	}
	if !strings.Contains(msg.Content, "Status: success") {
		t.Errorf("expected success status, got: %s", msg.Content)
	}
	// Signal metadata: routes to signal session namespace, non-silent
	if msg.Metadata["signal_action"] != "background_job" {
		t.Errorf("missing signal metadata: %v", msg.Metadata)
	}
	if msg.Metadata["signal_silent"] != false {
		t.Errorf("signal must not be silent: %v", msg.Metadata)
	}

	// Job removed after completion
	list, _ := bt.Execute(context.TODO(), bgArgs("action", "list"))
	if strings.Contains(list, "quick") {
		t.Errorf("job should be gone after completion: %s", list)
	}
}

func TestBackgroundStartFailureReport(t *testing.T) {
	bt, _, sink := newBGTestEnv(t)
	bt.SetContext("telegram", "1")

	if _, err := bt.Execute(context.TODO(), bgArgs("action", "start", "name", "failing", "cmd", []interface{}{"sh", "-c", "echo boom >&2; exit 3"})); err != nil {
		t.Fatalf("start: %v", err)
	}
	msg := sink.waitForMsg(t, "boom", 10*time.Second)
	if !strings.Contains(msg.Content, "Status: failed (exit 3)") {
		t.Errorf("expected failed exit 3, got: %s", msg.Content)
	}
}

func TestBackgroundStartTimeoutKills(t *testing.T) {
	bt, _, sink := newBGTestEnv(t)
	bt.SetContext("telegram", "1")

	start := time.Now()
	if _, err := bt.Execute(context.TODO(), bgArgs("action", "start", "name", "sleeper", "timeout", "2s", "cmd", []interface{}{"sleep", "60"})); err != nil {
		t.Fatalf("start: %v", err)
	}
	msg := sink.waitForMsg(t, "timed out", 10*time.Second)
	elapsed := time.Since(start)
	if !strings.Contains(msg.Content, "Status: timed out after 2s") {
		t.Errorf("expected timeout status: %s", msg.Content)
	}
	if elapsed > 8*time.Second {
		t.Errorf("timeout took too long to fire: %v", elapsed)
	}
}

func TestBackgroundCancelKillsRunningJob(t *testing.T) {
	bt, _, sink := newBGTestEnv(t)
	bt.SetContext("telegram", "1")

	if _, err := bt.Execute(context.TODO(), bgArgs("action", "start", "name", "longsleep", "timeout", "5m", "cmd", []interface{}{"sleep", "300"})); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let it spawn

	out, err := bt.Execute(context.TODO(), bgArgs("action", "cancel", "name", "longsleep"))
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !strings.Contains(out, "Cancelled") {
		t.Fatalf("unexpected cancel reply: %s", out)
	}
	msg := sink.waitForMsg(t, "Status: cancelled", 10*time.Second)
	if msg.Content == "" {
		t.Fatal("no cancellation report")
	}
	if sink.count() != 0 {
		t.Errorf("unexpected extra messages after cancel: %d", sink.count())
	}
}

func TestBackgroundMaxOneShotLimit(t *testing.T) {
	bt, _, sink := newBGTestEnv(t)
	bt.SetContext("telegram", "1")

	for i := 0; i < bgMaxOneShot; i++ {
		if _, err := bt.Execute(context.TODO(), bgArgs("action", "start", "name", "s", "cmd", []interface{}{"sleep", "30"})); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	_, err := bt.Execute(context.TODO(), bgArgs("action", "start", "name", "one-too-many", "cmd", []interface{}{"echo", "x"}))
	if err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("expected limit error, got: %v", err)
	}

	bt.Shutdown()
	if sink.count() != 0 {
		t.Errorf("shutdown should not notify for cancelled jobs, got %d msgs", sink.count())
	}
}

// ── Pollers ─────────────────────────────────────────────────────────────────

func TestBackgroundPollChangeDetection(t *testing.T) {
	bt, _, sink := newBGTestEnv(t)
	bt.SetContext("telegram", "9")

	dir := t.TempDir()
	file := filepath.Join(dir, "state.txt")
	os.WriteFile(file, []byte("v1"), 0644)

	cmd := []interface{}{"cat", file}
	if _, err := bt.Execute(context.TODO(), bgArgs("action", "poll", "name", "watcher", "cmd", cmd, "interval", "1s")); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// First run establishes baseline (no notification for "change" mode)
	// Second run sees identical output (still no notification)
	time.Sleep(2500 * time.Millisecond)
	if n := sink.count(); n != 0 {
		t.Fatalf("change mode must not notify on identical output, got %d msgs", n)
	}

	// Change the file → notification
	os.WriteFile(file, []byte("v2-NEWVALUE"), 0644)
	msg := sink.waitForMsg(t, "v2-NEWVALUE", 10*time.Second)
	if !strings.Contains(msg.Content, "[Background poll change]") {
		t.Errorf("expected change event: %s", msg.Content)
	}

	// Cancel
	if _, err := bt.Execute(context.TODO(), bgArgs("action", "cancel", "name", "watcher")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

func TestBackgroundPollFailureAndRecovery(t *testing.T) {
	bt, _, sink := newBGTestEnv(t)
	bt.SetContext("telegram", "9")

	dir := t.TempDir()
	file := filepath.Join(dir, "svc.pid")

	// Command "fails" while file missing, succeeds with it present
	cmd := []interface{}{"sh", "-c", "test -f " + file + " && echo up"}
	if _, err := bt.Execute(context.TODO(), bgArgs("action", "poll", "name", "svc", "cmd", cmd, "interval", "1s", "notifyOn", "failure")); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// First failure notifies
	sink.waitForMsg(t, "[Background poll failure]", 10*time.Second)

	// Continued failures do NOT re-notify
	time.Sleep(2200 * time.Millisecond)
	if n := sink.count(); n != 0 {
		t.Fatalf("repeated failures should not re-notify, got %d", n)
	}

	// Recovery notifies once
	os.WriteFile(file, []byte("1"), 0644)
	sink.waitForMsg(t, "[Background poll recovery]", 10*time.Second)

	bt.Execute(context.TODO(), bgArgs("action", "cancel", "name", "svc"))
}

func TestBackgroundPollMaxRuns(t *testing.T) {
	bt, _, sink := newBGTestEnv(t)
	bt.SetContext("telegram", "9")

	if _, err := bt.Execute(context.TODO(), bgArgs("action", "poll", "name", "thrice", "cmd", []interface{}{"echo", "tick"}, "interval", "1s", "notifyOn", "always", "maxRuns", 3)); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// 3 updates + 1 completion notice
	for i := 0; i < 3; i++ {
		sink.waitForMsg(t, "[Background poll update]", 10*time.Second)
	}
	sink.waitForMsg(t, "completed its 3 scheduled runs", 10*time.Second)

	list, _ := bt.Execute(context.TODO(), bgArgs("action", "list"))
	if strings.Contains(list, "thrice") {
		t.Errorf("poller should be auto-removed: %s", list)
	}
}

func TestBackgroundPollMinIntervalEnforced(t *testing.T) {
	bt, _, _ := newBGTestEnv(t)
	bt.SetContext("telegram", "1")
	// minInterval is 1s in tests; 100ms must be rejected
	_, err := bt.Execute(context.TODO(), bgArgs("action", "poll", "name", "fast", "cmd", []interface{}{"echo", "x"}, "interval", "100ms"))
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("expected min-interval error, got: %v", err)
	}
}

func TestBackgroundMaxPollersLimit(t *testing.T) {
	bt, _, _ := newBGTestEnv(t)
	bt.SetContext("telegram", "1")
	for i := 0; i < bgMaxPollers; i++ {
		if _, err := bt.Execute(context.TODO(), bgArgs("action", "poll", "name", "p", "cmd", []interface{}{"echo", "x"}, "interval", "1s")); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	_, err := bt.Execute(context.TODO(), bgArgs("action", "poll", "name", "extra", "cmd", []interface{}{"echo", "x"}, "interval", "1s"))
	if err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("expected limit error, got: %v", err)
	}
}

// ── Sandbox validation ──────────────────────────────────────────────────────

func TestBackgroundSandboxRejectsBlockedCommands(t *testing.T) {
	// strict-mode exec tool
	dir := t.TempDir()
	execTool := NewExecToolWithSandbox(30, dir, nil, config.SandboxConfig{Mode: "strict"})
	hub := chat.NewHub(64)
	sink := &bgSink{}
	sink.drain(hub.In)
	bt := NewBackgroundTool(hub, execTool)
	bt.minInterval = time.Second
	bt.SetContext("telegram", "1")

	// Blocked program
	_, err := bt.Execute(context.TODO(), bgArgs("action", "start", "name", "bad", "cmd", []interface{}{"rm", "-rf", "/tmp"}))
	if err == nil || !strings.Contains(err.Error(), "disallowed") {
		t.Errorf("expected rm rejection, got: %v", err)
	}
	// Absolute path arg in strict mode
	_, err = bt.Execute(context.TODO(), bgArgs("action", "start", "name", "bad2", "cmd", []interface{}{"cat", "/etc/passwd"}))
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Errorf("expected unsafe-arg rejection, got: %v", err)
	}
	// cwd outside allowed dirs
	_, err = bt.Execute(context.TODO(), bgArgs("action", "start", "name", "bad3", "cmd", []interface{}{"echo", "x"}, "cwd", "/etc"))
	if err == nil || !strings.Contains(err.Error(), "outside the allowed") {
		t.Errorf("expected cwd rejection, got: %v", err)
	}
	// Poll path validates too
	_, err = bt.Execute(context.TODO(), bgArgs("action", "poll", "name", "bad4", "cmd", []interface{}{"rm", "-rf", "/tmp"}, "interval", "1s"))
	if err == nil || !strings.Contains(err.Error(), "disallowed") {
		t.Errorf("expected poll rm rejection, got: %v", err)
	}
}

// ── Persistence ─────────────────────────────────────────────────────────────

func TestBackgroundPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bg.json")

	// Session 1: register a poller + a rerun job, then "restart"
	hub1 := chat.NewHub(64)
	sink1 := &bgSink{}
	sink1.drain(hub1.In)
	execTool1 := NewExecToolWithSandbox(30, t.TempDir(), nil, bgYoloSandbox())
	bt1 := NewBackgroundTool(hub1, execTool1)
	bt1.minInterval = time.Second
	bt1.SetContext("telegram", "42")
	if err := bt1.SetPersistencePath(path); err != nil {
		t.Fatalf("persistence setup: %v", err)
	}

	if _, err := bt1.Execute(context.TODO(), bgArgs("action", "poll", "name", "watcher", "cmd", []interface{}{"echo", "z"}, "interval", "2s")); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if _, err := bt1.Execute(context.TODO(), bgArgs("action", "start", "name", "rerunme", "rerunOnRestart", true, "cmd", []interface{}{"echo", "rerun-done"})); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := bt1.Execute(context.TODO(), bgArgs("action", "start", "name", "interrupted", "cmd", []interface{}{"sleep", "500"})); err != nil {
		t.Fatalf("start: %v", err)
	}
	bt1.Shutdown()

	// State should have been persisted (poller + both one-shots at shutdown time)
	b, err := os.ReadFile(path)
	if err == nil {
		var state bgPersistedState
		if err := json.Unmarshal(b, &state); err == nil {
			if len(state.Pollers) != 1 || state.Pollers[0].Name != "watcher" {
				t.Errorf("poller not persisted correctly: %+v", state.Pollers)
			}
		}
	}

	// Session 2: restore
	hub2 := chat.NewHub(64)
	sink2 := &bgSink{}
	sink2.drain(hub2.In)
	execTool2 := NewExecToolWithSandbox(30, t.TempDir(), nil, bgYoloSandbox())
	bt2 := NewBackgroundTool(hub2, execTool2)
	bt2.minInterval = time.Second
	if err := bt2.SetPersistencePath(path); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// rerun job relaunched and reports
	sink2.waitForMsg(t, "rerun-done", 10*time.Second)
	// interrupted job reported
	sink2.waitForMsg(t, "[Background job interrupted]", 10*time.Second)

	// poller resumed: verify via list
	list, _ := bt2.Execute(context.TODO(), bgArgs("action", "list"))
	if !strings.Contains(list, "watcher") {
		t.Errorf("poller not restored: %s", list)
	}
	bt2.Shutdown()
}

func TestBackgroundRestoreRevalidatesSandbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bg.json")

	// Write state as if from a yolo session
	state := bgPersistedState{
		NextID: 5,
		Pollers: []*bgPoller{{
			ID: "bg4", Name: "stale", Argv: []string{"rm", "-rf", "/tmp"},
			Interval: time.Minute, NotifyOn: "change", Channel: "telegram", ChatID: "1",
		}},
	}
	b, _ := json.Marshal(state)
	os.WriteFile(path, b, 0600)

	// Restore into a STRICT sandbox tool — the stale command must be skipped
	hub := chat.NewHub(64)
	sink := &bgSink{}
	sink.drain(hub.In)
	execTool := NewExecToolWithSandbox(30, t.TempDir(), nil, config.SandboxConfig{Mode: "strict"})
	bt := NewBackgroundTool(hub, execTool)
	bt.minInterval = time.Second
	if err := bt.SetPersistencePath(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	list, _ := bt.Execute(context.TODO(), bgArgs("action", "list"))
	if strings.Contains(list, "stale") {
		t.Errorf("stale poller must not survive strict sandbox: %s", list)
	}
	if bt.nextID < 5 {
		t.Errorf("nextID must be carried across restarts: %d", bt.nextID)
	}
}

// ── Misc ────────────────────────────────────────────────────────────────────

func TestBackgroundListAndUnknownAction(t *testing.T) {
	bt, _, _ := newBGTestEnv(t)
	out, err := bt.Execute(context.TODO(), bgArgs("action", "list"))
	if err != nil || out != "No background jobs." {
		t.Fatalf("empty list wrong: %q %v", out, err)
	}
	if _, err := bt.Execute(context.TODO(), bgArgs("action", "explode")); err == nil {
		t.Fatal("unknown action must error")
	}
	if _, err := bt.Execute(context.TODO(), bgArgs("action", "cancel", "name", "ghost")); err == nil {
		t.Fatal("cancelling a ghost must error")
	}
}

func TestBackgroundTailWriterKeepsLastBytes(t *testing.T) {
	var w bgTailWriter
	chunk := strings.Repeat("a", 3000)
	w.Write([]byte(chunk))          // 3000
	w.Write([]byte("BBBBBBBBBBBB")) // 3012 → tail holds last 4096 = all
	w.Write([]byte(strings.Repeat("c", 5000)))
	s := w.String()
	if w.total != 3000+12+5000 {
		t.Errorf("total = %d", w.total)
	}
	if len(s) != bgTailBytes {
		t.Errorf("tail len = %d, want %d", len(s), bgTailBytes)
	}
	if !strings.HasSuffix(s, strings.Repeat("c", 4096-3012+4096-4096)) && !strings.Contains(s, "cccc") {
		t.Errorf("tail should end with recent bytes")
	}
	// The 12 B's fell off the 4096 window
	if strings.Contains(w.String(), "BBBB") {
		t.Errorf("old bytes should be evicted")
	}
}
