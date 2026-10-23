package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/agent/memory"
	"github.com/wltechblog/gino/internal/config"
)

// newTestPaths returns three sibling XDG-style roots under one temp dir.
func newTestPaths(t *testing.T) config.Paths {
	t.Helper()
	root := t.TempDir()
	return config.Paths{
		ConfigDir: filepath.Join(root, "config"),
		DataDir:   filepath.Join(root, "data"),
		StateDir:  filepath.Join(root, "state"),
	}
}

func TestMemoryCLI_ReadAppendWriteRecent(t *testing.T) {
	paths := newTestPaths(t)
	if _, _, err := config.Onboard(paths); err != nil {
		t.Fatalf("onboard failed: %v", err)
	}

	// Test append today
	var buf bytes.Buffer
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	runMemoryAppend(paths, []string{"today", "-c", "hello"})

	w.Close()
	os.Stdout = oldStdout
	_, _ = buf.ReadFrom(r)
	output := buf.String()
	if !strings.Contains(output, "appended to today") {
		t.Fatalf("expected 'appended to today' in output, got %q", output)
	}

	// Verify memory files exist
	cfg, _ := config.LoadConfig(paths)
	ws := cfg.Agents.Defaults.Workspace
	if strings.HasPrefix(ws, "~") {
		home, _ := os.UserHomeDir()
		ws = filepath.Join(home, ws[2:])
	}
	memFile := filepath.Join(ws, "memory")
	files, _ := os.ReadDir(memFile)
	found := false
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".md") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected memory files, none found in %s", memFile)
	}

	// Test write long
	buf.Reset()
	r, w, _ = os.Pipe()
	os.Stdout = w
	runMemoryWrite(paths, []string{"long", "-c", "LONGCONTENT"})
	w.Close()
	os.Stdout = oldStdout
	_, _ = buf.ReadFrom(r)

	// Test read long
	buf.Reset()
	r, w, _ = os.Pipe()
	os.Stdout = w
	runMemoryRead(paths, []string{"long"})
	w.Close()
	os.Stdout = oldStdout
	_, _ = buf.ReadFrom(r)
	out := buf.String()
	if !strings.Contains(out, "LONGCONTENT") {
		t.Fatalf("expected LONGCONTENT in output, got %q", out)
	}

	// Test recent
	buf.Reset()
	r, w, _ = os.Pipe()
	os.Stdout = w
	runMemoryRecent(paths, []string{"-d", "1"})
	w.Close()
	os.Stdout = oldStdout
	_, _ = buf.ReadFrom(r)
	if buf.String() == "" {
		t.Fatalf("expected recent output, got empty")
	}
}

func TestMemoryCLI_Rank(t *testing.T) {
	paths := newTestPaths(t)
	if _, _, err := config.Onboard(paths); err != nil {
		t.Fatalf("onboard failed: %v", err)
	}

	cfg, _ := config.LoadConfig(paths)
	ws := cfg.Agents.Defaults.Workspace
	if strings.HasPrefix(ws, "~") {
		home, _ := os.UserHomeDir()
		ws = filepath.Join(home, ws[2:])
	}
	mem := memory.NewMemoryStoreWithWorkspace(ws, 100)
	_ = mem.AppendToday("buy milk and eggs")
	_ = mem.AppendToday("call mom tomorrow")
	_ = mem.AppendToday("milkshake recipe")

	var buf bytes.Buffer
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	runMemoryRank(paths, []string{"-q", "milk", "-k", "2"})
	w.Close()
	os.Stdout = oldStdout
	_, _ = buf.ReadFrom(r)
	out := buf.String()
	if !strings.Contains(out, "buy milk") {
		t.Fatalf("expected 'buy milk' in output, got: %q", out)
	}
	if !strings.Contains(out, "milkshake") && !strings.Contains(out, "Important facts") {
		t.Fatalf("expected either 'milkshake' or 'Important facts' in output, got: %q", out)
	}
}

// TestAgentCLI_CreatesMissingWorkspace covers a config without a workspace
// directory (e.g. deleted after onboard): startup must recreate it instead of
// failing to chdir.
func TestAgentCLI_CreatesMissingWorkspace(t *testing.T) {
	paths := newTestPaths(t)
	_, ws, err := config.Onboard(paths)
	if err != nil {
		t.Fatalf("onboard failed: %v", err)
	}
	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}

	cfg, _ := config.LoadConfig(paths)
	cfg.Providers.OpenAI = nil
	if err := config.SaveConfig(cfg, paths.ConfigFile()); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	runAgent(paths, []string{"-M", "stub-model", "-m", "hello"})
	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if out := buf.String(); !strings.Contains(out, "(stub) Echo") {
		t.Fatalf("expected stub echo output, got: %q", out)
	}

	if cwd, err := os.Getwd(); err != nil || cwd != ws {
		t.Fatalf("expected cwd %q, got %q (err %v)", ws, cwd, err)
	}
	if info, err := os.Stat(ws); err != nil || !info.IsDir() {
		t.Fatalf("workspace was not recreated: %v", err)
	}
}

func TestAgentCLI_ModelFlag(t *testing.T) {
	paths := newTestPaths(t)
	if _, _, err := config.Onboard(paths); err != nil {
		t.Fatalf("onboard failed: %v", err)
	}

	cfg, _ := config.LoadConfig(paths)
	cfg.Providers.OpenAI = nil
	if err := config.SaveConfig(cfg, paths.ConfigFile()); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	runAgent(paths, []string{"-M", "stub-model", "-m", "hello"})
	w.Close()
	os.Stdout = oldStdout
	_, _ = buf.ReadFrom(r)
	out := buf.String()
	if !strings.Contains(out, "(stub) Echo") {
		t.Fatalf("expected stub echo output, got: %q", out)
	}
}

func TestRequireConfiguredProvider(t *testing.T) {
	paths := newTestPaths(t)
	cfg := config.DefaultConfig(paths.Workspace())
	if err := requireConfiguredProvider(cfg, paths); err == nil {
		t.Fatal("placeholder key must be rejected before any request")
	}
	cfg.Providers.OpenAI.APIKey = "sk-real"
	if err := requireConfiguredProvider(cfg, paths); err != nil {
		t.Fatalf("configured provider rejected: %v", err)
	}
}
