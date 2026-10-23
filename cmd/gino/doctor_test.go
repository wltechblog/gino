package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/config"
)

func TestApplyDoctorFixesFreshInstall(t *testing.T) {
	paths := newTestPaths(t)

	fixes := applyDoctorFixes(paths)
	if len(fixes) == 0 {
		t.Fatal("expected fixes for a fresh install")
	}
	for _, f := range fixes {
		if !f.ok {
			t.Fatalf("%s failed: %s", f.name, f.detail)
		}
	}

	cfg, err := config.LoadConfig(paths)
	if err != nil {
		t.Fatalf("config does not load after fix: %v", err)
	}
	if cfg.Agents.Defaults.Workspace != paths.Workspace() {
		t.Fatalf("workspace = %q, want %q", cfg.Agents.Defaults.Workspace, paths.Workspace())
	}
	if !config.WorkspaceComplete(paths.Workspace()) {
		t.Fatal("workspace is still incomplete")
	}
	for _, dir := range []string{paths.ConfigDir, paths.DataDir, paths.StateDir} {
		if !statIsDir(dir) {
			t.Fatalf("%s was not created", dir)
		}
	}
}

func TestApplyDoctorFixesBacksUpCorruptConfig(t *testing.T) {
	paths := newTestPaths(t)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	backedUp := false
	for _, f := range applyDoctorFixes(paths) {
		if !f.ok {
			t.Fatalf("%s failed: %s", f.name, f.detail)
		}
		if f.name == "config" && strings.Contains(f.detail, "backed up corrupt config") {
			backedUp = true
		}
	}
	if !backedUp {
		t.Fatal("corrupt config was not reported as backed up")
	}
	if _, err := config.LoadConfig(paths); err != nil {
		t.Fatalf("config does not load after repair: %v", err)
	}
	matches, err := filepath.Glob(paths.ConfigFile() + ".corrupt.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one corrupt backup, got %v", matches)
	}
}

func TestApplyDoctorFixesPreservesUserFiles(t *testing.T) {
	paths := newTestPaths(t)
	if _, _, err := config.Onboard(paths); err != nil {
		t.Fatal(err)
	}
	soul := filepath.Join(paths.Workspace(), "SOUL.md")
	if err := os.WriteFile(soul, []byte("my custom soul"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(paths.Workspace(), "skills")); err != nil {
		t.Fatal(err)
	}

	for _, f := range applyDoctorFixes(paths) {
		if !f.ok {
			t.Fatalf("%s failed: %s", f.name, f.detail)
		}
	}

	got, err := os.ReadFile(soul)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "my custom soul" {
		t.Fatalf("user SOUL.md was overwritten: %q", got)
	}
	if !statIsDir(filepath.Join(paths.Workspace(), "skills")) {
		t.Fatal("skills directory was not restored")
	}
}

func TestApplyDoctorFixesNoOpWhenHealthy(t *testing.T) {
	paths := newTestPaths(t)
	if _, _, err := config.Onboard(paths); err != nil {
		t.Fatal(err)
	}

	if fixes := applyDoctorFixes(paths); len(fixes) != 0 {
		t.Fatalf("expected no fixes, got %+v", fixes)
	}
}

func TestCheckProviderRejectsPlaceholder(t *testing.T) {
	cfg := config.DefaultConfig("/ws")
	var checks []doctorCheck
	checkProvider(&checks, cfg)
	if len(checks) != 1 {
		t.Fatalf("expected one check, got %d", len(checks))
	}
	if checks[0].status != statusFail || !strings.Contains(checks[0].detail, "placeholder") {
		t.Fatalf("unexpected check: %+v", checks[0])
	}
}
