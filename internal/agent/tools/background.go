package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wltechblog/gino/internal/chat"
)

// ── Limits ──────────────────────────────────────────────────────────────────

const (
	bgMaxOneShot      = 8                // concurrent one-shot jobs
	bgMaxPollers      = 8                // registered pollers
	bgMinPollInterval = time.Minute      // minimum poll interval
	bgDefaultTimeout  = 10 * time.Minute // default one-shot timeout
	bgMaxTimeout      = 24 * time.Hour   // maximum one-shot timeout
	bgDefaultRunTO    = time.Minute      // default per-run poll timeout
	bgTailBytes       = 4096             // output tail kept in notifications
)

// ── Job types ───────────────────────────────────────────────────────────────

// bgOneShot is a single long-running command that reports its result once.
type bgOneShot struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Argv           []string      `json:"argv"`
	Cwd            string        `json:"cwd,omitempty"`
	Timeout        time.Duration `json:"timeout"`
	Channel        string        `json:"channel"`
	ChatID         string        `json:"chat_id"`
	RerunOnRestart bool          `json:"rerun_on_restart,omitempty"`
	StartedAt      time.Time     `json:"started_at"`

	cancel context.CancelFunc
	done   chan struct{}
}

// bgPoller runs a command on an interval and signals on change/failure/always.
type bgPoller struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Argv     []string      `json:"argv"`
	Cwd      string        `json:"cwd,omitempty"`
	Interval time.Duration `json:"interval"`
	RunTO    time.Duration `json:"run_timeout"`
	NotifyOn string        `json:"notify_on"`          // change | failure | always
	MaxRuns  int           `json:"max_runs,omitempty"` // 0 = unlimited
	Runs     int           `json:"runs"`
	Channel  string        `json:"channel"`
	ChatID   string        `json:"chat_id"`

	lastHash string
	lastFail bool
	started  bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

// ── Tool ────────────────────────────────────────────────────────────────────

// BackgroundTool lets the agent run commands out-of-band and get the result
// back as a signal. One-shot jobs cover long-running commands that exceed the
// exec tool timeout; pollers cover periodic checks that should only wake the
// agent when something happens. Deliveries are injected into the hub with
// signal metadata, so they use the "signal:" session namespace and never
// cancel the user's active interactive turn.
//
// Commands are validated against the exec tool's sandbox policy at
// registration time (and again when restored from disk after a restart).
type BackgroundTool struct {
	mu          sync.Mutex
	oneShots    map[string]*bgOneShot
	pollers     map[string]*bgPoller
	hub         *chat.Hub
	execTool    *ExecTool
	persistPath string
	nextID      int

	// channel/chatID captured per incoming message (used at registration time)
	channel string
	chatID  string

	minInterval time.Duration // test hook; defaults to bgMinPollInterval
}

func NewBackgroundTool(hub *chat.Hub, execTool *ExecTool) *BackgroundTool {
	return &BackgroundTool{
		oneShots:    make(map[string]*bgOneShot),
		pollers:     make(map[string]*bgPoller),
		hub:         hub,
		execTool:    execTool,
		nextID:      1,
		minInterval: bgMinPollInterval,
	}
}

func (t *BackgroundTool) Name() string { return "background" }

func (t *BackgroundTool) Description() string {
	return "Run shell commands in the background and get notified with the result — for jobs that exceed the exec tool timeout or periodic checks. " +
		"Actions: start (one-shot long job, notified on completion with output tail), poll (recurring command, notified on output change / failure / every run), list (show active jobs), cancel (stop a job by name). " +
		"Notifications arrive as new messages in the same chat, so the user is informed without polling. Uses the same sandbox policy as exec."
}

func (t *BackgroundTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"description": "The action: start (one-shot job), poll (recurring check), list (active jobs), cancel (stop a job)",
				"enum":        []string{"start", "poll", "list", "cancel"},
			},
			"name": map[string]interface{}{
				"type":        "string",
				"description": "A short name for the job (used to cancel it later)",
			},
			"cmd": map[string]interface{}{
				"type":        "array",
				"description": "Command as array [program, arg1, arg2, ...] (same rules as the exec tool)",
				"items":       map[string]interface{}{"type": "string"},
				"minItems":    1,
			},
			"cwd": map[string]interface{}{
				"type":        "string",
				"description": "Working directory (must be within an allowed directory). Defaults to the workspace root.",
			},
			// --- start ---
			"timeout": map[string]interface{}{
				"type":        "string",
				"description": "[start] Maximum run time, Go duration format (e.g. '30m', '2h'). Default 10m, max 24h. The job is killed and reported as timed out past this.",
			},
			"rerunOnRestart": map[string]interface{}{
				"type":        "boolean",
				"description": "[start] If true, the job is relaunched when the agent process restarts before it completes. Default false (the chat is notified the job was interrupted).",
			},
			// --- poll ---
			"interval": map[string]interface{}{
				"type":        "string",
				"description": "[poll] How often to run the command, Go duration format. Minimum 60s. Example: '5m'.",
			},
			"notifyOn": map[string]interface{}{
				"type":        "string",
				"description": "[poll] When to notify: 'change' (output differs from previous run — default), 'failure' (non-zero exit; also notifies on recovery), 'always' (every run)",
				"enum":        []string{"change", "failure", "always"},
			},
			"maxRuns": map[string]interface{}{
				"type":        "integer",
				"description": "[poll] Auto-cancel after N runs (0 = unlimited).",
			},
		},
		"required": []string{"action"},
	}
}

// SetContext records the originating channel/chat for newly registered jobs.
func (t *BackgroundTool) SetContext(channel, chatID string) {
	t.mu.Lock()
	t.channel = channel
	t.chatID = chatID
	t.mu.Unlock()
}

// ── Execute ─────────────────────────────────────────────────────────────────

func (t *BackgroundTool) Execute(_ context.Context, args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)

	switch action {
	case "start":
		return t.executeStart(args)
	case "poll":
		return t.executePoll(args)
	case "list":
		return t.executeList()
	case "cancel":
		return t.executeCancel(args)
	default:
		return "", fmt.Errorf("background: unknown action %q (use start, poll, list, or cancel)", action)
	}
}

func (t *BackgroundTool) executeStart(args map[string]interface{}) (string, error) {
	name, _ := args["name"].(string)
	if name == "" {
		name = "job"
	}
	argv, err := bgParseArgv(args)
	if err != nil {
		return "", err
	}
	cwd, _ := args["cwd"].(string)

	timeout := bgDefaultTimeout
	if s, ok := args["timeout"].(string); ok && s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return "", fmt.Errorf("background start: invalid timeout %q: %v", s, err)
		}
		if d <= 0 || d > bgMaxTimeout {
			return "", fmt.Errorf("background start: timeout must be between 1s and 24h (got %v)", d)
		}
		timeout = d
	}
	rerun, _ := args["rerunOnRestart"].(bool)

	if err := t.validate(argv, cwd); err != nil {
		return "", fmt.Errorf("background start: %v", err)
	}

	t.mu.Lock()
	if len(t.oneShots) >= bgMaxOneShot {
		t.mu.Unlock()
		return "", fmt.Errorf("background start: %d one-shot jobs already running (max %d) — wait for one to finish or cancel one", len(t.oneShots), bgMaxOneShot)
	}
	channel, chatID := t.channel, t.chatID
	id := fmt.Sprintf("bg%d", t.nextID)
	t.nextID++
	j := &bgOneShot{
		ID: id, Name: name, Argv: argv, Cwd: cwd, Timeout: timeout,
		Channel: channel, ChatID: chatID, RerunOnRestart: rerun,
		StartedAt: time.Now(),
	}
	t.oneShots[id] = j
	t.persistLocked()
	t.mu.Unlock()

	t.launchOneShot(j)

	toDesc := ""
	if timeout != bgDefaultTimeout {
		toDesc = fmt.Sprintf(" (max %v)", timeout)
	}
	return fmt.Sprintf("Background job %q (id %s) started%s. I'll report the result here when it completes.", name, id, toDesc), nil
}

func (t *BackgroundTool) executePoll(args map[string]interface{}) (string, error) {
	name, _ := args["name"].(string)
	if name == "" {
		name = "poll"
	}
	argv, err := bgParseArgv(args)
	if err != nil {
		return "", err
	}
	cwd, _ := args["cwd"].(string)

	intervalStr, _ := args["interval"].(string)
	if intervalStr == "" {
		return "", fmt.Errorf("background poll: 'interval' is required (e.g. '5m', minimum 60s)")
	}
	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		return "", fmt.Errorf("background poll: invalid interval %q: %v", intervalStr, err)
	}
	t.mu.Lock()
	minInt := t.minInterval
	t.mu.Unlock()
	if interval < minInt {
		return "", fmt.Errorf("background poll: interval must be at least %v (got %v)", minInt, interval)
	}

	notifyOn := "change"
	if s, ok := args["notifyOn"].(string); ok && s != "" {
		if s != "change" && s != "failure" && s != "always" {
			return "", fmt.Errorf("background poll: notifyOn must be change, failure, or always (got %q)", s)
		}
		notifyOn = s
	}

	maxRuns := 0
	switch v := args["maxRuns"].(type) {
	case float64:
		if v > 0 {
			maxRuns = int(v)
		}
	case int:
		if v > 0 {
			maxRuns = v
		}
	}

	if err := t.validate(argv, cwd); err != nil {
		return "", fmt.Errorf("background poll: %v", err)
	}

	t.mu.Lock()
	if len(t.pollers) >= bgMaxPollers {
		t.mu.Unlock()
		return "", fmt.Errorf("background poll: %d pollers already registered (max %d) — cancel one first", len(t.pollers), bgMaxPollers)
	}
	channel, chatID := t.channel, t.chatID
	id := fmt.Sprintf("bg%d", t.nextID)
	t.nextID++
	p := &bgPoller{
		ID: id, Name: name, Argv: argv, Cwd: cwd,
		Interval: interval, RunTO: bgDefaultRunTO,
		NotifyOn: notifyOn, MaxRuns: maxRuns,
		Channel: channel, ChatID: chatID,
		stopCh: make(chan struct{}),
	}
	t.pollers[id] = p
	t.persistLocked()
	t.mu.Unlock()

	go t.runPoller(p)

	return fmt.Sprintf("Poller %q (id %s) registered: runs every %v, notifies on %s. Survives restarts until cancelled.", name, id, interval, notifyOn), nil
}

func (t *BackgroundTool) executeList() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.oneShots) == 0 && len(t.pollers) == 0 {
		return "No background jobs.", nil
	}
	var sb strings.Builder
	if len(t.oneShots) > 0 {
		fmt.Fprintf(&sb, "Running one-shot jobs (%d):\n", len(t.oneShots))
		for _, j := range t.oneShots {
			fmt.Fprintf(&sb, "- %s (id %s): %q — running for %v (timeout %v)\n", j.Name, j.ID, bgCmdString(j.Argv), time.Since(j.StartedAt).Round(time.Second), j.Timeout)
		}
	}
	if len(t.pollers) > 0 {
		fmt.Fprintf(&sb, "Pollers (%d):\n", len(t.pollers))
		for _, p := range t.pollers {
			max := "unlimited"
			if p.MaxRuns > 0 {
				max = fmt.Sprintf("%d runs max", p.MaxRuns)
			}
			fmt.Fprintf(&sb, "- %s (id %s): %q — every %v, notify on %s, %d runs so far (%s)\n", p.Name, p.ID, bgCmdString(p.Argv), p.Interval, p.NotifyOn, p.Runs, max)
		}
	}
	return sb.String(), nil
}

func (t *BackgroundTool) executeCancel(args map[string]interface{}) (string, error) {
	target, _ := args["name"].(string)
	if target == "" {
		return "", fmt.Errorf("background cancel: 'name' is required (job name or id)")
	}
	t.mu.Lock()
	// pollers first
	for _, p := range t.pollers {
		if p.Name == target || p.ID == target {
			delete(t.pollers, p.ID)
			t.persistLocked()
			t.mu.Unlock()
			p.stopOnce.Do(func() { close(p.stopCh) })
			return fmt.Sprintf("Cancelled poller %q (%s).", p.Name, p.ID), nil
		}
	}
	for _, j := range t.oneShots {
		if j.Name == target || j.ID == target {
			delete(t.oneShots, j.ID)
			t.persistLocked()
			t.mu.Unlock()
			if j.cancel != nil {
				j.cancel() // ctx.Canceled → the completion report says "cancelled"
			}
			return fmt.Sprintf("Cancelled job %q (%s) — the process was killed.", j.Name, j.ID), nil
		}
	}
	t.mu.Unlock()
	return "", fmt.Errorf("background cancel: no job or poller named %q", target)
}

// ── Internals ───────────────────────────────────────────────────────────────

// validate applies the exec tool's sandbox policy to a background command.
func (t *BackgroundTool) validate(argv []string, cwd string) error {
	if t.execTool == nil {
		return nil
	}
	return t.execTool.Validate(argv, cwd)
}

// runArgv executes argv via sh -c (same PATH resolution as the exec tool).
// Output is captured into a fixed-size tail buffer (a 24h job may produce
// gigabytes; only the last bgTailBytes are kept for reporting). Returns the
// output tail, total bytes produced, and exit code. A non-nil err means the
// command could not run at all.
func (t *BackgroundTool) runArgv(ctx context.Context, argv []string, cwd string) (tail string, total int64, exitCode int, err error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", shellJoin(argv))
	if cwd != "" {
		cmd.Dir = cwd
	}
	// Run the command in its own process group so a timeout/cancel kills the
	// whole tree (sh plus any children it spawned). Killing only the direct
	// child leaves grandchildren alive holding the output pipe open, which
	// blocks Run() until they exit on their own.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			// Negative pid = signal the entire process group.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return cmd.Process.Kill()
	}
	// Belt and braces: even if something still holds the pipes open, Run()
	// returns shortly after the context fires instead of blocking forever.
	cmd.WaitDelay = 5 * time.Second
	var w bgTailWriter
	cmd.Stdout, cmd.Stderr = &w, &w
	runErr := cmd.Run()
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			return w.String(), w.total, ee.ExitCode(), nil
		}
		return w.String(), w.total, -1, runErr
	}
	return w.String(), w.total, 0, nil
}

// bgTailWriter keeps only the last bgTailBytes written, plus a total count.
type bgTailWriter struct {
	buf   []byte
	total int64
}

func (w *bgTailWriter) Write(p []byte) (int, error) {
	w.total += int64(len(p))
	w.buf = append(w.buf, p...)
	if len(w.buf) > bgTailBytes {
		w.buf = w.buf[len(w.buf)-bgTailBytes:]
	}
	return len(p), nil
}

func (w *bgTailWriter) String() string { return string(w.buf) }

// notify injects a completion/update message into the hub with signal
// metadata so it lands in the signal session namespace (never cancels an
// active interactive turn) and always produces a visible reply.
func (t *BackgroundTool) notify(channel, chatID, content string) {
	if t.hub == nil {
		return
	}
	msg := chat.Inbound{
		Channel:   channel,
		SenderID:  "background",
		ChatID:    chatID,
		Content:   content,
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"signal_action": "background_job",
			"signal_source": "background-manager",
			"signal_silent": false,
		},
	}
	select {
	case t.hub.In <- msg:
	case <-time.After(5 * time.Second):
		log.Printf("background: hub inbound full, dropping notification for %s:%s", channel, chatID)
	}
}

// launchOneShot runs the job to completion in its own context (detached from
// the registering turn) and notifies the originating chat.
func (t *BackgroundTool) launchOneShot(j *bgOneShot) {
	ctx, cancel := context.WithTimeout(context.Background(), j.Timeout)
	j.cancel = cancel
	j.done = make(chan struct{})

	go func() {
		defer close(j.done)
		defer cancel()
		start := time.Now()
		tail, total, code, runErr := t.runArgv(ctx, j.Argv, j.Cwd)
		dur := time.Since(start).Round(time.Second)

		var sb strings.Builder
		fmt.Fprintf(&sb, "[Background job finished] %q (id %s)\n", j.Name, j.ID)
		fmt.Fprintf(&sb, "Command: %s\n", bgCmdString(j.Argv))
		switch {
		case ctx.Err() == context.Canceled:
			fmt.Fprintf(&sb, "Status: cancelled\n")
		case runErr != nil:
			fmt.Fprintf(&sb, "Status: could not run — %v\n", runErr)
		case ctx.Err() == context.DeadlineExceeded:
			fmt.Fprintf(&sb, "Status: timed out after %v\n", j.Timeout)
		case code == 0:
			fmt.Fprintf(&sb, "Status: success (exit 0)\nDuration: %v\n", dur)
		default:
			fmt.Fprintf(&sb, "Status: failed (exit %d)\nDuration: %v\n", code, dur)
		}
		sb.WriteString(bgOutputSection(tail, total))

		t.notify(j.Channel, j.ChatID, sb.String()+"(Relay this result to the user, summarizing it briefly.)")

		t.mu.Lock()
		// If cancelled via executeCancel the entry is already gone.
		if _, still := t.oneShots[j.ID]; still {
			delete(t.oneShots, j.ID)
			t.persistLocked()
		}
		t.mu.Unlock()
	}()
}

// runPoller ticks the command and decides whether to notify.
func (t *BackgroundTool) runPoller(p *bgPoller) {
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
		}

		ctx, cancel := context.WithTimeout(context.Background(), p.RunTO)
		out, total, code, runErr := t.runArgv(ctx, p.Argv, p.Cwd)
		cancel()

		t.mu.Lock()
		if _, alive := t.pollers[p.ID]; !alive {
			t.mu.Unlock()
			return // cancelled while running
		}
		p.Runs++
		notify := false
		var event string
		switch p.NotifyOn {
		case "always":
			notify, event = true, "update"
		case "failure":
			failed := runErr != nil || code != 0
			if failed && !p.lastFail {
				notify, event = true, "failure"
			} else if !failed && p.lastFail && p.started {
				notify, event = true, "recovery"
			}
			p.lastFail = failed
		default: // change
			h := bgHash(out)
			if p.started && h != p.lastHash {
				notify, event = true, "change"
			}
			p.lastHash = h
		}
		p.started = true
		done := p.MaxRuns > 0 && p.Runs >= p.MaxRuns
		if done {
			delete(t.pollers, p.ID)
		}
		t.persistLocked()
		t.mu.Unlock()

		if notify {
			var sb strings.Builder
			fmt.Fprintf(&sb, "[Background poll %s] %q (id %s)\n", event, p.Name, p.ID)
			fmt.Fprintf(&sb, "Command: %s\n", bgCmdString(p.Argv))
			if runErr != nil {
				fmt.Fprintf(&sb, "Run error: %v\n", runErr)
			} else {
				fmt.Fprintf(&sb, "Exit: %d\n", code)
			}
			sb.WriteString(bgOutputSection(out, total))
			t.notify(p.Channel, p.ChatID, sb.String()+"(Relay this to the user, summarizing briefly.)")
		}
		if done {
			log.Printf("background: poller %q (%s) finished after %d runs", p.Name, p.ID, p.Runs)
			t.notify(p.Channel, p.ChatID, fmt.Sprintf("[Background poller finished] %q (id %s) completed its %d scheduled runs. (Relay briefly to the user.)", p.Name, p.ID, p.Runs))
			p.stopOnce.Do(func() { close(p.stopCh) })
			return
		}
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func bgParseArgv(args map[string]interface{}) ([]string, error) {
	raw, ok := args["cmd"]
	if !ok {
		return nil, fmt.Errorf("'cmd' is required")
	}
	var argv []string
	switch v := raw.(type) {
	case []interface{}:
		for _, a := range v {
			s, ok := a.(string)
			if !ok {
				return nil, fmt.Errorf("cmd array must contain strings only")
			}
			argv = append(argv, s)
		}
	case []string:
		argv = v
	default:
		return nil, fmt.Errorf("cmd must be an array of strings")
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("cmd array is empty")
	}
	return argv, nil
}

func bgCmdString(argv []string) string { return shellJoin(argv) }

func bgHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// bgOutputSection formats the output section of a notification from the tail
// buffer and total bytes produced.
func bgOutputSection(tail string, total int64) string {
	tail = strings.TrimRight(tail, "\n")
	if tail == "" {
		if total > 0 {
			return fmt.Sprintf("Output: (%d bytes, no trailing newline kept)\n", total)
		}
		return "Output: (empty)\n"
	}
	if total > int64(len(tail)) {
		return fmt.Sprintf("Output (last %d bytes of %d):\n%s\n", len(tail), total, tail)
	}
	return fmt.Sprintf("Output:\n%s\n", tail)
}

// ── Persistence ─────────────────────────────────────────────────────────────

type bgPersistedState struct {
	NextID   int          `json:"next_id"`
	OneShots []*bgOneShot `json:"one_shots"`
	Pollers  []*bgPoller  `json:"pollers"`
}

// SetPersistencePath enables persistence and loads prior state. Running
// one-shot jobs are either relaunched (rerunOnRestart) or reported as
// interrupted; pollers resume ticking. All restored commands are re-validated
// against the current sandbox policy.
func (t *BackgroundTool) SetPersistencePath(path string) error {
	t.mu.Lock()
	t.persistPath = path
	err := t.loadLocked()
	t.mu.Unlock()
	return err
}

func (t *BackgroundTool) persistLocked() {
	if t.persistPath == "" {
		return
	}
	state := &bgPersistedState{NextID: t.nextID}
	for _, j := range t.oneShots {
		state.OneShots = append(state.OneShots, j)
	}
	for _, p := range t.pollers {
		state.Pollers = append(state.Pollers, p)
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("background: persist marshal: %v", err)
		return
	}
	tmp := t.persistPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		log.Printf("background: persist write: %v", err)
		return
	}
	if err := os.Rename(tmp, t.persistPath); err != nil {
		log.Printf("background: persist rename: %v", err)
	}
}

func (t *BackgroundTool) loadLocked() error {
	b, err := os.ReadFile(t.persistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var state bgPersistedState
	if err := json.Unmarshal(b, &state); err != nil {
		return fmt.Errorf("background: parse %s: %w", t.persistPath, err)
	}

	for _, j := range state.OneShots {
		if err := t.validate(j.Argv, j.Cwd); err != nil {
			log.Printf("background: restored job %q fails current sandbox policy, skipping: %v", j.Name, err)
			continue
		}
		if j.RerunOnRestart {
			j.StartedAt = time.Now()
			t.oneShots[j.ID] = j
			t.launchOneShot(j)
			log.Printf("background: relaunched job %q (%s) after restart", j.Name, j.ID)
		} else {
			t.notify(j.Channel, j.ChatID, fmt.Sprintf(
				"[Background job interrupted] %q (id %s) did not complete — the agent process restarted before it finished. Command: %s\n(If the job is still needed, start it again. Relay briefly to the user.)",
				j.Name, j.ID, bgCmdString(j.Argv)))
		}
	}
	for _, p := range state.Pollers {
		if err := t.validate(p.Argv, p.Cwd); err != nil {
			log.Printf("background: restored poller %q fails current sandbox policy, skipping: %v", p.Name, err)
			continue
		}
		p.stopCh = make(chan struct{})
		t.pollers[p.ID] = p
		go t.runPoller(p)
		log.Printf("background: resumed poller %q (%s), every %v", p.Name, p.ID, p.Interval)
	}
	if state.NextID > t.nextID {
		t.nextID = state.NextID
	}
	return nil
}

// Shutdown stops all pollers and kills running one-shots. Called on agent
// shutdown so child processes don't outlive the agent.
func (t *BackgroundTool) Shutdown() {
	t.mu.Lock()
	pollers := make([]*bgPoller, 0, len(t.pollers))
	for _, p := range t.pollers {
		pollers = append(pollers, p)
	}
	t.pollers = make(map[string]*bgPoller)
	jobs := make([]*bgOneShot, 0, len(t.oneShots))
	for _, j := range t.oneShots {
		jobs = append(jobs, j)
	}
	t.oneShots = make(map[string]*bgOneShot)
	t.mu.Unlock()
	for _, p := range pollers {
		p.stopOnce.Do(func() { close(p.stopCh) })
	}
	for _, j := range jobs {
		if j.cancel != nil {
			j.cancel()
		}
	}
}
