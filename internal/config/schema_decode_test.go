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

// TestMaxRequestCharsDecodes pins the config-schema key for the emergency
// request-trim cap (t-gino-rails-006 item 2): encoding/json silently drops
// unknown fields, so a drifted struct tag would silently lose the deployed
// value. Also pins the tri-state contract: absent = nil (= default), a
// JSON number always lands (including explicit 0 = disable the trim).
func TestMaxRequestCharsDecodes(t *testing.T) {
	var absent Config
	if err := json.Unmarshal([]byte(`{"agents":{"defaults":{}}}`), &absent); err != nil {
		t.Fatalf("decode absent: %v", err)
	}
	if absent.Agents.Defaults.MaxRequestChars != nil {
		t.Fatalf("absent maxRequestChars: want nil, got %d", *absent.Agents.Defaults.MaxRequestChars)
	}

	raw := []byte(`{"agents":{"defaults":{"maxRequestChars":480000}}}`)
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Agents.Defaults.MaxRequestChars == nil || *cfg.Agents.Defaults.MaxRequestChars != 480000 {
		t.Fatalf("maxRequestChars: want 480000, got %v", cfg.Agents.Defaults.MaxRequestChars)
	}

	var zero Config
	if err := json.Unmarshal([]byte(`{"agents":{"defaults":{"maxRequestChars":0}}}`), &zero); err != nil {
		t.Fatalf("decode zero: %v", err)
	}
	if zero.Agents.Defaults.MaxRequestChars == nil || *zero.Agents.Defaults.MaxRequestChars != 0 {
		t.Fatalf("explicit 0 must decode as non-nil pointer (disable-the-trim signal), got %v", zero.Agents.Defaults.MaxRequestChars)
	}
}
