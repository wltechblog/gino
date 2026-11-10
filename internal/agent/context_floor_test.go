package agent

import (
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/agent/memory"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// lastUserContent returns the content of the final (current-turn) user
// message — the one carrying the <turn_context> volatile wrap.
func lastUserContent(t *testing.T, msgs []providers.Message) string {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("no messages built")
	}
	return msgs[len(msgs)-1].Content
}

// TestBuildMessagesSkipsMemoryContextOnLowSignal pins the GINO-RAILS-005
// presence gate: the raw file-memory dump (memoryContext) is suppressed on
// degenerate prompts. Presence beats rank for an LLM — injecting the raw
// MEMORY.md + daily-note dump on "say 1" surfaced stale fragments that then
// steered the turn.
func TestBuildMessagesSkipsMemoryContextOnLowSignal(t *testing.T) {
	cb := NewContextBuilder(t.TempDir(), memory.NewSimpleRanker(), 5)
	const dump = "SECRET-ROTATION-FRAGMENT-42 owner credential notes"

	// Low-signal prompt: no Memory block at all.
	msgs := cb.BuildMessages(nil, "say 1", "telegram", "c1", "u1", dump, nil, nil)
	if got := lastUserContent(t, msgs); strings.Contains(got, "Memory:\n"+dump) {
		t.Fatalf("low-signal prompt injected memory dump:\n%s", got)
	}

	// Content prompt: the dump IS injected (presence gate opens).
	msgs = cb.BuildMessages(nil, "what should I know about credentials", "telegram", "c1", "u1", dump, nil, nil)
	if got := lastUserContent(t, msgs); !strings.Contains(got, "Memory:\n"+dump) {
		t.Fatalf("content prompt lost memory dump:\n%s", got)
	}

	// Unprivileged turns keep the privilege gate regardless of query signal.
	msgs = cb.BuildMessages(nil, "what should I know about credentials", "telegram", "c1", "u1", dump, nil,
		map[string]interface{}{"privileged": false})
	if got := lastUserContent(t, msgs); strings.Contains(got, "Memory:\n"+dump) {
		t.Fatalf("unprivileged turn leaked memory dump:\n%s", got)
	}
}

// TestBuildMessagesMemoryFloorDropsBelowFloor pins the GINO-RAILS-005
// memory-ranker floor end to end: below-floor ranked items never reach the
// "Relevant memories" block; 0 keeps legacy behavior (everything injects).
func TestBuildMessagesMemoryFloorDropsBelowFloor(t *testing.T) {
	items := []memory.MemoryItem{
		{Text: "zulu deployment blue green release", Kind: "note"},
		{Text: "yankee deployment plan", Kind: "note"},
		{Text: "xray checklist", Kind: "note"},
	}
	// Token overlap with the query is strictly decreasing (4 / 2 / 0), so
	// SimpleRanker rank order equals stored order: positions 0.1 / 0.0909 /
	// 0.0833 on the reciprocal scale. Item texts are mutually non-substring
	// so Contains assertions cannot alias across items.
	query := "deployment blue green release plan strategy"

	build := func(floor float64) string {
		cb := NewContextBuilder(t.TempDir(), memory.NewSimpleRanker(), 5)
		cb.SetMemoryFloor(floor)
		msgs := cb.BuildMessages(nil, query, "cli", "c1", "", "", items, nil)
		return lastUserContent(t, msgs)
	}

	// Floor 0.09: rank-0 (0.1) + rank-1 (≈0.0909) clear; rank-2 (≈0.0833) drops.
	got := build(0.09)
	if !strings.Contains(got, "zulu deployment blue green release") || !strings.Contains(got, "yankee deployment plan") {
		t.Fatalf("floor 0.09 lost top-2:\n%s", got)
	}
	if strings.Contains(got, "xray checklist") {
		t.Fatalf("floor 0.09 kept below-floor rank-2:\n%s", got)
	}

	// Floor 0.095: only rank-0 clears.
	got = build(0.095)
	if !strings.Contains(got, "zulu deployment blue green release") {
		t.Fatalf("floor 0.095 lost rank-0:\n%s", got)
	}
	if strings.Contains(got, "yankee deployment plan") || strings.Contains(got, "xray checklist") {
		t.Fatalf("floor 0.095 kept below-floor items:\n%s", got)
	}

	// Floor 0 (disabled): legacy behavior, all ranked items inject.
	got = build(0)
	for _, want := range []string{"zulu deployment blue green release", "yankee deployment plan", "xray checklist"} {
		if !strings.Contains(got, want) {
			t.Fatalf("floor 0 lost %q:\n%s", want, got)
		}
	}
}

// TestSetSessionCompactionWiresMemoryFloor pins the config→loop→context
// wiring: agents.defaults.sessionCompaction.memoryMinScore must reach the
// ContextBuilder, not silently drop (plain json.Decode drops unknown keys;
// the wiring must not).
func TestSetSessionCompactionWiresMemoryFloor(t *testing.T) {
	ag := newLoopForCompaction(t, &summaryProvider{})
	if ag.context == nil {
		t.Fatal("agent loop has no context builder")
	}
	if ag.context.memFloor != 0 {
		t.Fatalf("default floor: want 0 (disabled), got %f", ag.context.memFloor)
	}
	ag.SetSessionCompaction(&config.SessionCompactionConfig{MemoryMinScore: 0.09})
	if ag.context.memFloor != 0.09 {
		t.Fatalf("wired floor: want 0.09, got %f", ag.context.memFloor)
	}
	// Disabled value (0) must also propagate, turning the floor off.
	ag.SetSessionCompaction(&config.SessionCompactionConfig{MemoryMinScore: 0})
	if ag.context.memFloor != 0 {
		t.Fatalf("zero floor: want 0, got %f", ag.context.memFloor)
	}
}
