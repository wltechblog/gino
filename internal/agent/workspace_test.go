package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveProjectWorkspaceEmptyKeepsProfile(t *testing.T) {
	profile := t.TempDir()
	got, err := ResolveProjectWorkspace("", profile)
	if err != nil {
		t.Fatal(err)
	}
	if got != profile {
		t.Fatalf("got %q, want profile %q", got, profile)
	}
}

func TestResolveProjectWorkspaceRequiresDirectory(t *testing.T) {
	profile := t.TempDir()
	missing := filepath.Join(profile, "missing")
	if _, err := ResolveProjectWorkspace(missing, profile); err == nil {
		t.Fatal("expected error for missing project")
	}

	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveProjectWorkspace(file, profile); err == nil {
		t.Fatal("expected error for non-directory project")
	}
}

func TestResolveProjectWorkspaceAcceptsExistingDir(t *testing.T) {
	profile := t.TempDir()
	project := t.TempDir()
	got, err := ResolveProjectWorkspace(project, profile)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(project)
	if err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if got != abs {
		t.Fatalf("got %q, want %q", got, abs)
	}
}

func TestResolveProjectWorkspaceFollowsSymlink(t *testing.T) {
	profile := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveProjectWorkspace(link, profile)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q, want symlink target %q", got, want)
	}
}
