package tools

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
)

// SpawnTool runs named subagent tasks as isolated `gino agent` subprocesses.
//
// Each task gets its own gino process with a dedicated session key
// ("sp:<name>"), so token cost stays proportional to the task itself —
// the parent's conversation history is never re-sent. Children run with
// a neutered tool set (spawn, message, cron, write_memory) so they can
// neither spawn recursively, message users, schedule jobs, nor pollute the
// profile's long-term memory; their output returns to the caller instead.
//
// Modes:
//   - wait=true  (default): run synchronously and return the child's output
//     directly as the tool result.
//   - wait=false: run in the background; the result is delivered to the
//     originating chat as a signal when the task finishes.
type SpawnTool struct {
	mu            sync.Mutex
	enabled       bool
	binary        string
	homeDir       string
	workspace     string
	model         string
	defaultTO     time.Duration
	maxConcurrent int
	disableTools  []string
	tasks         map[string]*spawnTask
	seq           int
	hub           *chat.Hub
	channel       string
	chatID        string
}

// spawnTask tracks one spawned child agent.
type spawnTask struct {
	ID        string
	Agent     string
	Task      string
	Session   string
	StartedAt time.Time
	cancel    context.CancelFunc
}

const (
	spDefaultTimeoutS  = 300
	spMaxConcurrent    = 4
	spMaxTimeoutS      = 3600
	spTailBytes        = 4096
	spTaskHeadBytes    = 200
	spSessionMaxLen    = 40
	spMaxConcurrentCap = 16
)

// spSessionRe matches characters not allowed in user-provided session names.
var spSessionRe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// NewSpawnToolDisabled creates the spawn tool in its disabled state for
// registration by the agent loop. It stays inert until Configure is called
// (AgentLoop.SetSpawnConfig), so config decides whether spawning exists.
func NewSpawnToolDisabled(homeDir, workspace string, hub *chat.Hub) *SpawnTool {
	return &SpawnTool{
		homeDir:   homeDir,
		workspace: workspace,
		tasks:     make(map[string]*spawnTask),
		hub:       hub,
	}
}

// NewSpawnTool creates an already-configured spawn tool (used by tests).
func NewSpawnTool(cfg config.SpawnConfig, homeDir, workspace string, hub *chat.Hub) *SpawnTool {
	t := NewSpawnToolDisabled(homeDir, workspace, hub)
	t.Configure(cfg, cfg.Model)
	return t
}

// Configure enables the tool and applies runtime settings.
func (t *SpawnTool) Configure(cfg config.SpawnConfig, model string) {
	timeoutS := cfg.DefaultTimeoutS
	if timeoutS <= 0 {
		timeoutS = spDefaultTimeoutS
	}
	if timeoutS > spMaxTimeoutS {
		timeoutS = spMaxTimeoutS
	}
	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = spMaxConcurrent
	}
	if maxConc > spMaxConcurrentCap {
		maxConc = spMaxConcurrentCap
	}
	binary := cfg.Binary
	if binary == "" {
		if exe, err := os.Executable(); err == nil {
			binary = exe
		} else {
			binary = "gino"
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.binary = binary
	t.model = model
	t.defaultTO = time.Duration(timeoutS) * time.Second
	t.maxConcurrent = maxConc
	t.disableTools = cfg.DisableTools
	t.enabled = true
}

func (t *SpawnTool) Name() string { return "spawn" }
func (t *SpawnTool) Description() string {
	return "Run a subagent task in an isolated gino process with its own session (low token cost — the task does not inherit this conversation). Pass a complete, self-contained task description. Actions: spawn (default), list (running tasks), cancel (by id)."
}

func (t *SpawnTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"agent": map[string]interface{}{
				"type":        "string",
				"description": "Short label for the task (e.g. 'docs-research'). Used in reporting.",
			},
			"task": map[string]interface{}{
				"type":        "string",
				"description": "Complete, self-contained task description for the subagent (it cannot see this conversation).",
			},
			"session": map[string]interface{}{
				"type":        "string",
				"description": "Optional session name so follow-up spawns can continue the same subagent context (default: derived from task).",
			},
			"wait": map[string]interface{}{
				"type":        "boolean",
				"description": "If true (default), block until the task finishes and return its output. If false, deliver the result as a message when done.",
			},
			"timeoutS": map[string]interface{}{
				"type":        "integer",
				"description": fmt.Sprintf("Task timeout in seconds (default %d, max %d).", spDefaultTimeoutS, spMaxTimeoutS),
			},
			"action": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"spawn", "list", "cancel"},
				"description": "spawn (default), list running tasks, or cancel by id.",
			},
			"id": map[string]interface{}{
				"type":        "string",
				"description": "Task id (required for cancel, from list).",
			},
		},
		"required": []string{},
	}
}

// SetContext records the originating channel/chat for async result delivery.
func (t *SpawnTool) SetContext(channel, chatID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.channel = channel
	t.chatID = chatID
}

// Shutdown cancels all running tasks (called when the agent loop closes).
func (t *SpawnTool) Shutdown() {
	t.mu.Lock()
	tasks := make([]*spawnTask, 0, len(t.tasks))
	for _, task := range t.tasks {
		tasks = append(tasks, task)
	}
	t.mu.Unlock()
	for _, task := range tasks {
		task.cancel()
	}
}

func (t *SpawnTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	t.mu.Lock()
	enabled := t.enabled
	t.mu.Unlock()
	if !enabled {
		return "", fmt.Errorf("spawn: not enabled (set agents.defaults.spawn.enabled=true in config)")
	}
	action, _ := args["action"].(string)
	switch action {
	case "", "spawn":
		return t.doSpawn(ctx, args)
	case "list":
		return t.listTasks(), nil
	case "cancel":
		id, _ := args["id"].(string)
		return "", t.cancelTask(id)
	default:
		return "", fmt.Errorf("spawn: unknown action %q (use spawn, list, or cancel)", action)
	}
}

func (t *SpawnTool) doSpawn(ctx context.Context, args map[string]interface{}) (string, error) {
	agentName, _ := args["agent"].(string)
	task, _ := args["task"].(string)
	sessionName, _ := args["session"].(string)
	wait := true
	if w, ok := args["wait"].(bool); ok {
		wait = w
	}
	timeoutS := 0
	if n, ok := args["timeoutS"].(float64); ok {
		timeoutS = int(n)
	}

	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("spawn: 'task' is required")
	}
	if len(task) > 100*1024 {
		return "", fmt.Errorf("spawn: task too long (%d bytes, max 100KB)", len(task))
	}

	timeout := t.defaultTO
	if timeoutS > 0 {
		if timeoutS > spMaxTimeoutS {
			timeoutS = spMaxTimeoutS
		}
		timeout = time.Duration(timeoutS) * time.Second
	}

	sess := t.normalizeSession(sessionName, task)

	t.mu.Lock()
	if len(t.tasks) >= t.maxConcurrent {
		running := len(t.tasks)
		t.mu.Unlock()
		return "", fmt.Errorf("spawn: %d task(s) already running (max %d); wait for one to finish, cancel it, or check `spawn list`", running, t.maxConcurrent)
	}
	t.seq++
	id := fmt.Sprintf("sp-%d-%d", t.seq, time.Now().Unix())
	tk := &spawnTask{
		ID:        id,
		Agent:     agentName,
		Task:      task,
		Session:   sess,
		StartedAt: time.Now(),
	}
	t.tasks[id] = tk
	t.mu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	tk.cancel = cancel

	childDisable := t.childDisableTools()
	argv := []string{
		t.binary, "agent",
		"-m", task,
		"-session", "sp:" + sess,
		"-disable-tools", strings.Join(childDisable, ","),
	}
	if t.model != "" {
		argv = append(argv, "-M", t.model)
	}

	if wait {
		// Synchronous: run inline, return output as the tool result.
		output, err := t.runChild(runCtx, argv)
		t.mu.Lock()
		delete(t.tasks, id)
		t.mu.Unlock()
		cancel()
		elapsed := time.Since(tk.StartedAt).Round(time.Second)
		if err != nil {
			if runCtx.Err() == context.DeadlineExceeded {
				return fmt.Sprintf("⏱ task %s (session %s) timed out after %s. Partial output:\n%s", id, sess, elapsed, tailBytes(output, spTailBytes)), nil
			}
			return fmt.Sprintf("✗ task %s (session %s) failed after %s: %v\nOutput:\n%s", id, sess, elapsed, err, tailBytes(output, spTailBytes)), nil
		}
		return fmt.Sprintf("✓ task %s (session %s) completed in %s\n%s", id, sess, elapsed, tailBytes(output, spTailBytes)), nil
	}

	// Asynchronous: deliver the result to the originating chat when done.
	channel, chatID := t.currentContext()
	go func() {
		defer cancel()
		output, err := t.runChild(runCtx, argv)
		elapsed := time.Since(tk.StartedAt).Round(time.Second)
		t.mu.Lock()
		delete(t.tasks, id)
		t.mu.Unlock()

		var head string
		if err != nil {
			head = fmt.Sprintf("🧩 spawn task %s (session %s) FAILED after %s: %v", id, sess, elapsed, err)
		} else {
			head = fmt.Sprintf("🧩 spawn task %s (session %s) finished in %s", id, sess, elapsed)
		}
		body := fmt.Sprintf("%s\nLabel: %s\nTask: %s\n--- output ---\n%s",
			head, agentLabel(agentName), headBytes(task, spTaskHeadBytes), tailBytes(output, spTailBytes))
		t.deliver(channel, chatID, body)
	}()

	return fmt.Sprintf("started: id=%s agent=%s session=%s timeout=%s — the result will be delivered when it finishes. Use action=list to see it or action=cancel id=%s to stop it.",
		id, agentLabel(agentName), sess, timeout.Round(time.Second), id), nil
}

// agentLabel renders the optional agent label for reports.
func agentLabel(name string) string {
	if name == "" {
		return "(unlabeled)"
	}
	return name
}

// runChild executes the gino agent subprocess, killing the whole process
// group on cancellation (the child may itself shell out). WaitDelay bounds
// the wait for grandchildren to exit after the group kill.
func (t *SpawnTool) runChild(ctx context.Context, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = t.workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	var combined strings.Builder
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return combined.String(), ctx.Err()
		}
		return combined.String(), err
	}
	return combined.String(), nil
}

// childDisableTools builds the disabled-tools list for children: always
// neuter spawn (no recursion), message and cron (no side-channel messaging
// or scheduling), and write_memory (subagent tasks must not pollute the
// profile's long-term memory), plus any configured extras.
func (t *SpawnTool) childDisableTools() []string {
	seen := map[string]bool{"spawn": true, "message": true, "cron": true, "write_memory": true}
	out := []string{"spawn", "message", "cron", "write_memory"}
	for _, name := range t.disableTools {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// normalizeSession sanitizes a user-provided session name or derives one
// from the task text. The child's session key becomes "sp:<name>".
func (t *SpawnTool) normalizeSession(name, task string) string {
	t.mu.Lock()
	seq := t.seq
	t.mu.Unlock()
	if name != "" {
		name = spSessionRe.ReplaceAllString(strings.TrimSpace(name), "-")
		name = strings.Trim(name, "-")
		if name == "" {
			name = "task"
		}
		if len(name) > spSessionMaxLen {
			name = name[:spSessionMaxLen]
		}
		return name
	}
	// Derive from the first few words of the task.
	slug := strings.ToLower(strings.Join(strings.Fields(task), "-"))
	slug = spSessionRe.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > spSessionMaxLen {
		slug = slug[:spSessionMaxLen]
	}
	if slug == "" {
		slug = "task"
	}
	return fmt.Sprintf("%s-%d", slug, seq)
}

func (t *SpawnTool) currentContext() (string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.channel, t.chatID
}

// deliver injects a completed task's output into the hub as a signal, so it
// routes to the signal session namespace and never cancels an active
// interactive turn (mirrors the background tool's delivery pattern).
func (t *SpawnTool) deliver(channel, chatID, body string) {
	if t.hub == nil {
		log.Printf("spawn: no hub; dropping delivery: %s", headBytes(body, 200))
		return
	}
	meta := map[string]interface{}{
		"signal_action": "spawn_task",
		"signal_silent": false,
	}
	select {
	case t.hub.In <- chat.Inbound{
		Channel:  channel,
		SenderID: "spawn",
		ChatID:   chatID,
		Content:  body,
		Metadata: meta,
	}:
	case <-time.After(10 * time.Second):
		log.Printf("spawn: hub delivery timed out; dropping result")
	}
}

func (t *SpawnTool) listTasks() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.tasks) == 0 {
		return "no running spawn tasks"
	}
	ids := make([]string, 0, len(t.tasks))
	for id := range t.tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d running spawn task(s):\n", len(t.tasks)))
	for _, id := range ids {
		tk := t.tasks[id]
		sb.WriteString(fmt.Sprintf("- %s agent=%s session=%s running %s task=%s\n",
			tk.ID, agentLabel(tk.Agent), tk.Session, time.Since(tk.StartedAt).Round(time.Second), headBytes(tk.Task, spTaskHeadBytes)))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (t *SpawnTool) cancelTask(id string) error {
	t.mu.Lock()
	tk, ok := t.tasks[id]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("spawn: no running task %q (see action=list)", id)
	}
	tk.cancel()
	return nil
}

// headBytes returns at most n bytes of s (no truncation marker).
func headBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// tailBytes returns the last n bytes of s with a leading-ellipsis marker,
// preferring to start on a whole line.
func tailBytes(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	tail := s[len(s)-n:]
	if idx := strings.Index(tail, "\n"); idx >= 0 {
		tail = tail[idx+1:]
	}
	return "…\n" + tail
}
