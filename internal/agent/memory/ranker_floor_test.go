package memory

import "testing"

func TestFilterByFloor(t *testing.T) {
	items := []MemoryItem{{Text: "alpha"}, {Text: "beta"}, {Text: "gamma"}}

	// Floor disabled (0): everything passes through.
	if got := FilterByFloor(items, 0); len(got) != 3 {
		t.Fatalf("floor 0: want 3 kept, got %d", len(got))
	}
	// Negative floor: same as disabled.
	if got := FilterByFloor(items, -1); len(got) != 3 {
		t.Fatalf("negative floor: want 3 kept, got %d", len(got))
	}
	// Nil/empty input never panics.
	if got := FilterByFloor(nil, 0.5); len(got) != 0 {
		t.Fatalf("nil input: want 0, got %d", len(got))
	}
	if got := FilterByFloor([]MemoryItem{}, 0.5); len(got) != 0 {
		t.Fatalf("empty input: want 0, got %d", len(got))
	}

	// Positions: rank0=0.1, rank1≈0.0909, rank2≈0.0833.
	kept := FilterByFloor(items, 0.2) // rank-0 (0.1) is below this → all dropped
	if len(kept) != 0 {
		t.Fatalf("floor 0.2: want 0 kept, got %d", len(kept))
	}
	kept = FilterByFloor(items, 0.09) // rank-0 (0.1) + rank-1 (≈0.0909) clear; rank-2 (≈0.0833) drops
	if len(kept) != 2 || kept[0].Text != "alpha" || kept[1].Text != "beta" {
		t.Fatalf("floor 0.09: want [alpha beta], got %+v", kept)
	}
	kept = FilterByFloor(items, 0.095) // only rank-0 (0.1) clears
	if len(kept) != 1 || kept[0].Text != "alpha" {
		t.Fatalf("floor 0.095: want [alpha], got %+v", kept)
	}
	// Boundary: rank-0 lands exactly ON floor 0.1 → injects (>= semantics).
	kept = FilterByFloor(items, 0.1)
	if len(kept) != 1 || kept[0].Text != "alpha" {
		t.Fatalf("floor 0.1 boundary: want [alpha] kept, got %+v", kept)
	}
}
