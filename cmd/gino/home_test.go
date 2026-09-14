package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveHomeDirFallsBackToUserDB covers the systemd case: $HOME unset.
// user.Current must still resolve via /etc/passwd so a root-run gateway
// finds /root/.gino instead of the relative ".gino" that broke chdir.
func TestResolveHomeDirFallsBackToUserDB(t *testing.T) {
	old := os.Getenv("HOME")
	os.Unsetenv("HOME")
	defer os.Setenv("HOME", old)

	dir := resolveHomeDir("")
	if !filepath.IsAbs(dir) {
		t.Fatalf("home dir must be absolute, got %q", dir)
	}
	if filepath.Base(dir) != ".gino" {
		t.Fatalf("expected base .gino, got %q", dir)
	}
}

// TestResolveHomeDirAbsoluteFlag verifies explicit -home survives unchanged.
func TestResolveHomeDirAbsoluteFlag(t *testing.T) {
	if got := resolveHomeDir("/opt/ginohome"); got != "/opt/ginohome" {
		t.Fatalf("expected /opt/ginohome, got %q", got)
	}
}

// TestRequireGatewayConfigMissing pins the hard-fail: a gateway without
// config.json must not silently run with defaults.
func TestRequireGatewayConfigMissing(t *testing.T) {
	dir := t.TempDir()
	if err := requireGatewayConfig(dir); err == nil {
		t.Fatal("expected error for missing config.json")
	}
	// exists -> nil
	f, err := os.Create(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := requireGatewayConfig(dir); err != nil {
		t.Fatalf("expected nil for existing config, got %v", err)
	}
}
