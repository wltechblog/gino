package session

import (
	"strings"
	"testing"
)

// TestCompactSessionEdgeKeeps verifies the floor guard: keep <= 0 must NOT
// trigger the old behavior (keep == 0 dropped ALL history; negative keeps
// were undefined). It is a hard error now, so a bad CLI/config value cannot
// silently destroy a session.
func TestCompactSessionEdgeKeeps(t *testing.T) {
	sm := newTestManager(t)
	s := sm.GetOrCreate("k1")
	addEntries(s, 6) // 12 entries
	if err := sm.Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	for _, keep := range []int{0, -3} {
		n, err := sm.CompactSession("k1", keep, func(old []string) (string, error) {
			t.Errorf("summarizer must not run for keep=%d", keep)
			return "SHOULD-NOT-APPEAR", nil
		})
		if err == nil {
			t.Fatalf("keep=%d: want error, got nil (n=%d)", keep, n)
		}
		if !strings.Contains(err.Error(), "keep") {
			t.Fatalf("keep=%d: error should mention keep, got %v", keep, err)
		}
	}

	// The session is untouched.
	n, ok := sm.CheckHistoryLength("k1")
	if !ok {
		t.Fatal("session vanished")
	}
	if n != 12 {
		t.Fatalf("want 12 history entries intact, got %d", n)
	}
}

// TestCompactSessionKeepExact verifies the keep == len boundary: everything
// is retained, no summary, summarizer never invoked.
func TestCompactSessionKeepExact(t *testing.T) {
	sm := newTestManager(t)
	s := sm.GetOrCreate("k2")
	addEntries(s, 5) // 10 entries
	if err := sm.Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	n, err := sm.CompactSession("k2", 10, func(old []string) (string, error) {
		t.Errorf("summarizer must not run when keep == len")
		return "SUM", nil
	})
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if n != 0 {
		t.Fatalf("want 0 summarized entries, got %d", n)
	}
	nHist, _ := sm.CheckHistoryLength("k2")
	if nHist != 10 {
		t.Fatalf("want 10 entries intact, got %d", nHist)
	}
}
