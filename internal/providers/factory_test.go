package providers

import (
	"testing"

	"github.com/wltechblog/gino/internal/config"
)

func TestNewProviderFromConfig_PicksOpenAI(t *testing.T) {
	cfg := config.Config{}
	cfg.Providers.OpenAI = &config.ProviderConfig{APIKey: "test"}
	p := NewProviderFromConfig(cfg)
	_, ok := p.(*OpenAIProvider)
	if !ok {
		t.Fatalf("expected OpenAIProvider, got %T", p)
	}
}

func TestNewProviderFromConfig_FallbacksToStub(t *testing.T) {
	cfg := config.Config{}
	p := NewProviderFromConfig(cfg)
	_, ok := p.(*StubProvider)
	if !ok {
		t.Fatalf("expected StubProvider, got %T", p)
	}
}

// ---- t-gino-rails-006 item 2: trim-cap wiring tests ----

// TestApplyTrimCapFromConfig pins the tri-state wiring: nil keeps the
// current cap, 0 disables the trim, positive overrides.
func TestApplyTrimCapFromConfig(t *testing.T) {
	restore := func() { SetMaxRequestChars(DefaultMaxRequestChars) }
	defer restore()

	// nil = current cap stays.
	SetMaxRequestChars(12345)
	applyTrimCapFromConfig(nil)
	if got := effectiveTrimCap(); got != 12345 {
		t.Fatalf("nil config must leave cap untouched: want 12345, got %d", got)
	}
	restore()

	// explicit 0 = disable.
	applyTrimCapFromConfig(new(int))
	if got := effectiveTrimCap(); got != 0 {
		t.Fatalf("explicit 0 must disable the trim: want 0, got %d", got)
	}
	restore()

	// positive = override.
	v := 480000
	applyTrimCapFromConfig(&v)
	if got := effectiveTrimCap(); got != 480000 {
		t.Fatalf("positive config must override cap: want 480000, got %d", got)
	}
}

// TestNewProviderFromConfigWiresTrimCap is the end-to-end pin: the config
// key must actually move the process-global cap when the provider factory
// runs (if the wiring drifts, the knob silently stops working — the exact
// failure class this task exists to close).
func TestNewProviderFromConfigWiresTrimCap(t *testing.T) {
	restore := func() { SetMaxRequestChars(DefaultMaxRequestChars) }
	defer restore()

	zero := 0
	_ = NewProviderFromConfig(config.Config{
		Providers: config.ProvidersConfig{OpenAI: &config.ProviderConfig{APIKey: "k", APIBase: "http://example.invalid"}},
		Agents:    config.AgentsConfig{Defaults: config.AgentDefaults{MaxRequestChars: &zero}},
	})
	if got := effectiveTrimCap(); got != 0 {
		t.Fatalf("factory must apply maxRequestChars=0 (disable): want cap 0, got %d", got)
	}
}
