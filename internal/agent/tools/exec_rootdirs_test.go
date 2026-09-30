package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wltechblog/gino/internal/config"
)

// Regression: an operator granting allowedDirs ["/"] authorizes absolute
// paths EVERYWHERE. Strict mode must honor the grant (previously it
// rejected all absolute args before ever consulting allowedDirs), and the
// root grant must actually match (Clean("/")+"/" == "//" prefixes nothing).
func TestExecRootAllowedDirAuthorizesStrict(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "strict"}
	e := NewExecToolWithSandbox(2, "", []string{"/"}, sandbox)

	// Absolute-path ARG under a root grant: field-reported failure
	// (`tail -n 60 /home/joist/.joist/interns/.../gateway.log`).
	logPath := filepath.Join(t.TempDir(), "gateway.log")
	_ = os.WriteFile(logPath, []byte("line1\nline2\n"), 0o644)
	_, err := e.Execute(context.Background(), map[string]interface{}{
		"cmd": []interface{}{"tail", "-n", "2", logPath},
	})
	if err != nil {
		t.Fatalf("root allowedDirs grant must authorize absolute args in strict mode: %v", err)
	}

	// Absolute-path cwd under a root grant (the second field-reported
	// failure: cwd "not within an allowed directory").
	cwd := filepath.Dir(logPath)
	_, err = e.Execute(context.Background(), map[string]interface{}{
		"cmd": []interface{}{"ls"},
		"cwd": cwd,
	})
	if err != nil {
		t.Fatalf("root allowedDirs grant must authorize cwd in strict mode: %v", err)
	}
}

// Narrow grants keep working as subtree containment in strict mode:
// inside passes, outside stays blocked.
func TestExecStrictNarrowGrantSubtree(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "strict"}
	base := t.TempDir()
	inside := filepath.Join(base, "inside")
	_ = os.MkdirAll(inside, 0o755)
	e := NewExecToolWithSandbox(2, "", []string{inside}, sandbox)

	if _, err := e.Execute(context.Background(), map[string]interface{}{
		"cmd": []interface{}{"ls", inside},
	}); err != nil {
		t.Fatalf("granted subtree arg must pass in strict mode: %v", err)
	}
	outside := filepath.Join(base, "outside")
	if _, err := e.Execute(context.Background(), map[string]interface{}{
		"cmd": []interface{}{"ls", outside},
	}); err == nil {
		t.Fatal("absolute arg outside the grant must stay blocked in strict mode")
	}
	if _, err := e.Execute(context.Background(), map[string]interface{}{
		"cmd": []interface{}{"ls"},
		"cwd": outside,
	}); err == nil {
		t.Fatal("cwd outside the grant must stay blocked in strict mode")
	}
}

// The background tool's Validate() rides isArgUnsafe/isDirAllowed — it
// inherits the root-grant fix.
func TestValidateRootAllowedDir(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "strict"}
	e := NewExecToolWithSandbox(2, "", []string{"/"}, sandbox)
	logPath := filepath.Join(t.TempDir(), "gateway.log")
	_ = os.WriteFile(logPath, []byte("x\n"), 0o644)

	if err := e.Validate([]string{"tail", "-n", "5", logPath}, filepath.Dir(logPath)); err != nil {
		t.Fatalf("Validate must honor the root grant: %v", err)
	}

	// A NARROW grant must still contain: outside the granted subtree the
	// Validate call rejects, root grant or not.
	narrow := NewExecToolWithSandbox(2, "", []string{filepath.Dir(logPath)}, config.SandboxConfig{Mode: "strict"})
	if err := narrow.Validate([]string{"ls", "/definitely/not/granted"}, ""); err == nil {
		t.Fatal("un-granted absolute arg must still be rejected outside the granted subtree")
	}
}

// Permissive + root grant: cwd containment previously failed on the "//"
// prefix bug even in permissive; pin the fix.
func TestExecPermissiveRootGrantCwd(t *testing.T) {
	sandbox := config.SandboxConfig{Mode: "permissive"}
	e := NewExecToolWithSandbox(2, "", []string{"/"}, sandbox)
	cwd := t.TempDir()
	if _, err := e.Execute(context.Background(), map[string]interface{}{
		"cmd": []interface{}{"pwd"},
		"cwd": cwd,
	}); err != nil {
		t.Fatalf("permissive + root grant cwd must pass: %v", err)
	}
}
