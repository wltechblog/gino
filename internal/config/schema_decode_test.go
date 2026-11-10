package config

import (
	"encoding/json"
	"testing"
)

// TestSessionCompactionMemoryMinScoreDecodes pins the config-schema key:
// encoding/json silently drops unknown fields, so if the struct tag drifts
// from the documented key the deployed config value would be silently lost.
func TestSessionCompactionMemoryMinScoreDecodes(t *testing.T) {
	raw := []byte(`{"agents":{"defaults":{"sessionCompaction":{"enabled":true,"compactAt":40,"keepRecent":16,"memoryMinScore":0.09}}}}`)
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := cfg.Agents.Defaults.SessionCompaction.MemoryMinScore; got != 0.09 {
		t.Fatalf("memoryMinScore: want 0.09, got %f", got)
	}
}
