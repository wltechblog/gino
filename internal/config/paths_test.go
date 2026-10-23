package config

import (
	"os"
	"path/filepath"
	"testing"
)

// isolatePaths clears every path-related environment variable and points
// HOME at a fresh temp dir.
func isolatePaths(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
		"GINO_HOME", "GINO_CONFIG_DIR", "GINO_DATA_DIR", "GINO_STATE_DIR",
	} {
		t.Setenv(name, "")
	}
	return home
}

func TestResolvePathsXDGDefaults(t *testing.T) {
	home := isolatePaths(t)

	got, err := ResolvePaths(PathOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		ConfigDir: filepath.Join(home, ".config", "gino"),
		DataDir:   filepath.Join(home, ".local", "share", "gino"),
		StateDir:  filepath.Join(home, ".local", "state", "gino"),
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestResolvePathsXDGEnv(t *testing.T) {
	isolatePaths(t)
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	t.Setenv("XDG_DATA_HOME", "/xdg/data")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")

	got, err := ResolvePaths(PathOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		ConfigDir: "/xdg/config/gino",
		DataDir:   "/xdg/data/gino",
		StateDir:  "/xdg/state/gino",
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestResolvePathsIgnoresRelativeXDG(t *testing.T) {
	home := isolatePaths(t)
	t.Setenv("XDG_DATA_HOME", "relative/path")

	got, err := ResolvePaths(PathOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "gino"); got.DataDir != want {
		t.Fatalf("data dir = %q, want %q", got.DataDir, want)
	}
}

func TestResolvePathsUnifiedHome(t *testing.T) {
	isolatePaths(t)

	got, err := ResolvePaths(PathOverrides{Home: "/opt/gino"})
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{ConfigDir: "/opt/gino", DataDir: "/opt/gino", StateDir: "/opt/gino"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if !got.IsUnified() {
		t.Fatal("expected unified layout")
	}
}

func TestResolvePathsPerDirOverridesWin(t *testing.T) {
	isolatePaths(t)

	got, err := ResolvePaths(PathOverrides{Home: "/h", ConfigDir: "/c"})
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{ConfigDir: "/c", DataDir: "/h", StateDir: "/h"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestResolvePathsEnvOverrides(t *testing.T) {
	isolatePaths(t)
	t.Setenv("GINO_DATA_DIR", "/srv/gino")

	got, err := ResolvePaths(PathOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got.DataDir != "/srv/gino" {
		t.Fatalf("data dir = %q, want /srv/gino", got.DataDir)
	}
}

func TestResolvePathsLegacyFallback(t *testing.T) {
	home := isolatePaths(t)
	legacy := filepath.Join(home, ".gino")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := ResolvePaths(PathOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsUnified() || got.ConfigDir != legacy {
		t.Fatalf("got %+v, want legacy unified home %q", got, legacy)
	}
}

func TestResolvePathsXDGWinsOverLegacy(t *testing.T) {
	home := isolatePaths(t)
	if err := os.MkdirAll(filepath.Join(home, ".gino"), 0o755); err != nil {
		t.Fatal(err)
	}
	xdgConfig := filepath.Join(home, ".config", "gino")
	if err := os.MkdirAll(xdgConfig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdgConfig, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ResolvePaths(PathOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigDir != xdgConfig {
		t.Fatalf("config dir = %q, want %q", got.ConfigDir, xdgConfig)
	}
}

func TestPathsEnsure(t *testing.T) {
	root := t.TempDir()
	paths := Paths{
		ConfigDir: filepath.Join(root, "config"),
		DataDir:   filepath.Join(root, "data", "nested"),
		StateDir:  filepath.Join(root, "state"),
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{paths.ConfigDir, paths.DataDir, paths.StateDir} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("%s was not created: %v", dir, err)
		}
	}
}

func TestPathsFileHelpers(t *testing.T) {
	p := Paths{ConfigDir: "/c", DataDir: "/d", StateDir: "/s"}
	cases := map[string]string{
		p.ConfigFile():         "/c/config.json",
		p.TokensFile():         "/c/tokens.json",
		p.Workspace():          "/d/workspace",
		p.BrainDB():            "/d/brain.db",
		p.ProjectsFile():       "/d/projects.json",
		p.CronJobsFile():       "/s/cron_jobs.json",
		p.BackgroundJobsFile(): "/s/background_jobs.json",
		p.SignalRoutesFile():   "/s/signal_routes.json",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestResolvePathsGinoHomeEnv(t *testing.T) {
	isolatePaths(t)
	t.Setenv("GINO_HOME", "/env/gino")

	got, err := ResolvePaths(PathOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{ConfigDir: "/env/gino", DataDir: "/env/gino", StateDir: "/env/gino"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestResolvePathsExpandsTilde(t *testing.T) {
	home := isolatePaths(t)

	got, err := ResolvePaths(PathOverrides{Home: "~/bundle"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "bundle")
	if got.ConfigDir != want {
		t.Fatalf("config dir = %q, want %q", got.ConfigDir, want)
	}
}
