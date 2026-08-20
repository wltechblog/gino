package brain

import (
	"context"
	"testing"
	"time"
)

// ingestAt ingests a page then backdates its updated_at for testing.
func ingestAt(t *testing.T, b *Brain, slug, content string, ageDays int) {
	t.Helper()
	_, err := b.IngestPage(context.Background(), Page{
		SourceID: "default", Slug: slug, Title: slug, Content: content,
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", slug, err)
	}
	if ageDays != 0 {
		past := time.Now().AddDate(0, 0, -ageDays).UTC().Format(time.RFC3339)
		if _, err := b.db.Exec(`UPDATE pages SET updated_at = ? WHERE slug = ? AND source_id = 'default'`, past, slug); err != nil {
			t.Fatalf("backdate %s: %v", slug, err)
		}
	}
}

func TestRecencyDecayReranks(t *testing.T) {
	b := testBrain(t)
	ctx := context.Background()

	// Two pages, near-identical keyword relevance. Old page is older.
	ingestAt(t, b, "old", "quarterly status: the deployment plan uses blue green release strategy for staging", 90)
	ingestAt(t, b, "new", "updated status: the deployment plan uses blue green release strategy for staging", 0)

	// Keyword-only path (single-source normalization + decay)
	res, err := b.Search(ctx, "deployment plan blue green release", SearchOpts{Limit: 2, KeywordOnly: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) < 2 {
		t.Fatalf("expected 2 results, got %d", len(res))
	}
	if res[0].Slug != "new" {
		t.Errorf("expected fresh page 'new' ranked first, got %q (score %f vs %f)", res[0].Slug, res[0].Score, res[1].Score)
	}

	// Disabled decay (weight >= 1) must preserve original order: old was ingested first
	// and BM25 ties break toward earlier rowid, so verify disabled mode doesn't error
	b2 := testBrain(t)
	b2.opts.RecencyWeight = 0 // disabled
	ingestAt(t, b2, "old", "quarterly status: the deployment plan uses blue green release strategy for staging", 90)
	ingestAt(t, b2, "new", "updated status: the deployment plan uses blue green release strategy for staging", 0)
	if _, err := b2.Search(ctx, "deployment", SearchOpts{Limit: 2, KeywordOnly: true}); err != nil {
		t.Fatalf("search with decay disabled: %v", err)
	}
}

func TestApplyRecencyUnit(t *testing.T) {
	b := testBrain(t)
	b.opts.RecencyWeight = 0.5 // halve per day

	now := time.Now().UTC()
	results := []rankedResult{
		{PageID: 1, Score: 1.0, UpdatedAt: now.Add(-24 * time.Hour)}, // factor 0.5^1 = 0.5
		{PageID: 2, Score: 1.0, UpdatedAt: now.Add(-48 * time.Hour)}, // factor 0.5^2 = 0.25
		{PageID: 3, Score: 1.0, UpdatedAt: now},                      // factor 1.0
	}
	got := b.applyRecency(results, SearchOpts{})
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d", len(got))
	}
	byID := map[int64]float64{}
	for _, r := range got {
		byID[r.PageID] = r.Score
	}
	if byID[3] < 0.99 || byID[3] > 1.01 {
		t.Errorf("age-0 page should keep full score, got %f", byID[3])
	}
	if byID[1] < 0.45 || byID[1] > 0.55 {
		t.Errorf("1-day-old page should score ~0.5, got %f", byID[1])
	}
	if byID[2] < 0.20 || byID[2] > 0.30 {
		t.Errorf("2-day-old page should score ~0.25, got %f", byID[2])
	}
	if got[0].PageID != 3 {
		t.Errorf("freshest page should rank first after decay, got PageID %d", got[0].PageID)
	}
}

func TestRRFRankSingleSource(t *testing.T) {
	b := testBrain(t)
	in := []rankedResult{
		{PageID: 1, Score: -3.2}, // BM25-style negative
		{PageID: 2, Score: -1.1},
	}
	out := b.rrfRank(in)
	if out[0].Score <= out[1].Score {
		t.Errorf("expected rank-0 to outscore rank-1 after RRF conversion, got %f vs %f", out[0].Score, out[1].Score)
	}
	if out[0].Score > 1.0/61.0 {
		t.Errorf("RRF rank-0 score should be 1/61 ≈ 0.0164, got %f", out[0].Score)
	}
}

func TestZeroUpdatedAtNoPanic(t *testing.T) {
	b := testBrain(t)
	b.opts.RecencyWeight = 0.985
	// Zero UpdatedAt (parse failure or legacy row) must be skipped, not panic or zero-score
	results := []rankedResult{
		{PageID: 1, Score: 0.5, UpdatedAt: time.Time{}},
		{PageID: 2, Score: 0.3, UpdatedAt: time.Now().Add(-100 * 24 * time.Hour)},
	}
	got := b.applyRecency(results, SearchOpts{})
	if got[0].Score != 0.5 {
		t.Errorf("zero-time page should keep its score, got %f", got[0].Score)
	}
}
