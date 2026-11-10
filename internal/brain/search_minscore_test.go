package brain

import (
	"context"
	"testing"
)

// TestMinScoreFiltersInSearch runs against a real SQLite brain: three pages
// sharing a keyword give ordered fused scores; the floor keeps the matches,
// drops the rest, and leaves unset-floor behavior untouched.
func TestMinScoreFiltersInSearch(t *testing.T) {
	b := testBrain(t)
	ctx := context.Background()

	// Three pages with strictly decreasing keyword relevance: query words
	// hit all three, more hits = higher BM25 score (no tie).
	strong := "deployment plan uses blue green release strategy for staging"
	mid := "deployment plan uses blue green strategy"
	weak := "deployment checklist"

	for _, pg := range []struct{ slug, content string }{
		{"strong", strong}, {"mid", mid}, {"weak", weak},
	} {
		if _, err := b.IngestPage(ctx, Page{SourceID: "default", Slug: pg.slug, Title: pg.slug, Content: pg.content}); err != nil {
			t.Fatalf("ingest %s: %v", pg.slug, err)
		}
	}

	q := "deployment plan blue green release strategy"
	res, err := b.Search(ctx, q, SearchOpts{Limit: 5, KeywordOnly: true})
	if err != nil {
		t.Fatalf("baseline search: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("baseline: want 3 results, got %d", len(res))
	}
	if !(res[0].Score > res[1].Score && res[1].Score > res[2].Score) {
		t.Fatalf("baseline scores not strictly decreasing: %f, %f, %f",
			res[0].Score, res[1].Score, res[2].Score)
	}

	// Floor between mid and weak: weak dropped, others kept in order.
	floor := (res[1].Score + res[2].Score) / 2
	res, err = b.Search(ctx, q, SearchOpts{Limit: 5, KeywordOnly: true, MinScore: floor})
	if err != nil {
		t.Fatalf("floored search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("floored: want 2 results above %f, got %d", floor, len(res))
	}
	for i, r := range res {
		if r.Score <= floor {
			t.Fatalf("floored: result %d score %f not above floor", i, r.Score)
		}
	}
	if res[0].Score < res[1].Score {
		t.Fatalf("floored: order not preserved: %f then %f", res[0].Score, res[1].Score)
	}

	// Floor above every score: everything filtered, no error.
	res, err = b.Search(ctx, q, SearchOpts{Limit: 5, KeywordOnly: true, MinScore: 1e9})
	if err != nil {
		t.Fatalf("all-filtered search: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("all-filtered: want 0 results, got %d", len(res))
	}
}

// TestApplyMinScoreUnit exercises the filter directly on rankedResult
// values, pinning the passthrough and strictly-greater semantics.
func TestApplyMinScoreUnit(t *testing.T) {
	b := &Brain{}
	mk := func(scores ...float64) []rankedResult {
		out := make([]rankedResult, 0, len(scores))
		for _, s := range scores {
			out = append(out, rankedResult{Score: s, Title: "r"})
		}
		return out
	}

	in := mk(0.05, 0.005, 0.001)
	if got := b.applyMinScore(in, SearchOpts{}); len(got) != 3 {
		t.Fatalf("MinScore unset: want 3 kept, got %d", len(got))
	}
	if got := b.applyMinScore(in, SearchOpts{MinScore: -1}); len(got) != 3 {
		t.Fatalf("negative MinScore: want passthrough, got %d kept", len(got))
	}

	got := b.applyMinScore(mk(0.02, 0.01, 0.009), SearchOpts{MinScore: 0.01})
	if len(got) != 1 || got[0].Score != 0.02 {
		t.Fatalf("strict floor: want only 0.02 kept, got %+v", got)
	}

	// Strictly-greater semantics: the result exactly AT the floor is dropped.
	got = b.applyMinScore(mk(0.03, 0.02, 0.015, 0.009), SearchOpts{MinScore: 0.015})
	if len(got) != 2 || got[0].Score != 0.03 || got[1].Score != 0.02 {
		t.Fatalf("strict floor: want [0.03 0.02] kept (0.015 not > floor), got %+v", got)
	}
}
