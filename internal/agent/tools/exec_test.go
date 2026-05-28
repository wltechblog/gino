package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/local/picobot/internal/config"
)

// =============================================================================
// Strict mode tests (default, backward compatible)
// =============================================================================

func TestExecArrayEcho(t *testing.T) {
	e := NewExecTool(2)
	out, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"echo", "hello"}})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if out != "hello" {
		t.Fatalf("unexpected out: %s", out)
	}
}

func TestExecStringDisallowed(t *testing.T) {
	e := NewExecTool(2)
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": "ls -la"})
	if err == nil {
		t.Fatalf("expected error for string command")
	}
}

func TestExecDangerousProgRejected(t *testing.T) {
	e := NewExecTool(2)
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"rm", "-rf", "/"}})
	if err == nil {
		t.Fatalf("expected error for dangerous program")
	}
}

func TestExecWithWorkspace(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "file.txt")
	if err := os.WriteFile(f, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	e := NewExecToolWithWorkspace(2, d)
	out, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"cat", "file.txt"}})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if out != "content" {
		t.Fatalf("unexpected out: %s", out)
	}
}

func TestExecRejectsUnsafeArg(t *testing.T) {
	e := NewExecTool(2)
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"ls", "/etc"}})
	if err == nil {
		t.Fatalf("expected error for absolute path arg")
	}
}

func TestExecAllowedDir(t *testing.T) {
	tmp := t.TempDir()
	safe := filepath.Join(tmp, "safe")
	os.MkdirAll(filepath.Join(safe, "sub"), 0o755)

	e := NewExecToolWithAllowedDirs(2, "", []string{safe})
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"ls", safe}})
	if err != nil {
		t.Fatalf("expected allowed dir %q to pass, got %v", safe, err)
	}

	sub := filepath.Join(safe, "sub")
	_, err = e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"ls", sub}})
	if err != nil {
		t.Fatalf("expected subdir %q to pass, got %v", sub, err)
	}

	outside := filepath.Join(tmp, "outside")
	_, err = e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"ls", outside}})
	if err == nil {
		t.Fatalf("expected error for path outside allowed dirs")
	}
}

func TestExecTimeout(t *testing.T) {
	e := NewExecTool(1)
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"sleep", "2"}})
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}

func TestExecCdBuiltin(t *testing.T) {
	e := NewExecTool(2)
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"cd", "somepath"}})
	if err == nil {
		t.Fatal("expected error for cd builtin")
	}
	if !strings.Contains(err.Error(), "cwd parameter") {
		t.Fatalf("expected cwd hint, got: %v", err)
	}
}

func TestExecBuiltinWrappedInShell(t *testing.T) {
	e := NewExecTool(2)
	out, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"export", "FOO=bar"}})
	if err != nil {
		t.Fatalf("expected export to succeed via sh -c, got: %v", err)
	}
	_ = out
}

func TestExecEchoNotBuiltin(t *testing.T) {
	e := NewExecTool(2)
	out, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"echo", "hello"}})
	if err != nil {
		t.Fatalf("echo should work as binary, got: %v", err)
	}
	if out != "hello" {
		t.Fatalf("expected 'hello', got %q", out)
	}
}

func TestExecCwdParameter(t *testing.T) {
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "subdir")
	os.MkdirAll(sub, 0o755)
	f := filepath.Join(sub, "hello.txt")
	os.WriteFile(f, []byte("from subdir"), 0644)

	e := NewExecToolWithAllowedDirs(2, tmp, []string{tmp})

	out, err := e.Execute(context.Background(), map[string]interface{}{
		"cmd": []interface{}{"pwd"},
		"cwd": sub,
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if out != sub {
		t.Fatalf("expected cwd %q, got %q", sub, out)
	}

	out, err = e.Execute(context.Background(), map[string]interface{}{
		"cmd": []interface{}{"cat", "hello.txt"},
		"cwd": sub,
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if out != "from subdir" {
		t.Fatalf("expected 'from subdir', got %q", out)
	}
}

func TestExecCwdNotAllowed(t *testing.T) {
	tmp := t.TempDir()
	outside := t.TempDir()

	e := NewExecToolWithAllowedDirs(2, tmp, []string{tmp})
	_, err := e.Execute(context.Background(), map[string]interface{}{
		"cmd": []interface{}{"ls"},
		"cwd": outside,
	})
	if err == nil {
		t.Fatal("expected error for cwd outside allowed dirs")
	}
	if !strings.Contains(err.Error(), "not within an allowed directory") {
		t.Fatalf("expected allowed dir error, got: %v", err)
	}
}

func TestExecCwdDefaultsToAllowedDir(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "test.txt")
	os.WriteFile(f, []byte("default dir"), 0644)

	e := NewExecToolWithWorkspace(2, d)
	out, err := e.Execute(context.Background(), map[string]interface{}{
		"cmd": []interface{}{"cat", "test.txt"},
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if out != "default dir" {
		t.Fatalf("expected 'default dir', got %q", out)
	}
}

// =============================================================================
// Strict mode — whitelist test
// =============================================================================

func TestExecStrictWhitelist(t *testing.T) {
	sandbox := config.SandboxConfig{
		Mode:            "strict",
		AllowedCommands: []string{"echo", "ls"},
	}
	e := NewExecToolWithSandbox(2, "", nil, sandbox)

	// echo is in whitelist
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"echo", "ok"}})
	if err != nil {
		t.Fatalf("expected echo to be allowed, got: %v", err)
	}

	// cat is NOT in whitelist
	_, err = e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"cat", "file"}})
	if err == nil {
		t.Fatal("expected cat to be rejected by whitelist")
	}
	if !strings.Contains(err.Error(), "not in the allowed list") {
		t.Fatalf("expected whitelist error, got: %v", err)
	}
}

// =============================================================================
// Strict mode — custom blocked commands
// =============================================================================

func TestExecStrictCustomBlocked(t *testing.T) {
	sandbox := config.SandboxConfig{
		Mode:            "strict",
		BlockedCommands: []string{"curl", "wget"},
	}
	e := NewExecToolWithSandbox(2, "", nil, sandbox)

	// curl is in custom blocked list — test via isBlocked, don't execute
	if !e.isBlocked("curl") {
		t.Fatal("expected curl to be blocked")
	}

	// wget is in custom blocked list
	if !e.isBlocked("wget") {
		t.Fatal("expected wget to be blocked")
	}

	// echo is fine
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"echo", "ok"}})
	if err != nil {
		t.Fatalf("expected echo to work, got: %v", err)
	}
}

// =============================================================================
// Strict mode — sudo blocked
// =============================================================================

func TestExecStrictBlocksSudo(t *testing.T) {
	e := NewExecTool(2) // default strict mode
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"sudo", "ls"}})
	if err == nil {
		t.Fatal("expected sudo to be blocked in strict mode")
	}
	if !strings.Contains(err.Error(), "disallowed") {
		t.Fatalf("expected disallowed error, got: %v", err)
	}
}

// =============================================================================
// Permissive mode tests
// =============================================================================

func TestExecPermissiveAllowsAbsPaths(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "permissive"}
	tmp := t.TempDir()
	safe := filepath.Join(tmp, "safe")
	os.MkdirAll(safe, 0o755)

	e := NewExecToolWithSandbox(2, "", []string{safe}, sandbox)

	// Absolute path within allowed dirs should work
	out, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"ls", safe}})
	if err != nil {
		t.Fatalf("expected permissive to allow absolute path, got: %v", err)
	}
	_ = out
}

func TestExecPermissiveBlocksDangerous(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "permissive"}
	e := NewExecToolWithSandbox(2, "", nil, sandbox)

	// rm should NOT be blocked in permissive — test via isBlocked
	if e.isBlocked("rm") {
		t.Fatal("permissive should not block rm")
	}

	// dd should still be blocked
	if !e.isBlocked("dd") {
		t.Fatal("expected dd to be blocked in permissive mode")
	}
}

func TestExecPermissiveBlocksSudo(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "permissive"}
	e := NewExecToolWithSandbox(2, "", nil, sandbox)
	// Test via isBlocked — don't actually execute sudo
	if !e.isBlocked("sudo") {
		t.Fatal("expected sudo to be blocked in permissive mode")
	}
}

func TestExecPermissiveRejectsStringCommands(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "permissive"}
	e := NewExecToolWithSandbox(2, "", nil, sandbox)
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": "ls -la"})
	if err == nil {
		t.Fatal("expected string commands to be rejected in permissive mode")
	}
}

func TestExecPermissiveAbsPathOutsideAllowed(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "permissive"}
	tmp := t.TempDir()
	safe := filepath.Join(tmp, "safe")
	os.MkdirAll(safe, 0o755)

	e := NewExecToolWithSandbox(2, "", []string{safe}, sandbox)

	// Absolute path outside allowed dirs should still be rejected
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"ls", "/etc/passwd"}})
	if err == nil {
		t.Fatal("expected absolute path outside allowed dirs to be rejected")
	}
}

// =============================================================================
// YOLO mode tests
// =============================================================================

func TestExecYoloAllowsStringCommands(t *testing.T) {
	sandbox := config.SandboxConfig{
		Mode:               "yolo",
		AllowStringCommands: true,
	}
	e := NewExecToolWithSandbox(2, "", nil, sandbox)
	out, err := e.Execute(context.Background(), map[string]interface{}{"cmd": "echo hello world"})
	if err != nil {
		t.Fatalf("expected yolo to allow string commands, got: %v", err)
	}
	if out != "hello world" {
		t.Fatalf("expected 'hello world', got %q", out)
	}
}

func TestExecYoloStringDisabledByDefault(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "yolo"} // AllowStringCommands defaults to false
	e := NewExecToolWithSandbox(2, "", nil, sandbox)
	_, err := e.Execute(context.Background(), map[string]interface{}{"cmd": "echo hello"})
	if err == nil {
		t.Fatal("expected string commands to be rejected when AllowStringCommands is false")
	}
}

func TestExecYoloAllowsRm(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "yolo"}
	e := NewExecToolWithSandbox(2, "", nil, sandbox)
	// Test via isBlocked — rm should not be blocked in yolo
	if e.isBlocked("rm") {
		t.Fatal("yolo mode should not block rm")
	}
}

func TestExecYoloAllowsAbsolutePaths(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "yolo"}
	e := NewExecToolWithSandbox(2, "", nil, sandbox)
	// Absolute path normally rejected but yolo allows it
	out, err := e.Execute(context.Background(), map[string]interface{}{"cmd": []interface{}{"ls", "/tmp"}})
	if err != nil {
		t.Fatalf("expected yolo to allow absolute paths, got: %v", err)
	}
	_ = out
}

func TestExecYoloCwdAnywhere(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "yolo"}
	e := NewExecToolWithSandbox(2, "", nil, sandbox)
	out, err := e.Execute(context.Background(), map[string]interface{}{
		"cmd": []interface{}{"pwd"},
		"cwd": "/tmp",
	})
	if err != nil {
		t.Fatalf("expected yolo to allow any cwd, get: %v", err)
	}
	if out != "/tmp" {
		t.Fatalf("expected /tmp, got %q", out)
	}
}

func TestExecYoloDoesNotBlockSudo(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "yolo"}
	e := NewExecToolWithSandbox(2, "", nil, sandbox)
	// Test via isBlocked — don't actually execute sudo
	if e.isBlocked("sudo") {
		t.Fatal("yolo mode should not block sudo")
	}
}

func TestExecYoloAllowsPipes(t *testing.T) {
	sandbox := config.SandboxConfig{
		Mode:               "yolo",
		AllowStringCommands: true,
	}
	e := NewExecToolWithSandbox(2, "", nil, sandbox)
	out, err := e.Execute(context.Background(), map[string]interface{}{"cmd": "echo hello | tr a-z A-Z"})
	if err != nil {
		t.Fatalf("expected yolo to allow pipes, got: %v", err)
	}
	if out != "HELLO" {
		t.Fatalf("expected 'HELLO', got %q", out)
	}
}

// =============================================================================
// SandboxConfig helper tests
// =============================================================================

func TestSandboxConfigDefaults(t *testing.T) {
	s := config.SandboxConfig{}
	if s.GetMode() != "strict" {
		t.Fatalf("expected default mode 'strict', got %q", s.GetMode())
	}
	if s.IsYolo() {
		t.Fatal("expected IsYolo to be false")
	}
	if s.IsPermissive() {
		t.Fatal("expected IsPermissive to be false")
	}
	if s.AllowsAbsolutePaths() {
		t.Fatal("expected strict to disallow absolute paths by default")
	}
	if s.AllowsStringCommands() {
		t.Fatal("expected strict to disallow string commands")
	}
}

func TestSandboxConfigPermissiveDefaults(t *testing.T) {
	s := config.SandboxConfig{Mode: "permissive"}
	if !s.AllowsAbsolutePaths() {
		t.Fatal("expected permissive to allow absolute paths by default")
	}
	if s.AllowsStringCommands() {
		t.Fatal("expected permissive to disallow string commands")
	}
}

func TestSandboxConfigYoloDefaults(t *testing.T) {
	s := config.SandboxConfig{
		Mode:               "yolo",
		AllowStringCommands: true,
	}
	if !s.IsYolo() {
		t.Fatal("expected IsYolo")
	}
	if !s.AllowsAbsolutePaths() {
		t.Fatal("expected yolo to allow absolute paths")
	}
	if !s.AllowsStringCommands() {
		t.Fatal("expected yolo to allow string commands")
	}
}
