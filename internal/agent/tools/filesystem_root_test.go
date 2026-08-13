package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wltechblog/gino/internal/config"
)

func TestRootForDirMatchesConstructorNormalisation(t *testing.T) {
	ws := t.TempDir()
	extra := t.TempDir()

	ft, err := NewFilesystemTool(ws, []string{extra}, config.SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ft.Close() }()

	if ft.RootForDir(ws) == nil {
		t.Fatal("expected root for workspace")
	}
	if ft.RootForDir(extra) == nil {
		t.Fatal("expected root for allowed directory")
	}
	if ft.RootForDir(filepath.Join(ws, "..", filepath.Base(ws))) == nil {
		t.Fatal("logically equivalent cleaned path should match")
	}
	if ft.RootForDir(t.TempDir()) != nil {
		t.Fatal("unknown directory must not resolve to a root")
	}
}

func TestCanonicalDirCleansAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	got, err := canonicalDir(filepath.Join(dir, ".", "sub", ".."))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(want) {
		t.Fatalf("got %q, want %q", got, filepath.Clean(want))
	}

	if _, err := os.Stat(got); err != nil {
		t.Fatal(err)
	}
}
