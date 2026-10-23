package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wltechblog/gino/internal/config"
)

func TestParseGlobalFlags(t *testing.T) {
	overrides, rest := parseGlobalFlags([]string{"-home", "/opt/gino", "-m", "hello", "-data-dir=/srv/data"})
	if overrides.Home != "/opt/gino" {
		t.Fatalf("home = %q, want /opt/gino", overrides.Home)
	}
	if overrides.DataDir != "/srv/data" {
		t.Fatalf("data dir = %q, want /srv/data", overrides.DataDir)
	}
	if !slices.Equal(rest, []string{"-m", "hello"}) {
		t.Fatalf("rest = %v, want [-m hello]", rest)
	}
}

func TestParseGlobalFlagsAllDirs(t *testing.T) {
	overrides, rest := parseGlobalFlags([]string{"-config-dir", "/c", "-data-dir", "/d", "-state-dir", "/s"})
	if overrides.ConfigDir != "/c" || overrides.DataDir != "/d" || overrides.StateDir != "/s" {
		t.Fatalf("unexpected overrides: %+v", overrides)
	}
	if len(rest) != 0 {
		t.Fatalf("rest = %v, want empty", rest)
	}
}

func TestRequireGatewayConfigMissing(t *testing.T) {
	dir := t.TempDir()
	paths := config.Paths{ConfigDir: dir, DataDir: dir, StateDir: dir}
	if err := requireGatewayConfig(paths); err == nil {
		t.Fatal("expected error for missing config.json")
	}
	f, err := os.Create(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := requireGatewayConfig(paths); err != nil {
		t.Fatalf("expected nil for existing config, got %v", err)
	}
}
