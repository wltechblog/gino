package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/agent/tools"
	"github.com/wltechblog/gino/internal/config"
)

func TestProjectRegistryPersistence(t *testing.T) {
	dir := t.TempDir()
	reg, err := LoadProjectRegistry(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	ws := t.TempDir()
	if _, err := reg.Add("alpha", ws); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := reg.SetActive("alpha"); err != nil {
		t.Fatalf("set active: %v", err)
	}

	// Reload from disk — registry must round-trip projects + active.
	reg2, err := LoadProjectRegistry(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reg2.ActiveProject(); got != "alpha" {
		t.Fatalf("active = %q, want alpha", got)
	}
	if _, ok := reg2.Get("alpha"); !ok {
		t.Fatal("project alpha missing after reload")
	}

	// Duplicate names rejected.
	if _, err := reg2.Add("alpha", ws); err == nil {
		t.Fatal("duplicate add should fail")
	}
	// Invalid names rejected.
	for _, bad := range []string{"", ".hidden", "has space", "a/b", "x;y"} {
		if _, err := reg2.Add(bad, ws); err == nil {
			t.Fatalf("invalid name %q should fail", bad)
		}
	}
	// Nonexistent path rejected.
	if _, err := reg2.Add("ghost", filepath.Join(dir, "nope")); err == nil {
		t.Fatal("nonexistent path should fail")
	}

	// Removing the active project clears the active selection.
	if err := reg2.Remove("alpha"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := reg2.ActiveProject(); got != "" {
		t.Fatalf("active after remove = %q, want empty", got)
	}
}

func TestProjectRegistryCorruptFileRecovers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "projects.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadProjectRegistry(dir)
	if err != nil {
		t.Fatalf("corrupt registry should recover, got: %v", err)
	}
	if reg.ActiveProject() != "" || len(reg.List()) != 0 {
		t.Fatal("corrupt registry should load empty")
	}
}

func TestFilesystemToolSetWorkspace(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	// Marker file in each workspace.
	if err := os.WriteFile(filepath.Join(wsA, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsB, "b.txt"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft, err := tools.NewFilesystemTool(wsA, []string{wsA}, config.SandboxConfig{Mode: "yolo"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer ft.Close()

	// Relative path resolves against wsA.
	res, err := ft.Execute(context.Background(), map[string]interface{}{"action": "read", "path": "a.txt"})
	if err != nil || res == "" {
		t.Fatalf("read a.txt before switch: %v %q", err, res)
	}

	// Switch primary to wsB.
	if err := ft.SetWorkspace(wsB); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if got := ft.WorkspaceDir(); got != wsB {
		t.Fatalf("workspace dir = %q, want %q", got, wsB)
	}
	// Relative path now resolves against wsB.
	res, err = ft.Execute(context.Background(), map[string]interface{}{"action": "read", "path": "b.txt"})
	if err != nil || res == "" {
		t.Fatalf("read b.txt after switch: %v %q", err, res)
	}
	// The old workspace remains accessible by absolute path.
	if _, err := ft.Execute(context.Background(), map[string]interface{}{"action": "read", "path": filepath.Join(wsA, "a.txt")}); err != nil {
		t.Fatalf("old workspace should remain accessible: %v", err)
	}

	// Invalid workspace rejected, state unchanged.
	if err := ft.SetWorkspace(filepath.Join(wsA, "missing")); err == nil {
		t.Fatal("switch to missing dir should fail")
	}
	if got := ft.WorkspaceDir(); got != wsB {
		t.Fatalf("workspace after failed switch = %q, want %q", got, wsB)
	}

	// Switch back — no duplicate roots, still functional.
	if err := ft.SetWorkspace(wsA); err != nil {
		t.Fatalf("switch back: %v", err)
	}
	res, err = ft.Execute(context.Background(), map[string]interface{}{"action": "read", "path": "a.txt"})
	if err != nil || res == "" {
		t.Fatalf("read a.txt after switch back: %v %q", err, res)
	}
}

func TestBuildProjectKeyboard(t *testing.T) {
	projects := []*Project{{Name: "alpha", Path: "/tmp/a"}, {Name: "beta", Path: "/tmp/b"}}
	kb := buildProjectKeyboard(projects, "beta")
	// Must contain both callbacks plus the profile-workspace option.
	for _, want := range []string{`"prj:none"`, `"prj:alpha"`, `"prj:beta"`, "✅ beta"} {
		if !strings.Contains(kb, want) {
			t.Fatalf("keyboard %q missing %q", kb, want)
		}
	}
}
