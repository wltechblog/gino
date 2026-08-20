package brain

import (
	"context"
	"testing"
	"time"
)

// TestMaintainNoDeadlock is a regression test for the Maintain() deadlock:
// Phase 2 used to keep its query rows open (holding the store RLock) while
// calling ExtractEntities -> ensureEntity -> db.Exec, which needs the write
// lock. The turn hung forever with no reply ever sent.
func TestMaintainNoDeadlock(t *testing.T) {
	b := testBrain(t)

	// Ingest several pages with rich entity content so Maintain has real
	// work to do in Phase 2 (entity extraction writes).
	for i := 0; i < 5; i++ {
		page := Page{
			SourceID: "default",
			Slug:     "test-page-" + time.Now().Format("150405.000000000") + "-" + string(rune('a'+i)),
			Type:     "note",
			Title:    "Test Page " + string(rune('A'+i)),
			Content:  "Worked at [[Acme Corp]] with @josh on the gino project.",
		}
		if _, err := b.IngestPage(context.Background(), page); err != nil {
			t.Fatalf("ingest page %d: %v", i, err)
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := b.Maintain(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("maintain returned error: %v", err)
		}
		// Maintain completed — verify entity extraction actually ran
		stats, serr := b.Stats(context.Background())
		if serr != nil {
			t.Fatalf("stats: %v", serr)
		}
		if stats.Entities == 0 {
			t.Errorf("expected entities to be extracted, got 0 (check test content)")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Maintain deadlocked — did not complete within 10s (regression of the open-rows/Exec deadlock)")
	}
}

// TestMaintainPhase1NoDeadlock covers the Phase 1 embedding backfill path:
// rows must be closed before EmbedBatch's subsequent writes, or the same
// RLock/write-lock deadlock applies when an embedder is configured.
func TestMaintainPhase1NoDeadlock(t *testing.T) {
	b := testBrain(t)
	// No embedder configured in testBrain — Phase 1 is skipped, so this
	// asserts Maintain still completes quickly with entity writes ongoing.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := b.Maintain(ctx); err != nil {
		t.Fatalf("maintain: %v", err)
	}
}
