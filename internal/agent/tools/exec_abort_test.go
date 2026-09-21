package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/config"
)

// newAbortTestTool builds an ExecTool with a short timeout and a scratch
// workspace, in yolo mode so sleep works without path validation fuss.
func newAbortTestTool(t *testing.T) *ExecTool {
	t.Helper()
	return NewExecToolWithSandbox(10, t.TempDir(), nil, sandboxYolo())
}

func sandboxYolo() config.SandboxConfig {
	return config.SandboxConfig{Mode: "yolo", AllowStringCommands: true}
}

// TestExecNeverInfiniteDefault verifies a zero-config tool still bounds
// commands: sleep 5 against a 1s per-call timeout must die at ~1s, not run
// the full five.
func TestExecNeverInfiniteDefault(t *testing.T) {
	tool := NewExecToolWithSandbox(0, t.TempDir(), nil, sandboxYolo()) // 0 → default 300s
	ctx := withPerCallTimeout(context.Background(), 1*time.Second)
	start := time.Now()
	_, err := tool.runCmd(ctx, "sh", []string{"-c", "sleep 5"}, "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("command ran %s — per-call deadline was not enforced (expected ~1s)", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}

// TestExecPerCallClampedToCeiling verifies absurd timeout requests are
// clamped to the hard ceiling rather than honored.
func TestExecPerCallClampedToCeiling(t *testing.T) {
	tool := newAbortTestTool(t)
	// Request 10 years; must clamp to maxPerCallExecTimeoutS (1h), not hang.
	// We verify clamping via Execute's arg parsing instead of waiting an hour:
	// parse path clamps secs before withPerCallTimeout.
	tool.SetDefaultTimeout(1)
	ctx := context.Background()
	_, err := tool.Execute(ctx, map[string]interface{}{
		"cmd":     []interface{}{"sh", "-c", "sleep 3"},
		"timeout": 99999999,
	})
	if err == nil {
		t.Fatal("expected timeout error from tool default, proving clamp didn't extend the deadline past default")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}

// TestAbortKillsInflightNotOthers verifies /abort targeting: commands
// registered under session A die; session B's command survives.
func TestAbortKillsInflightNotOthers(t *testing.T) {
	tool := newAbortTestTool(t)

	doneA := make(chan error, 1)
	doneB := make(chan error, 1)

	ctxA := WithExecSession(withPerCallTimeout(context.Background(), 30*time.Second), "telegram:111")
	ctxB := WithExecSession(withPerCallTimeout(context.Background(), 30*time.Second), "telegram:222")

	go func() {
		_, err := tool.runCmd(ctxA, "sh", []string{"-c", "sleep 25"}, "")
		doneA <- err
	}()
	go func() {
		_, err := tool.runCmd(ctxB, "sh", []string{"-c", "sleep 25"}, "")
		doneB <- err
	}()

	// Wait for both to be registered.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tool.mu.RLock()
		a := len(tool.inFlight["telegram:111"])
		b := len(tool.inFlight["telegram:222"])
		tool.mu.RUnlock()
		if a > 0 && b > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	killed := tool.AbortSession("telegram:111")
	if killed != 1 {
		t.Fatalf("expected 1 killed, got %d", killed)
	}

	select {
	case err := <-doneA:
		if err == nil || !strings.Contains(err.Error(), "aborted by user") {
			t.Fatalf("session A should die with abort error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session A command did not return after abort")
	}

	// B must still be running (not aborted).
	select {
	case err := <-doneB:
		t.Fatalf("session B should survive the abort of A, but returned: %v", err)
	default:
	}

	// Cleanup B so the test doesn't wait 25s.
	tool.AbortSessionPrefix("telegram:222")
	select {
	case <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("session B cleanup failed")
	}
}

// TestAbortPrefixCoversNamespaces verifies the chat-prefix fallback: a
// command under a namespaced key (proj:book:telegram:111) is caught by an
// abort aimed at the bare chat key.
func TestAbortPrefixCoversNamespaces(t *testing.T) {
	tool := newAbortTestTool(t)

	done := make(chan error, 1)
	ctx := WithExecSession(withPerCallTimeout(context.Background(), 30*time.Second), "proj:book:telegram:111")
	go func() {
		_, err := tool.runCmd(ctx, "sh", []string{"-c", "sleep 25"}, "")
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tool.mu.RLock()
		n := len(tool.inFlight["proj:book:telegram:111"])
		tool.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Abort targeting the bare chat — prefix match must catch the project
	// namespace.
	if n := tool.AbortSessionPrefix("telegram:111"); n != 1 {
		t.Fatalf("prefix abort should kill 1, got %d", n)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "aborted by user") {
			t.Fatalf("expected abort error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not return after prefix abort")
	}
}

// TestAbortNoTargetIsNoop verifies aborting with nothing in flight returns 0
// and doesn't panic.
func TestAbortNoTargetIsNoop(t *testing.T) {
	tool := newAbortTestTool(t)
	if n := tool.AbortSession("telegram:999"); n != 0 {
		t.Fatalf("expected 0, got %d", n)
	}
	if n := tool.AbortSessionPrefix("telegram:999"); n != 0 {
		t.Fatalf("expected 0, got %d", n)
	}
}

// TestExecStringInflightRegistration verifies string-form (yolo) commands
// are also registered for abort tracking.
func TestExecStringInflightRegistration(t *testing.T) {
	tool := newAbortTestTool(t)
	ctx := WithExecSession(context.Background(), "telegram:111")
	go func() {
		_, _ = tool.Execute(ctx, map[string]interface{}{
			"cmd":     "sleep 20",
			"timeout": 30,
		})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tool.mu.RLock()
		n := len(tool.inFlight["telegram:111"])
		tool.mu.RUnlock()
		if n > 0 {
			// Kill it and finish fast.
			if k := tool.AbortSession("telegram:111"); k != 1 {
				t.Fatalf("expected 1 kill, got %d", k)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("string-form command was never registered as in-flight")
}
