package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// LoadConfig loads config from <homeDir>/config.json if present, then applies any environment variable overrides on top.
func LoadConfig(homeDir string) (Config, error) {
	path := filepath.Join(homeDir, "config.json")
	var cfg Config
	f, err := os.Open(path)
	if err == nil {
		defer func() { _ = f.Close() }()
		if err := json.NewDecoder(f).Decode(&cfg); err != nil {
			return Config{}, err
		}
	}
	// env vars always take precedence over the config file, enabling runtime overrides without editing config.json.
	applyEnvOverrides(&cfg)
	return cfg, nil
}

// applyEnvOverrides updates config fields from all environment variables
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("GINO_MODEL"); v != "" {
		cfg.Agents.Defaults.Model = v
	}
	if v := os.Getenv("GINO_MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Agents.Defaults.MaxTokens = n
		}
	}
	if v := os.Getenv("GINO_MAX_TOOL_ITERATIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Agents.Defaults.MaxToolIterations = n
		}
	}
	if v := os.Getenv("GINO_ENABLE_TOOL_ACTIVITY_INDICATOR"); v != "" {
		b := v != "false" && v != "0" && v != "False" && v != "FALSE"
		cfg.Agents.Defaults.EnableToolActivityIndicator = &b
	}
	if v := os.Getenv("GINO_WEB_TIMEOUT_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Agents.Defaults.Web.TimeoutS = n
		}
	}
	if v := os.Getenv("GINO_WEB_MAX_RESPONSE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Agents.Defaults.Web.MaxResponseBytes = n
		}
	}
	if v := os.Getenv("GINO_WEB_USER_AGENT"); v != "" {
		cfg.Agents.Defaults.Web.UserAgent = v
	}
}
