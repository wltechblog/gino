package cron

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Job represents a scheduled task.
type Job struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Message    string        `json:"message"`
	FireAt     time.Time     `json:"fire_at"`
	Channel    string        `json:"channel,omitempty"`
	ChatID     string        `json:"chat_id,omitempty"`
	SessionKey string        `json:"session_key,omitempty"`
	Recurring  bool          `json:"recurring,omitempty"`
	Interval   time.Duration `json:"interval,omitempty"`

	// Cron-expression scheduling (optional alternative to Interval-based recurring).
	Schedule string `json:"schedule,omitempty"` // raw cron expression, e.g. "*/15 9-16 * * 1-5"
	Timezone string `json:"timezone,omitempty"` // IANA timezone, e.g. "America/New_York"

	// Internal fields (not persisted).
	fired bool           `json:"-"`
	expr  *CronExpr      `json:"-"`
	loc   *time.Location `json:"-"`
}

// isCron reports whether this job uses cron-expression scheduling.
func (j *Job) isCron() bool {
	return j.Schedule != ""
}

// ensureCompiled parses and caches the cron expression and timezone.
// Safe to call multiple times; the result is cached on the Job.
func (j *Job) ensureCompiled() error {
	if j.Schedule == "" {
		return nil
	}
	if j.expr != nil {
		return nil // already compiled
	}

	expr, err := ParseCron(j.Schedule)
	if err != nil {
		return fmt.Errorf("cron: invalid schedule %q: %w", j.Schedule, err)
	}

	loc := time.UTC
	if j.Timezone != "" {
		l, err := time.LoadLocation(j.Timezone)
		if err != nil {
			log.Printf("cron: warning: unknown timezone %q for job %q, using UTC", j.Timezone, j.Name)
		} else {
			loc = l
		}
	}

	j.expr = expr
	j.loc = loc
	return nil
}

// computeNext computes the next fire time after `after` using the cron expression.
// Must not be called on non-cron jobs.
func (j *Job) computeNext(after time.Time) (time.Time, error) {
	if err := j.ensureCompiled(); err != nil {
		return time.Time{}, err
	}
	return j.expr.NextAfter(after, j.loc)
}

// FireCallback is called when a job fires. The scheduler passes the job details.
type FireCallback func(job Job)

// Scheduler manages scheduled jobs and fires them when due.
// Jobs are persisted to disk so they survive restarts.
type Scheduler struct {
	mu       sync.Mutex
	jobs     map[string]*Job
	callback FireCallback
	nextID   int
	running  bool
	filePath string // if set, jobs are persisted here
}

// NewScheduler creates a new scheduler with the given fire callback.
func NewScheduler(callback FireCallback) *Scheduler {
	return &Scheduler{
		jobs:     make(map[string]*Job),
		callback: callback,
	}
}

// SetPersistencePath enables disk persistence at the given file path.
// Must be called before Start(). Existing jobs are loaded immediately.
func (s *Scheduler) SetPersistencePath(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filePath = path
	return s.loadLocked()
}

// Add registers a one-time job. Returns the job ID. The optional sessionKey
// routes the fired reminder into the exact session that scheduled it
// (project- and user-namespaced sessions included).
func (s *Scheduler) Add(name, message string, delay time.Duration, channel, chatID string, sessionKey ...string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("job-%d", s.nextID)
	s.jobs[id] = &Job{
		ID:         id,
		Name:       name,
		Message:    message,
		FireAt:     time.Now().Add(delay),
		Channel:    channel,
		ChatID:     chatID,
		SessionKey: firstNonEmpty(sessionKey),
	}
	log.Printf("cron: scheduled job %q (%s) to fire in %v", name, id, delay)
	s.saveLocked()
	return id
}

// AddRecurring schedules a recurring job. Returns the job ID.
func (s *Scheduler) AddRecurring(name, message string, interval time.Duration, channel, chatID string, sessionKey ...string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("job-%d", s.nextID)
	s.jobs[id] = &Job{
		ID:         id,
		Name:       name,
		Message:    message,
		FireAt:     time.Now().Add(interval),
		Channel:    channel,
		ChatID:     chatID,
		SessionKey: firstNonEmpty(sessionKey),
		Recurring:  true,
		Interval:   interval,
	}
	log.Printf("cron: scheduled recurring job %q (%s) every %v", name, id, interval)
	s.saveLocked()
	return id
}

// AddScheduled creates a cron-expression-based recurring job.
// The cron expression determines when the job fires.
// The timezone determines how the expression is interpreted.
// Returns the job ID or an error if the expression is invalid.
func (s *Scheduler) AddScheduled(name, message, cronExpr, timezone, channel, chatID string, sessionKey ...string) (string, error) {
	// Validate the expression before adding.
	parsed, err := ParseCron(cronExpr)
	if err != nil {
		return "", err
	}

	loc := time.UTC
	if timezone != "" {
		loc, err = time.LoadLocation(timezone)
		if err != nil {
			return "", fmt.Errorf("cron: invalid timezone %q: %w", timezone, err)
		}
	}

	// Compute the first fire time.
	now := time.Now()
	next, err := parsed.NextAfter(now, loc)
	if err != nil {
		return "", fmt.Errorf("cron: could not compute next fire time: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("job-%d", s.nextID)
	s.jobs[id] = &Job{
		ID:        id,
		Name:      name,
		Message:   message,
		FireAt:    next,
		Channel:   channel,
		ChatID:    chatID,
		Recurring: true, // cron jobs are always recurring
		Schedule:  cronExpr,
		Timezone:  timezone,
		expr:      parsed,
		loc:       loc,
	}
	log.Printf("cron: scheduled cron job %q (%s): %q (tz: %s), next fire at %s", name, id, cronExpr, timezone, next.Format(time.RFC3339))
	s.saveLocked()
	return id, nil
}

// Cancel removes a job by ID. Returns true if found.
func (s *Scheduler) Cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; ok {
		delete(s.jobs, id)
		log.Printf("cron: cancelled job %s", id)
		s.saveLocked()
		return true
	}
	return false
}

// CancelByName removes a job by name. Returns true if found.
func (s *Scheduler) CancelByName(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, j := range s.jobs {
		if j.Name == name {
			delete(s.jobs, id)
			log.Printf("cron: cancelled job %q (%s)", name, id)
			s.saveLocked()
			return true
		}
	}
	return false
}

// List returns all pending jobs.
func (s *Scheduler) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		result = append(result, *j)
	}
	return result
}

// Start begins the scheduler tick loop. Call in a goroutine.
func (s *Scheduler) Start(done <-chan struct{}) {
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	log.Println("cron: scheduler started")
	for {
		select {
		case <-done:
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
			log.Println("cron: scheduler stopped")
			return
		case now := <-ticker.C:
			s.tick(now)
		}
	}
}

// tick checks all jobs and fires any that are due.
func (s *Scheduler) tick(now time.Time) {
	s.mu.Lock()
	// collect jobs to fire
	var toFire []*Job
	for _, j := range s.jobs {
		if !j.fired && now.After(j.FireAt) {
			toFire = append(toFire, j)
		}
	}
	// handle fired jobs while still holding lock
	for _, j := range toFire {
		if j.isCron() {
			// Cron job: compute next fire time from now.
			next, err := j.computeNext(now)
			if err != nil {
				log.Printf("cron: error computing next fire time for %q (%s): %v — removing", j.Name, j.ID, err)
				delete(s.jobs, j.ID)
			} else {
				j.FireAt = next
			}
		} else if j.Recurring {
			// Interval-based recurring job.
			j.FireAt = now.Add(j.Interval)
		} else {
			// One-time job — fire and delete.
			j.fired = true
			delete(s.jobs, j.ID)
		}
	}
	if len(toFire) > 0 {
		s.saveLocked()
	}
	s.mu.Unlock()

	// fire callbacks outside lock
	for _, j := range toFire {
		log.Printf("cron: firing job %q (%s): %s", j.Name, j.ID, j.Message)
		if s.callback != nil {
			s.callback(*j)
		}
	}
}

// ─── Persistence ────────────────────────────────────────────────────────────

// persistedState is the JSON representation of saved scheduler state.
// nextID is stored to avoid ID collisions across restarts.
type persistedState struct {
	NextID int   `json:"next_id"`
	Jobs   []Job `json:"jobs"`
}

// loadLocked reads jobs from disk. Caller must hold s.mu.
func (s *Scheduler) loadLocked() error {
	if s.filePath == "" {
		return nil
	}

	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // first run — no jobs file yet
		}
		return fmt.Errorf("cron: read jobs file: %w", err)
	}

	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("cron: unmarshal jobs file: %w", err)
	}

	s.nextID = state.NextID
	now := time.Now()
	restored := 0
	dropped := 0

	for _, j := range state.Jobs {
		// Deep copy since j is a range variable.
		job := j

		if job.isCron() {
			// Cron job: recompute next fire time from now.
			// This avoids firing a backlog of missed intervals.
			if err := job.ensureCompiled(); err != nil {
				log.Printf("cron: dropping job %q (%s): invalid expression on reload: %v", job.Name, job.ID, err)
				dropped++
				continue
			}
			next, err := job.computeNext(now)
			if err != nil {
				log.Printf("cron: dropping job %q (%s): cannot compute next fire: %v", job.Name, job.ID, err)
				dropped++
				continue
			}
			job.FireAt = next
			s.jobs[job.ID] = &job
			restored++
		} else if job.Recurring {
			// Interval-based recurring job: reschedule to next interval from now.
			job.FireAt = now.Add(job.Interval)
			s.jobs[job.ID] = &job
			restored++
		} else {
			// One-time job — only keep if not yet due.
			if job.FireAt.After(now) {
				s.jobs[job.ID] = &job
				restored++
			} else {
				dropped++
			}
		}
	}

	if restored > 0 || dropped > 0 {
		log.Printf("cron: loaded %d job(s) from disk", restored)
		if dropped > 0 {
			log.Printf("cron: dropped %d expired or invalid job(s)", dropped)
		}
	}

	// Re-normalize nextID in case loaded jobs have higher IDs.
	for _, j := range s.jobs {
		var num int
		if _, err := fmt.Sscanf(j.ID, "job-%d", &num); err == nil && num >= s.nextID {
			s.nextID = num + 1
		}
	}

	// Save to clean up dropped jobs from the file.
	s.saveLocked()
	return nil
}

// saveLocked writes jobs to disk. Caller must hold s.mu.
func (s *Scheduler) saveLocked() {
	if s.filePath == "" {
		return
	}

	state := persistedState{
		NextID: s.nextID,
		Jobs:   make([]Job, 0, len(s.jobs)),
	}
	for _, j := range s.jobs {
		state.Jobs = append(state.Jobs, *j)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("cron: marshal jobs file: %v", err)
		return
	}

	// Write atomically: temp file + rename.
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("cron: write jobs file: %v", err)
		return
	}
	if err := os.Rename(tmp, s.filePath); err != nil {
		log.Printf("cron: rename jobs file: %v", err)
		return
	}

	// Ensure parent dir exists (belt and suspenders).
	_ = os.MkdirAll(filepath.Dir(s.filePath), 0700)
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(ss []string) string {
	for _, v := range ss {
		if v != "" {
			return v
		}
	}
	return ""
}
