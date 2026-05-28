package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

	// With cwd set to subdir, pwd should return that dir
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

	// With cwd set, should be able to read relative file
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
	outside := t.TempDir() // different temp dir, not in allowedDirs

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
	// No cwd specified — should default to allowedDir (d)
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
