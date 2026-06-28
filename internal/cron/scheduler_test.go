package cron

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSchedulerPersistence(t *testing.T) {
	dir := t.TempDir()
	jobsFile := filepath.Join(dir, "jobs.json")

	// Create scheduler, add jobs, persist.
	s1 := NewScheduler(nil)
	if err := s1.SetPersistencePath(jobsFile); err != nil {
		t.Fatalf("SetPersistencePath: %v", err)
	}
	s1.Add("one-time", "do something", 1*time.Hour, "telegram", "1")
	s1.AddRecurring("recurring", "check stuff", 30*time.Minute, "telegram", "2")

	// Simulate restart: new scheduler loads from same file.
	s2 := NewScheduler(nil)
	if err := s2.SetPersistencePath(jobsFile); err != nil {
		t.Fatalf("reload SetPersistencePath: %v", err)
	}

	jobs := s2.List()
	if len(jobs) != 2 {
		t.Fatalf("expected 2 restored jobs, got %d", len(jobs))
	}

	names := map[string]bool{}
	for _, j := range jobs {
		names[j.Name] = true
	}
	if !names["one-time"] || !names["recurring"] {
		t.Errorf("expected both 'one-time' and 'recurring' jobs, got %v", names)
	}
}

func TestSchedulerPersistenceDropsExpired(t *testing.T) {
	dir := t.TempDir()
	jobsFile := filepath.Join(dir, "jobs.json")

	// Add a job that's already due.
	s1 := NewScheduler(nil)
	_ = s1.SetPersistencePath(jobsFile)
	s1.Add("past-job", "expired", -1*time.Second, "telegram", "1")
	// Manually close done channel pattern not needed — just reload.

	s2 := NewScheduler(nil)
	if err := s2.SetPersistencePath(jobsFile); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// The expired job should have been loaded (save happens on Add before FireAt check).
	// After loadLocked, expired one-time jobs are dropped.
	jobs := s2.List()
	if len(jobs) != 0 {
		t.Errorf("expected 0 jobs after reload (expired dropped), got %d", len(jobs))
	}
}

func TestSchedulerPersistenceRecurringReschedules(t *testing.T) {
	dir := t.TempDir()
	jobsFile := filepath.Join(dir, "jobs.json")

	s1 := NewScheduler(nil)
	_ = s1.SetPersistencePath(jobsFile)
	// Add recurring job with short interval.
	s1.AddRecurring("every-2m", "ping", 2*time.Minute, "telegram", "1")

	// Reload.
	s2 := NewScheduler(nil)
	if err := s2.SetPersistencePath(jobsFile); err != nil {
		t.Fatalf("reload: %v", err)
	}

	jobs := s2.List()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 recurring job, got %d", len(jobs))
	}
	if !jobs[0].Recurring {
		t.Error("expected job to be recurring")
	}
	// FireAt should be in the future (rescheduled from now).
	if !jobs[0].FireAt.After(time.Now()) {
		t.Error("expected recurring job FireAt to be in the future after reload")
	}
}

func TestSchedulerPersistenceFileFormat(t *testing.T) {
	dir := t.TempDir()
	jobsFile := filepath.Join(dir, "jobs.json")

	s1 := NewScheduler(nil)
	_ = s1.SetPersistencePath(jobsFile)
	s1.Add("test-job", "hello world", 5*time.Minute, "telegram", "99")

	// Verify file exists and is valid JSON with expected fields.
	data, err := os.ReadFile(jobsFile)
	if err != nil {
		t.Fatalf("read jobs file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("jobs file is empty")
	}
	// Should contain the job name and message.
	str := string(data)
	if !contains(str, "test-job") {
		t.Error("jobs file should contain job name")
	}
	if !contains(str, "hello world") {
		t.Error("jobs file should contain job message")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && (s[0:len(substr)] == substr || contains(s[1:], substr)))
}

func TestSchedulerFiresAfterReload(t *testing.T) {
	dir := t.TempDir()
	jobsFile := filepath.Join(dir, "jobs.json")

	// Phase 1: create and add a short job, let it save.
	s1 := NewScheduler(nil)
	_ = s1.SetPersistencePath(jobsFile)
	s1.Add("quick", "fire fast", 50*time.Millisecond, "telegram", "1")
	// Don't start the scheduler — just persist.
	time.Sleep(100 * time.Millisecond) // let it expire on disk without firing

	// Phase 2: reload — the job should be expired and dropped.
	var mu sync.Mutex
	var fired []Job
	s2 := NewScheduler(func(job Job) {
		mu.Lock()
		fired = append(fired, job)
		mu.Unlock()
	})
	if err := s2.SetPersistencePath(jobsFile); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// The expired one-time job should not fire.
	done := make(chan struct{})
	go s2.Start(done)
	time.Sleep(500 * time.Millisecond)
	close(done)

	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 0 {
		t.Errorf("expected 0 fired jobs (expired), got %d", len(fired))
	}
}
