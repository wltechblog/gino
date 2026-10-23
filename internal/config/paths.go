package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Paths holds the directories gino uses at runtime, following the XDG Base
// Directory Specification.
//
//	ConfigDir  config.json, MCP OAuth tokens
//	DataDir    workspace, brain database, project registry
//	StateDir   cron jobs, background jobs, signal routes
type Paths struct {
	ConfigDir string
	DataDir   string
	StateDir  string
}

// PathOverrides carries explicit CLI overrides. Empty fields fall back to the
// matching environment variable, then to the XDG defaults.
type PathOverrides struct {
	Home      string
	ConfigDir string
	DataDir   string
	StateDir  string
}

// ConfigFile returns the path to config.json.
func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "config.json") }

// TokensFile returns the MCP OAuth token store path.
func (p Paths) TokensFile() string { return filepath.Join(p.ConfigDir, "tokens.json") }

// Workspace returns the default agent workspace path.
func (p Paths) Workspace() string { return filepath.Join(p.DataDir, "workspace") }

// BrainDB returns the knowledge brain database path.
func (p Paths) BrainDB() string { return filepath.Join(p.DataDir, "brain.db") }

// ProjectsFile returns the project registry path.
func (p Paths) ProjectsFile() string { return filepath.Join(p.DataDir, "projects.json") }

// CronJobsFile returns the persisted cron job path.
func (p Paths) CronJobsFile() string { return filepath.Join(p.StateDir, "cron_jobs.json") }

// BackgroundJobsFile returns the persisted background job path.
func (p Paths) BackgroundJobsFile() string { return filepath.Join(p.StateDir, "background_jobs.json") }

// SignalRoutesFile returns the persisted signal routing table path.
func (p Paths) SignalRoutesFile() string { return filepath.Join(p.StateDir, "signal_routes.json") }

// IsUnified reports whether all three roots point at the same directory,
// which is the legacy single-home layout still used by the installer and
// Docker image.
func (p Paths) IsUnified() bool {
	return p.ConfigDir == p.DataDir && p.DataDir == p.StateDir
}

// Ensure creates all three base directories.
func (p Paths) Ensure() error {
	for _, dir := range []string{p.ConfigDir, p.DataDir, p.StateDir} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// UserHomeDir resolves the user's home directory with a fallback chain:
// $HOME first, then the user database. Under systemd system services $HOME
// is typically unset; user.Current still resolves via /etc/passwd (pure Go,
// no cgo needed), so root-run gateways keep working.
func UserHomeDir() (string, error) {
	if h := os.Getenv("HOME"); h != "" {
		return h, nil
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir, nil
	}
	return "", fmt.Errorf("cannot determine home directory ($HOME unset and user lookup failed) — pass -home <dir>")
}

// ResolvePaths resolves the runtime directories. Precedence, highest first:
//
//	per-directory flag, per-directory env var, -home/GINO_HOME,
//	XDG environment variable, XDG default under $HOME.
//
// When nothing is configured and a legacy ~/.gino directory exists (and no
// XDG config has been written yet), the legacy unified home is returned so
// existing installs keep working after upgrading.
func ResolvePaths(overrides PathOverrides) (Paths, error) {
	home := firstNonEmpty(overrides.Home, os.Getenv("GINO_HOME"))
	configDir := firstNonEmpty(overrides.ConfigDir, os.Getenv("GINO_CONFIG_DIR"))
	dataDir := firstNonEmpty(overrides.DataDir, os.Getenv("GINO_DATA_DIR"))
	stateDir := firstNonEmpty(overrides.StateDir, os.Getenv("GINO_STATE_DIR"))

	explicit := home != "" || configDir != "" || dataDir != "" || stateDir != "" ||
		os.Getenv("XDG_CONFIG_HOME") != "" ||
		os.Getenv("XDG_DATA_HOME") != "" ||
		os.Getenv("XDG_STATE_HOME") != ""

	if !explicit {
		if legacy, ok := legacyHome(); ok {
			return Paths{ConfigDir: legacy, DataDir: legacy, StateDir: legacy}, nil
		}
	}

	if configDir == "" && home != "" {
		configDir = home
	}
	if dataDir == "" && home != "" {
		dataDir = home
	}
	if stateDir == "" && home != "" {
		stateDir = home
	}

	var err error
	if configDir == "" {
		if configDir, err = xdgDefault("XDG_CONFIG_HOME", ".config"); err != nil {
			return Paths{}, err
		}
	}
	if dataDir == "" {
		if dataDir, err = xdgDefault("XDG_DATA_HOME", filepath.Join(".local", "share")); err != nil {
			return Paths{}, err
		}
	}
	if stateDir == "" {
		if stateDir, err = xdgDefault("XDG_STATE_HOME", filepath.Join(".local", "state")); err != nil {
			return Paths{}, err
		}
	}

	p := Paths{ConfigDir: configDir, DataDir: dataDir, StateDir: stateDir}
	for _, dir := range []*string{&p.ConfigDir, &p.DataDir, &p.StateDir} {
		abs, err := absoluteDir(*dir)
		if err != nil {
			return Paths{}, err
		}
		*dir = abs
	}
	return p, nil
}

// xdgDefault returns $<env>/gino when the variable holds an absolute path,
// otherwise the path under $HOME.
func xdgDefault(env, fallback string) (string, error) {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return filepath.Join(v, "gino"), nil
	}
	home, err := UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, fallback, "gino"), nil
}

func absoluteDir(dir string) (string, error) {
	dir = expandUser(dir)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path %q: %w", dir, err)
	}
	return abs, nil
}

// legacyHome returns ~/.gino when it exists and no XDG config is present.
func legacyHome() (string, bool) {
	home, err := UserHomeDir()
	if err != nil {
		return "", false
	}
	legacy := filepath.Join(home, ".gino")
	if info, err := os.Stat(legacy); err != nil || !info.IsDir() {
		return "", false
	}
	xdgConfig := filepath.Join(home, ".config", "gino", "config.json")
	if _, err := os.Stat(xdgConfig); err == nil {
		return "", false
	}
	return legacy, true
}

func expandUser(p string) string {
	if p == "~" {
		if home, err := UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
