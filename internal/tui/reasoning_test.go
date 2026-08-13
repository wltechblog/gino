package tui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

func TestHandleCommandReasoningOverride(t *testing.T) {
	p := providers.NewOpenAIProvider("k", "http://example.invalid", 1, 0)
	p.SetReasoningEffort("low")
	var buf bytes.Buffer
	s := New(config.Config{}, p, t.TempDir(), t.TempDir())
	s.out = &buf

	if !s.handleCommand("/reasoning high") {
		t.Fatal("expected /reasoning to keep the session running")
	}
	if p.GetReasoningEffort() != "high" {
		t.Fatalf("provider effort = %q, want high", p.GetReasoningEffort())
	}
	if !strings.Contains(buf.String(), "high") {
		t.Fatalf("expected confirmation in output, got %q", buf.String())
	}

	buf.Reset()
	if !s.handleCommand("/reason none") {
		t.Fatal("expected /reason alias to keep the session running")
	}
	if p.GetReasoningEffort() != "none" {
		t.Fatalf("provider effort = %q, want none", p.GetReasoningEffort())
	}

	buf.Reset()
	if !s.handleCommand("/reasoning bogus") {
		t.Fatal("invalid reasoning should not exit")
	}
	if p.GetReasoningEffort() != "none" {
		t.Fatalf("invalid value mutated effort to %q", p.GetReasoningEffort())
	}
	if !strings.Contains(buf.String(), "Invalid reasoning") {
		t.Fatalf("expected invalid-value message, got %q", buf.String())
	}
}
