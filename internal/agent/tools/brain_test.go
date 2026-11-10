package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/brain"
)

// newToolTestBrain builds a brain with ContextMinScore left at the zero
// value: Init does NOT apply DefaultOptions, so ContextFloor() == 0 and the
// tool's floor is deterministically DISABLED — score boundaries below are
// exact, not subject to a tunable default.
func newToolTestBrain(t *testing.T) *brain.Brain {
	t.Helper()
	b, err := brain.Init(filepath.Join(t.TempDir(), "brain.db"), nil, brain.Options{})
	if err != nil {
		t.Fatalf("init brain: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// TestBrainSearchToolAppliesConfiguredFloor pins the GINO-RAILS-002 fix:
// the model-invoked brain_search tool must apply the brain's configured
// relevance floor. Before the fix it passed raw SearchOpts{Limit} — a legit
// query surfaced low-score noise ("score: 0.01/0.02") that the model then
// treated as fact (live stale-answer specimen). Top-1 organic RRF score on
// gino's k=60 scale is 1/61 ≈ 0.0164, rank-2 is 1/62 ≈ 0.0161 — both above
// the 0.005 floor used here; a two-word-only match (rank-3 ≈ 0.0159 is
// still above, so the weak page needs to miss the strong terms entirely)
// must drop when its only overlap is a single weak term that BM25 ranks
// below the floor after recency decay... verified empirically below via a
// floor placed between the observed score bands.
func TestBrainSearchToolAppliesConfiguredFloor(t *testing.T) {
	b := newToolTestBrain(t)
	if b.ContextFloor() != 0 {
		t.Fatalf("test brain must have floor disabled (got %f) so the tool's SearchOpts are exact", b.ContextFloor())
	}
	ctx := context.Background()
	// Two pages sharing the strong terms; the weak page shares one term.
	for _, pg := range []struct{ slug, content string }{
		{"strong", "deployment plan uses blue green release strategy for staging"},
		{"mid", "deployment plan uses blue green strategy"},
		{"weak", "deployment checklist"},
	} {
		if _, err := b.IngestPage(ctx, brain.Page{SourceID: "default", Slug: pg.slug, Title: pg.slug, Content: pg.content}); err != nil {
			t.Fatalf("ingest %s: %v", pg.slug, err)
		}
	}
	tk := NewBrainSearchTool(b)

	// Baseline (no floor): all three pages come back, strong > mid, weak
	// ranked last with a strictly lower score.
	out, err := tk.Execute(ctx, map[string]interface{}{"query": "deployment plan blue green release strategy"})
	if err != nil {
		t.Fatalf("baseline search: %v", err)
	}
	if !strings.Contains(out, "strong") || !strings.Contains(out, "mid") {
		t.Fatalf("baseline missing expected pages: %q", out)
	}

	// Baseline raw scores, so the floor below is EMPIRICALLY placed
	// between rank-2 and rank-3 regardless of BM25 details.
	res, err := b.Search(ctx, "deployment plan blue green release strategy", brain.SearchOpts{Limit: 5, KeywordOnly: true})
	if err != nil {
		t.Fatalf("raw baseline: %v", err)
	}
	if len(res) != 3 || !(res[0].Score > res[1].Score && res[1].Score > res[2].Score) {
		t.Fatalf("raw baseline not 3 strictly-decreasing results: %+v", res)
	}
	floor := (res[1].Score + res[2].Score) / 2

	// With the floor configured, the tool must drop the sub-floor page
	// (pre-fix it shipped whatever the raw search returned).
	opts := brain.DefaultOptions()
	opts.ContextMinScore = floor
	fb, err := brain.Init(filepath.Join(t.TempDir(), "brain2.db"), nil, opts)
	if err != nil {
		t.Fatalf("init floored brain: %v", err)
	}
	t.Cleanup(func() { fb.Close() })
	for _, pg := range []struct{ slug, content string }{
		{"strong", "deployment plan uses blue green release strategy for staging"},
		{"mid", "deployment plan uses blue green strategy"},
		{"weak", "deployment checklist"},
	} {
		if _, err := fb.IngestPage(ctx, brain.Page{SourceID: "default", Slug: pg.slug, Title: pg.slug, Content: pg.content}); err != nil {
			t.Fatalf("ingest %s (floored): %v", pg.slug, err)
		}
	}
	ftk := NewBrainSearchTool(fb)
	out, err = ftk.Execute(ctx, map[string]interface{}{"query": "deployment plan blue green release strategy"})
	if err != nil {
		t.Fatalf("floored search: %v", err)
	}
	if !strings.Contains(out, "strong") || !strings.Contains(out, "mid") {
		t.Fatalf("floored tool search lost above-floor pages: %q", out)
	}
	if strings.Contains(out, "weak") {
		t.Fatalf("floored tool search still returned sub-floor page (pre-fix behavior): %q", out)
	}
}

// TestBrainSearchToolFloorZeroDisabled pins the explicit-disable contract:
// contextMinScore <= 0 means NO floor — the tool returns everything, same
// as the pre-fix behavior, for operators who deliberately opt out.
func TestBrainSearchToolFloorZeroDisabled(t *testing.T) {
	b := newToolTestBrain(t) // Options{} → ContextMinScore 0
	ctx := context.Background()
	if _, err := b.IngestPage(ctx, brain.Page{SourceID: "default", Slug: "p1", Title: "p1", Content: "harbor sunset and deep orange light"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	tk := NewBrainSearchTool(b)
	out, err := tk.Execute(ctx, map[string]interface{}{"query": "sunset harbor"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(out, "p1") {
		t.Fatalf("zero floor must disable filtering entirely: %q", out)
	}
}
