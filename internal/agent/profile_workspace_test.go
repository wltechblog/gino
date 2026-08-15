package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

func writeSkill(t *testing.T, workspace, name, description string) {
	t.Helper()
	dir := filepath.Join(workspace, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProfileProjectWorkspaceSeparation(t *testing.T) {
	profile := t.TempDir()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "marker.txt"), []byte("from-project"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "SOUL.md"), []byte("keep this identity"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, profile, "weather", "Get weather info")
	writeSkill(t, project, "only-in-project", "Should not be listed")

	b := chat.NewHub(10)
	p := providers.NewStubProvider()
	ag := NewAgentLoopWithProfileWorkspace(b, p, p.GetDefaultModel(), 3, project, profile, nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	defer ag.Close()

	sys := ag.context.BuildMessages(nil, "hello", "cli", "1", "", "", nil, nil)[0].Content
	if !strings.Contains(sys, "keep this identity") {
		t.Fatalf("expected profile SOUL.md in context:\n%s", sys)
	}

	ctx := context.Background()
	listing, err := ag.tools.Execute(ctx, "filesystem", map[string]interface{}{"action": "list", "path": "."})
	if err != nil {
		t.Fatalf("list project: %v", err)
	}
	if !strings.Contains(listing, "marker.txt") {
		t.Fatalf("filesystem primary root should be the project, got %q", listing)
	}

	_, err = ag.tools.Execute(ctx, "filesystem", map[string]interface{}{
		"action": "read",
		"path":   filepath.Join(profile, "SOUL.md"),
	})
	if err == nil {
		t.Fatal("profile identity files should not be a generic filesystem root")
	}

	skillsOut, err := ag.tools.Execute(ctx, "list_skills", map[string]interface{}{})
	if err != nil {
		t.Fatalf("list_skills: %v", err)
	}
	if !strings.Contains(skillsOut, "weather") {
		t.Fatalf("skills should load from profile, got %q", skillsOut)
	}
	if strings.Contains(skillsOut, "only-in-project") {
		t.Fatalf("project skills must not replace profile skills, got %q", skillsOut)
	}

	if _, err := ag.tools.Execute(ctx, "write_memory", map[string]interface{}{
		"target":  "long",
		"content": "remember the profile",
		"append":  true,
	}); err != nil {
		t.Fatalf("write_memory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(profile, "memory")); err != nil {
		t.Fatalf("memory should persist under profile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "memory")); !os.IsNotExist(err) {
		t.Fatalf("memory should not be created in the project, err=%v", err)
	}

	sess := ag.sessions.GetOrCreate("cli:test")
	sess.AddMessage("user", "hello")
	sess.AddMessage("assistant", "hi")
	if err := ag.sessions.Save(sess); err != nil {
		t.Fatalf("save session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(profile, "sessions", "cli:test.json")); err != nil {
		t.Fatalf("sessions should persist under profile: %v", err)
	}
	if ag.checkpoints.workspace != profile {
		t.Fatalf("checkpoints workspace = %q, want profile %q", ag.checkpoints.workspace, profile)
	}
	if _, err := os.Stat(filepath.Join(project, "sessions")); !os.IsNotExist(err) {
		t.Fatalf("sessions should not be created in the project, err=%v", err)
	}
}

func TestNewAgentLoopKeepsSingleWorkspace(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "SOUL.md"), []byte("single soul"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, ws, "weather", "Get weather info")

	b := chat.NewHub(10)
	p := providers.NewStubProvider()
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 3, ws, nil, nil, nil, nil, nil, "", config.SandboxConfig{}, "", 0, 0, nil, config.WebConfig{}, config.SearchConfig{}, "")
	defer ag.Close()

	sys := ag.context.BuildMessages(nil, "hello", "cli", "1", "", "", nil, nil)[0].Content
	if strings.Contains(sys, "Profile SOUL.md") {
		t.Fatal("NewAgentLoop should keep original bootstrap headings")
	}

	listing, err := ag.tools.Execute(context.Background(), "filesystem", map[string]interface{}{"action": "list", "path": "."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing, "SOUL.md") {
		t.Fatalf("single-workspace mode should allow listing profile files, got %q", listing)
	}
}
