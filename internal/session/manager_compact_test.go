package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *SessionManager {
	t.Helper()
	return NewSessionManager(t.TempDir())
}

func addEntries(s *Session, n int) {
	for i := 0; i < n; i++ {
		s.AddMessage("user", "u"+strings.Repeat("x", i%7)+itoa(i))
		s.AddMessage("assistant", "a"+itoa(i))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestCompactSessionSummarizesOldEntries(t *testing.T) {
	sm := newTestManager(t)
	s := sm.GetOrCreate("test:key")
	addEntries(s, 15) // 30 entries
	if err := sm.Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	const keep = 10
	n, err := sm.CompactSession("test:key", keep, func(old []string) (string, error) {
		if len(old) != 30-keep {
			t.Errorf("summarize got %d entries, want %d", len(old), 30-keep)
		}
		return "TEST-SUMMARY user asked about backup pool", nil
	})
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if n != 30-keep {
		t.Errorf("returned %d summarized entries, want %d", n, 30-keep)
	}

	hist, ok := sm.CheckHistoryLength("test:key")
	if !ok {
		t.Fatal("session vanished")
	}
	if hist != keep+1 {
		t.Errorf("history length %d, want %d (summary + kept)", hist, keep+1)
	}

	got := sm.Get("test:key")
	if !strings.Contains(got.History[0], "TEST-SUMMARY") {
		t.Errorf("first entry missing summary: %q", got.History[0])
	}
	if !strings.Contains(got.History[0], "Earlier conversation summary") {
		t.Errorf("first entry missing marker: %q", got.History[0])
	}
	last := got.History[len(got.History)-1]
	if !strings.Contains(last, "a14") {
		t.Errorf("newest entry not preserved; last=%q", last)
	}
}

func TestCompactSessionNothingToDo(t *testing.T) {
	sm := newTestManager(t)
	s := sm.GetOrCreate("short")
	s.AddMessage("user", "hi")
	sm.Save(s)

	called := false
	n, err := sm.CompactSession("short", 16, func([]string) (string, error) {
		called = true
		return "x", nil
	})
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if n != 0 || called {
		t.Errorf("expected no-op, got n=%d called=%v", n, called)
	}
}

func TestCompactSessionUnknownKey(t *testing.T) {
	sm := newTestManager(t)
	if _, err := sm.CompactSession("nope", 4, func([]string) (string, error) { return "x", nil }); err != os.ErrNotExist {
		t.Errorf("want os.ErrNotExist, got %v", err)
	}
}

func TestCompactSessionPreservesConcurrentAppends(t *testing.T) {
	sm := newTestManager(t)
	s := sm.GetOrCreate("race")
	addEntries(s, 12) // 24 entries
	sm.Save(s)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Simulates a turn finishing while the summarizer LLM call runs.
		time.Sleep(20 * time.Millisecond)
		cur := sm.GetOrCreate("race")
		cur.AddMessage("user", "LATE-ARRIVAL")
		sm.Save(cur)
	}()

	_, err := sm.CompactSession("race", 4, func(old []string) (string, error) {
		wg.Wait() // ensure the append happens mid-compaction
		return "RACE-SUMMARY", nil
	})
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}

	got := sm.Get("race")
	if len(got.History) != 4+2 { // summary + kept 4 + late user entry
		t.Fatalf("history length %d, want 6; hist=%v", len(got.History), got.History)
	}
	if !strings.Contains(got.History[0], "RACE-SUMMARY") {
		t.Errorf("summary missing from first entry: %q", got.History[0])
	}
	if got.History[len(got.History)-1] != "user: LATE-ARRIVAL" {
		t.Errorf("late append lost; last=%q", got.History[len(got.History)-1])
	}
}

func TestCheckHistoryLength(t *testing.T) {
	sm := newTestManager(t)
	if _, ok := sm.CheckHistoryLength("missing"); ok {
		t.Error("missing session reported as existing")
	}
	s := sm.GetOrCreate("k")
	s.AddMessage("user", "x")
	sm.Save(s)
	if n, ok := sm.CheckHistoryLength("k"); !ok || n != 1 {
		t.Errorf("got n=%d ok=%v, want 1 true", n, ok)
	}
}

func TestCompactSessionEmptySummary(t *testing.T) {
	sm := newTestManager(t)
	s := sm.GetOrCreate("e")
	addEntries(s, 10)
	sm.Save(s)

	before := sm.Get("e").History
	_, err := sm.CompactSession("e", 4, func([]string) (string, error) {
		return "", nil
	})
	if err == nil {
		t.Fatal("expected error on empty summary")
	}
	// History must be untouched on failure.
	if strings.Join(sm.Get("e").History, "|") != strings.Join(before, "|") {
		t.Error("history changed despite failed compaction")
	}
	_ = filepath.Join // keep filepath import if unused elsewhere
}
