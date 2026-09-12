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
