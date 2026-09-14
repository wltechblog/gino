package config

import "testing"

func TestParseLevelsEnv(t *testing.T) {
	got := parseLevelsEnv("minimal, low ,,HIGH,")
	if len(got) != 3 || got[0] != "minimal" || got[1] != "low" || got[2] != "HIGH" {
		t.Errorf("parseLevelsEnv: %v", got)
	}
	if parseLevelsEnv("") != nil {
		t.Error("empty env value should yield nil")
	}
	if parseLevelsEnv(" , ,") != nil {
		t.Error("whitespace-only should yield nil")
	}
}

func TestLogLevelEnvOverride(t *testing.T) {
	t.Setenv("GINO_LOG_LEVEL", "OFF")
	cfg := Config{}
	applyEnvOverrides(&cfg)
	if !cfg.Agents.Defaults.LogsSilenced() {
		t.Fatalf("expected logs silenced via env, got level=%q", cfg.Agents.Defaults.LogLevel)
	}
	if got, want := cfg.Agents.Defaults.LogLevel, "off"; got != want {
		t.Fatalf("level = %q, want %q (normalized lowercase)", got, want)
	}
}

func TestLogLevelInfoIsNotSilenced(t *testing.T) {
	t.Setenv("GINO_LOG_LEVEL", "info")
	cfg := Config{}
	applyEnvOverrides(&cfg)
	if cfg.Agents.Defaults.LogsSilenced() {
		t.Fatal("info level must not silence logs")
	}
}
