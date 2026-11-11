package session

import (
	"strings"
	"testing"
)

// TestAddMessageClampsFatEntry (work-order item 4): a fat tool dump must
// be clamped at the entry cap when it enters history — not persisted
// verbatim into a multi-MB session file.
func TestAddMessageClampsFatEntry(t *testing.T) {
	s := &Session{}
	fat := strings.Repeat("x", MaxMessageChars+100_000)
	s.AddMessage("assistant", fat)

	if len(s.History) != 1 {
		t.Fatalf("history entries: want 1, got %d", len(s.History))
	}
	entry := s.History[0]
	if len(entry) > MaxMessageChars+len("assistant: ") {
		t.Fatalf("entry not clamped: %d chars (cap %d)", len(entry), MaxMessageChars)
	}
	if !strings.Contains(entry, "chars elided by session size cap") {
		t.Fatalf("no elision marker in clamped entry")
	}
	// Head+tail framing: both ends of the original content survive.
	if !strings.HasPrefix(entry, "assistant: xxx") {
		t.Fatalf("head of content lost")
	}
	if !strings.HasSuffix(entry, fat[len(fat)-10:]) {
		t.Fatalf("tail of content lost")
	}
}

// TestAddMessageKeepsSmallEntries: content under the cap must pass
// through byte-for-byte (no mutation of normal messages).
func TestAddMessageKeepsSmallEntries(t *testing.T) {
	s := &Session{}
	msg := "normal message, well under the cap"
	s.AddMessage("user", msg)
	if s.History[0] != "user: "+msg {
		t.Fatalf("small message mutated: %q", s.History[0])
	}
}

// TestTrimDropsOldestPastCharBudget: the total-char budget must evict
// oldest-first, keeping the newest (live-topic) entry.
func TestTrimDropsOldestPastCharBudget(t *testing.T) {
	s := &Session{}
	// 3 fat entries (24k each) = 72k total; budget 50k -> 2 must go.
	for i := 0; i < 3; i++ {
		s.AddMessage("assistant", strings.Repeat("t", MaxMessageChars))
	}
	if MaxHistoryChars < MaxMessageChars*3 {
		t.Fatalf("test constants changed; rewrite this test")
	}
	// Shrink the budget for the test by simulating: use the real trim but
	// with a history guaranteed over budget.
	saved := MaxHistoryChars
	// Cannot assign to a const; instead pad the history until over budget.
	s2 := &Session{}
	entries := MaxHistoryChars/MaxMessageChars + 2 // definitely over budget
	for i := 0; i < entries; i++ {
		s2.AddMessage("assistant", strings.Repeat("t", MaxMessageChars))
	}
	s2.trim()
	total := 0
	for i := range s2.History {
		total += len(s2.History[i])
	}
	if total > MaxHistoryChars {
		t.Fatalf("history over budget after trim: %d > %d", total, MaxHistoryChars)
	}
	if s2.History[len(s2.History)-1] != s2.History[len(s2.History)-1] {
		t.Fatalf("unreachable")
	}
	// Newest entry must survive.
	last := s2.History[len(s2.History)-1]
	if !strings.HasPrefix(last, "assistant: tttt") {
		t.Fatalf("newest entry lost after budget trim")
	}
	if len(s2.History) >= entries {
		t.Fatalf("oldest entries were not dropped (len=%d, was %d)", len(s2.History), entries)
	}
	_ = saved
}

// TestTrimKeepsAtLeastOneEntry: the budget loop must never evict the
// newest entry even when it alone exceeds the budget.
func TestTrimKeepsAtLeastOneEntry(t *testing.T) {
	s := &Session{}
	s.AddMessage("assistant", strings.Repeat("t", MaxMessageChars+50_000))
	s.trim() // single entry, over budget alone; must survive
	if len(s.History) != 1 {
		t.Fatalf("newest entry evicted: len=%d", len(s.History))
	}
}

// TestClampMessageCharsEdge: exact-cap and tiny-cap behavior.
func TestClampMessageCharsEdge(t *testing.T) {
	exact := strings.Repeat("e", MaxMessageChars)
	if got := clampMessageChars(exact, MaxMessageChars); got != exact {
		t.Fatalf("exact-cap content was modified")
	}
	tiny := clampMessageChars(strings.Repeat("y", 1000), 50)
	if len(tiny) != 50 {
		t.Fatalf("tiny cap: len=%d, want 50", len(tiny))
	}
	if got := clampMessageChars("short", 0); got != "short" {
		t.Fatalf("zero cap should disable clamping, got %q", got)
	}
}
