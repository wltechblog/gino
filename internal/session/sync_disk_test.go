package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestListByPrefixSeesExternalRename: a session file edited on disk AFTER the
// manager loaded (bulk rename by another process) must be visible in
// ListByPrefix without a restart. This is the exact production incident:
// 45 archives renamed on disk at 17:16; gateway (started 15:58) kept serving
// old titles from memory until restarted.
func TestListByPrefixSeesExternalRename(t *testing.T) {
	ws := t.TempDir()
	sm := NewSessionManager(ws)

	s := sm.GetOrCreate("telegram:1:archive:111")
	s.AddMessage("user", "hello")
	if err := sm.Save(s); err != nil {
		t.Fatal(err)
	}

	// Sanity: manager currently sees no title.
	if got := sm.ListByPrefix("telegram:1:archive:"); len(got) != 1 || got[0].Title != "" {
		t.Fatalf("pre: %+v", got)
	}

	// External edit: rewrite the file on disk with a title, bumping
	// UpdatedAt so the merge rule adopts it.
	time.Sleep(10 * time.Millisecond)
	fpath := filepath.Join(ws, "sessions", "telegram:1:archive:111.json")
	b, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatal(err)
	}
	// Marshal via a second manager to keep the format identical.
	probe := NewSessionManager(t.TempDir())
	ps := probe.GetOrCreate("telegram:1:archive:111")
	ps.AddMessage("user", "hello")
	ps.Title = "Bulk Renamed Title"
	ps.TitleSource = "manual"
	ps.CreatedAt = s.CreatedAt
	ps.UpdatedAt = time.Now() // strictly newer
	if err := probe.Save(ps); err != nil {
		t.Fatal(err)
	}
	pb, _ := os.ReadFile(filepath.Join(probe.workspace, "sessions", "telegram:1:archive:111.json"))
	_ = b
	if err := os.WriteFile(fpath, pb, 0644); err != nil {
		t.Fatal(err)
	}

	got := sm.ListByPrefix("telegram:1:archive:")
	if len(got) != 1 {
		t.Fatalf("post: got %d sessions", len(got))
	}
	if got[0].Title != "Bulk Renamed Title" {
		t.Fatalf("external rename not visible: title=%q", got[0].Title)
	}
}

// TestGetSeesExternalRename: Get (used by /session N restore and title copy)
// must also pick up disk edits.
func TestGetSeesExternalRename(t *testing.T) {
	ws := t.TempDir()
	sm := NewSessionManager(ws)

	s := sm.GetOrCreate("cli:one")
	s.AddMessage("user", "hi")
	sm.Save(s)

	// External edit with a title.
	probe := NewSessionManager(t.TempDir())
	ps := probe.GetOrCreate("cli:one")
	ps.AddMessage("user", "hi")
	ps.Title = "Renamed Externally"
	ps.TitleSource = "manual"
	ps.UpdatedAt = time.Now()
	probe.Save(ps)
	pb, _ := os.ReadFile(filepath.Join(probe.workspace, "sessions", "cli:one.json"))
	os.WriteFile(filepath.Join(ws, "sessions", "cli:one.json"), pb, 0644)

	g := sm.Get("cli:one")
	if g == nil || g.Title != "Renamed Externally" {
		t.Fatalf("Get did not see external rename: %+v", g)
	}
}

// TestSyncDoesNotClobberInFlightSession: an in-memory session being actively
// appended to (mid-turn, UpdatedAt bumped in memory, not yet saved) must NOT
// be replaced by an older disk copy.
func TestSyncDoesNotClobberInFlightSession(t *testing.T) {
	ws := t.TempDir()
	sm := NewSessionManager(ws)

	s := sm.GetOrCreate("cli:busy")
	s.AddMessage("user", "first")
	sm.Save(s) // disk now has UpdatedAt T1

	// Simulate mid-turn mutation: AddMessage bumps UpdatedAt in memory
	// past the disk copy, before Save.
	time.Sleep(10 * time.Millisecond)
	s.AddMessage("user", "second")

	got := sm.ListByPrefix("cli:")
	if len(got) != 1 {
		t.Fatalf("lost session during sync")
	}
	if len(got[0].History) != 2 {
		t.Fatalf("in-flight history clobbered: %d entries", len(got[0].History))
	}
}

// TestSetTitleFindsExternallyAddedSession: /title <N> after another process
// wrote a brand-new archive file must find it (SetTitle syncs first).
func TestSetTitleFindsExternallyAddedSession(t *testing.T) {
	ws := t.TempDir()
	sm := NewSessionManager(ws)

	// Another process writes an archive the manager has never seen.
	probe := NewSessionManager(t.TempDir())
	ps := probe.GetOrCreate("telegram:2:archive:222")
	ps.AddMessage("user", "external")
	ps.UpdatedAt = time.Now()
	probe.Save(ps)
	pb, _ := os.ReadFile(filepath.Join(probe.workspace, "sessions", "telegram:2:archive:222.json"))
	os.MkdirAll(filepath.Join(ws, "sessions"), 0755)
	os.WriteFile(filepath.Join(ws, "sessions", "telegram:2:archive:222.json"), pb, 0644)

	sm.SetTitle("telegram:2:archive:222", "Now In Memory", "manual")
	g := sm.Get("telegram:2:archive:222")
	if g == nil || g.Title != "Now In Memory" {
		t.Fatalf("SetTitle could not find external session: %+v", g)
	}
	// And the title persisted back to disk.
	d, err := os.ReadFile(filepath.Join(ws, "sessions", "telegram:2:archive:222.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(d, "Now In Memory") {
		t.Fatalf("title not persisted: %s", d)
	}
}

func contains(b []byte, sub string) bool {
	return len(sub) == 0 || (len(b) >= len(sub) && string(b[:len(sub)]) == sub || stringContains(b, sub))
}

func stringContains(b []byte, sub string) bool {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return true
		}
	}
	return false
}
