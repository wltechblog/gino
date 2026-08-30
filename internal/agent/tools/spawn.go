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
	mu            sync.RWMutex
	enabled       bool
	binary        string
	homeDir       string
	workspace     string
	model         string
	defaultTO     time.Duration
	maxConcurrent int
	disableTools  []string
	// agents is the registry of named subagent profiles (config-defined).
	agents map[string]config.SpawnAgentConfig
	// providerPresets holds named endpoint definitions (providers.presets)
	// that agent profiles can reference. nil entries fall back to inherit.
	providerPresets map[string]*config.ProviderConfig
	// cliCfg carries global CLI flag overrides (model, system-prompt,
	// disable-tools) so they apply to spawned children too.
	cliCfg  CLIFlags
	tasks   map[string]*spawnTask
	seq     int
	hub     *chat.Hub
	channel string
	chatID  string
}

// CLIFlags carries the gino agent CLI flag overrides that should propagate
// to spawned children (set once at tool creation from main.go).
type CLIFlags struct {
	Model        string
	SystemPrompt string
	DisableTools []string
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
	return NewSpawnToolWithFlags(cfg, homeDir, workspace, hub, CLIFlags{})
}

// NewSpawnToolWithFlags creates a configured spawn tool with CLI flag
// overrides applied (-M, -system-prompt, -disable-tools inheritance).
// Provider presets are supplied separately via SetProviderPresets.
func NewSpawnToolWithFlags(cfg config.SpawnConfig, homeDir, workspace string, hub *chat.Hub, cliCfg CLIFlags) *SpawnTool {
	t := NewSpawnToolDisabled(homeDir, workspace, hub)
	t.SetCLIFlags(cliCfg)
	t.Configure(cfg, cfg.Model)
	return t
}

// SetCLIFlags stores CLI flag overrides to propagate to children. Must be
// called before Configure (or any spawn) to take effect on argv building.
func (t *SpawnTool) SetCLIFlags(f CLIFlags) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cliCfg = f
}

// SetProviderPresets stores named provider presets from providers.presets
// for resolution by agent profiles at spawn time.
func (t *SpawnTool) SetProviderPresets(presets map[string]*config.ProviderConfig) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.providerPresets = presets
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
	t.agents = make(map[string]config.SpawnAgentConfig, len(cfg.Agents))
	for _, a := range cfg.Agents {
		t.agents[a.Name] = a
	}
	t.enabled = true
}

func (t *SpawnTool) Name() string { return "spawn" }

// spawnToolDescription composes the tool description from the static base
// plus one line per configured agent profile, so the parent LLM can route
// tasks to the right agent by purpose.
func spawnToolDescription(agents map[string]config.SpawnAgentConfig) string {
	var sb strings.Builder
	sb.WriteString("Run a subagent task in an isolated gino process with its own session (low token cost — the task does not inherit this conversation). Pass a complete, self-contained task description. Actions: spawn (default), list (running tasks), cancel (by id).")
	if len(agents) > 0 {
		names := make([]string, 0, len(agents))
		for name := range agents {
			names = append(names, name)
		}
		sort.Strings(names)
		sb.WriteString("\n\nAvailable agents (pass one as 'agent'; use its listed strengths to choose):")
		for _, name := range names {
			a := agents[name]
			desc := strings.TrimSpace(a.Description)
			if desc == "" {
				desc = "(no description — general-purpose)"
			}
			fmt.Fprintf(&sb, "\n- %s: %s", name, desc)
			var caps []string
			if a.Model != "" {
				caps = append(caps, "model="+a.Model)
			}
			if a.Provider != "" {
				caps = append(caps, "provider="+a.Provider)
			}
			if a.TimeoutS > 0 {
				caps = append(caps, fmt.Sprintf("timeout=%ds", a.TimeoutS))
			}
			if len(caps) > 0 {
				fmt.Fprintf(&sb, " [%s]", strings.Join(caps, ", "))
			}
		}
		sb.WriteString("\n\nIf no agent fits the task, omit 'agent' to use the default configuration. Prefer spawning a listed agent over doing equivalent work inline when its description matches.")
	}
	return sb.String()
}

func (t *SpawnTool) Description() string {
	t.mu.RLock()
	agents := t.agents
	t.mu.RUnlock()
	return spawnToolDescription(agents)
}

func (t *SpawnTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"agent": map[string]interface{}{
				"type":        "string",
				"description": "Named agent profile to run the task (see description for available agents and when to use each). Omit for a general-purpose task with default settings.",
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

	// Resolve the named agent profile (if any) under the tool lock.
	profile, known, err := t.resolveAgent(agentName)
	if err != nil {
		return "", err
	}
	_ = known // profile applies whenever non-nil

	// Timeout precedence: call timeoutS > profile.TimeoutS > defaultTO.
	timeout := t.defaultTO
	if profile != nil && profile.TimeoutS > 0 {
		timeout = time.Duration(profile.TimeoutS) * time.Second
	}
	if timeoutS > 0 {
		if timeoutS > spMaxTimeoutS {
			timeoutS = spMaxTimeoutS
		}
		timeout = time.Duration(timeoutS) * time.Second
	}
	if timeout > time.Duration(spMaxTimeoutS)*time.Second {
		timeout = time.Duration(spMaxTimeoutS) * time.Second
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

	argv, childEnv := t.buildChildInvocation(agentName, profile, sess, task)

	if wait {
		// Synchronous: run inline, return output as the tool result.
		output, err := t.runChild(runCtx, argv, childEnv)
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
		output, err := t.runChild(runCtx, argv, childEnv)
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
// the wait for grandchildren to exit after the group kill. extraEnv holds
// additional environment entries (e.g. GINO_SYSTEM_PROMPT from an agent
// profile's systemPromptOverride).
func (t *SpawnTool) runChild(ctx context.Context, argv []string, extraEnv []string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = t.workspace
	cmd.Env = append(os.Environ(), extraEnv...)
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

// resolveAgent looks up a named agent profile. An empty name returns
// (nil, false, nil) — the default path with inherited settings. A non-empty
// name that is not in the registry returns an error listing valid names,
// so typos fail fast instead of silently running with default settings.
func (t *SpawnTool) resolveAgent(name string) (*config.SpawnAgentConfig, bool, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if name == "" {
		return nil, false, nil
	}
	a, ok := t.agents[name]
	if !ok {
		return nil, false, fmt.Errorf("spawn: unknown agent %q %s", name, t.validAgentsHint())
	}
	return &a, true, nil
}

// validAgentsHint formats the registry contents for error messages. Must be
// called with t.mu held.
func (t *SpawnTool) validAgentsHint() string {
	if len(t.agents) == 0 {
		return "(no agents configured — omit 'agent' or add spawn.agents entries to config)"
	}
	names := make([]string, 0, len(t.agents))
	for name := range t.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return "(available: " + strings.Join(names, ", ") + ")"
}

// buildChildInvocation assembles argv and env for a child agent run,
// applying the precedence chain for model and system prompt:
//
//	model:         call site > agent profile > spawn.model > CLI -M > (config default)
//	system prompt: agent profile override > CLI -system-prompt > (default bootstrap)
//	provider:      agent profile preset (via env vars) > inherited provider config
func (t *SpawnTool) buildChildInvocation(agentName string, profile *config.SpawnAgentConfig, sess, task string) ([]string, []string) {
	t.mu.RLock()
	binary, baseModel, cfgDisable, cliCfg := t.binary, t.model, t.disableTools, t.cliCfg
	t.mu.RUnlock()

	var argv []string

	if profile == nil {
		// Un-named default path (original behavior + CLI flag propagation).
		disable := mergeDisableTools(spMandatoryDisable, cfgDisable, nil, cliCfg.DisableTools)
		argv = []string{
			binary, "agent",
			"-m", task,
			"-session", "sp:" + sess,
			"-disable-tools", strings.Join(disable, ","),
		}
		if m := firstNonEmpty(cliCfg.Model, baseModel); m != "" {
			argv = append(argv, "-M", m)
		}
		if sp := firstNonEmpty(cliCfg.SystemPrompt); sp != "" {
			argv = append(argv, "-system-prompt", sp)
		}
		return argv, nil
	}

	// Named profile path.
	model := firstNonEmpty(profile.Model, baseModel, cliCfg.Model)
	agentDisable := mergeDisableTools(spMandatoryDisable, cfgDisable, profile.DisableTools, cliCfg.DisableTools)
	argv = []string{
		binary, "agent",
		"-m", task,
		"-session", "sp:" + sess,
		"-disable-tools", strings.Join(agentDisable, ","),
	}
	if model != "" {
		argv = append(argv, "-M", model)
	}
	if sp := firstNonEmpty(profile.SystemPromptOverride, cliCfg.SystemPrompt); sp != "" {
		argv = append(argv, "-system-prompt", sp)
	}

	// Provider resolution: a named preset injects endpoint + key + model via
	// env vars (env overrides config in the loader), keeping the child's
	// argv identical in shape to the default path.
	var env []string
	if profile.Provider != "" {
		t.mu.RLock()
		preset := t.providerPresets[profile.Provider]
		t.mu.RUnlock()
		if preset == nil {
			log.Printf("spawn: agent %q references unknown provider preset %q — child will use inherited provider", agentName, profile.Provider)
		} else {
			if preset.APIKey != "" {
				env = append(env, buildEnv("GINO_API_KEY", preset.APIKey))
			}
			if preset.APIBase != "" {
				env = append(env, buildEnv("GINO_API_BASE", preset.APIBase))
			}
			if preset.Model != "" && profile.Model == "" {
				// Profile model (explicit) beats preset default model; preset
				// model beats config default only when profile has none.
				env = append(env, buildEnv("GINO_MODEL", preset.Model))
			}
			if preset.ReasoningEffort != "" {
				env = append(env, buildEnv("GINO_REASONING_EFFORT", preset.ReasoningEffort))
			}
		}
	}
	return argv, env
}

// firstNonEmpty returns the first non-empty argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildEnv returns "KEY=VALUE" — a tiny helper to keep env assembly readable.
func buildEnv(key, value string) string {
	return key + "=" + value
}

// spMandatoryDisable lists tools always disabled in children: spawn (no
// recursion), message and cron (no side-channel messaging or scheduling),
// and write_memory (subagent tasks must not pollute the profile's
// long-term memory). This list is prepended to every child's disable set,
// regardless of profile or CLI flags — it cannot be opted out of.
var spMandatoryDisable = []string{"spawn", "message", "cron", "write_memory"}

// mergeDisableTools merges the mandatory child-neutering list, the global
// spawn disable list, agent-specific extras, and CLI -disable-tools
// overrides, deduplicating while preserving that order.
func mergeDisableTools(mandatory, global, agentExtras, cli []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(mandatory)+len(global)+len(agentExtras)+len(cli))
	for _, group := range [][]string{mandatory, global, agentExtras, cli} {
		for _, name := range group {
			if name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
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
