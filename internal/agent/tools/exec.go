package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wltechblog/gino/internal/config"
)

// ExecTool runs shell commands with a timeout.
// Behavior depends on SandboxConfig mode:
//   - "strict":     array-only commands, no absolute paths, full blacklist (default, backward compatible)
//   - "permissive": block truly dangerous commands (dd, mkfs, shutdown), allow absolute paths, array-only
//   - "yolo":       no restrictions — string commands allowed, no path validation, no blacklist

const (
	// defaultExecTimeoutS applies when no explicit timeout is configured.
	// Foreground exec must never run unbounded — long jobs belong to the
	// background tool.
	defaultExecTimeoutS = 300
	// maxPerCallExecTimeoutS caps the per-call "timeout" argument. Calls may
	// never request an infinite or absurdly long foreground timeout.
	maxPerCallExecTimeoutS = 3600
)

type ExecTool struct {
	mu          sync.RWMutex
	timeout     time.Duration
	allowedDir  string
	allowedDirs []string
	sandbox     config.SandboxConfig

	// inFlight tracks live commands by turn-origin session key
	// ("channel:chatID") so /abort can kill a session's foreground execs
	// without stopping the turn itself.
	inFlight map[string]map[*inflightCmd]struct{}

	// inFlightMu serializes exec.Cmd.Start (inside runCmd) against
	// abortMatching's process-handle reads + process-group kill. Start
	// mutates the Cmd struct (allocates Process, sets Pid); reading
	// ic.cmd.Process concurrently with Start was the -race DATA RACE.
	// The lock is held ONLY for the Start instant and the kill instant —
	// never across Wait — so aborting a long-running command still kills
	// it immediately (abort semantics unchanged). Deliberately NOT t.mu:
	// runCmd holds this around Start while a command is being spawned,
	// and tests poll inFlight under t.mu.RLock() — reusing t.mu would
	// let aborters block those polls (and abort waits on this lock while
	// holding t.mu, which would self-deadlock).
	inFlightMu sync.Mutex
}

// inflightCmd is one live exec command registered for abort tracking.
type inflightCmd struct {
	cmd *exec.Cmd

	// mu guards aborted: written by abortMatching, read by runCmd after
	// Wait returns — different goroutines, so the flag needs its own lock.
	mu      sync.Mutex
	aborted bool // set when AbortSession targeted this command
}

// wasAborted reports whether AbortSession targeted this command. Call it
// only after the command's Wait has returned.
func (ic *inflightCmd) wasAborted() bool {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	return ic.aborted
}

func NewExecTool(timeoutSecs int) *ExecTool {
	return &ExecTool{timeout: time.Duration(timeoutSecs) * time.Second, sandbox: config.SandboxConfig{}, inFlight: make(map[string]map[*inflightCmd]struct{})}
}

func NewExecToolWithWorkspace(timeoutSecs int, allowedDir string) *ExecTool {
	return &ExecTool{timeout: time.Duration(timeoutSecs) * time.Second, allowedDir: allowedDir, sandbox: config.SandboxConfig{}, inFlight: make(map[string]map[*inflightCmd]struct{})}
}

func NewExecToolWithAllowedDirs(timeoutSecs int, allowedDir string, allowedDirs []string) *ExecTool {
	dirs := make([]string, 0, len(allowedDirs))
	for _, d := range allowedDirs {
		if d != "" {
			dirs = append(dirs, filepath.Clean(d))
		}
	}
	return &ExecTool{timeout: time.Duration(timeoutSecs) * time.Second, allowedDir: allowedDir, allowedDirs: dirs, sandbox: config.SandboxConfig{Mode: "permissive"}, inFlight: make(map[string]map[*inflightCmd]struct{})}
}

// NewExecToolWithSandbox creates an ExecTool with full sandbox configuration.
func NewExecToolWithSandbox(timeoutSecs int, allowedDir string, allowedDirs []string, sandbox config.SandboxConfig) *ExecTool {
	if timeoutSecs <= 0 {
		timeoutSecs = defaultExecTimeoutS
	}
	dirs := make([]string, 0, len(allowedDirs))
	for _, d := range allowedDirs {
		if d != "" {
			dirs = append(dirs, filepath.Clean(d))
		}
	}
	return &ExecTool{timeout: time.Duration(timeoutSecs) * time.Second, allowedDir: allowedDir, allowedDirs: dirs, sandbox: sandbox, inFlight: make(map[string]map[*inflightCmd]struct{})}
}

func (t *ExecTool) Name() string { return "exec" }
func (t *ExecTool) Description() string {
	if t.sandbox.IsYolo() {
		return "Execute shell commands. YOLO mode: string or array form allowed, no restrictions."
	}
	return "Execute shell commands (array form only, restricted for safety)"
}

func (t *ExecTool) Parameters() map[string]interface{} {
	props := map[string]interface{}{
		"cmd": map[string]interface{}{
			"type":        "array",
			"description": "Command as array [program, arg1, arg2, ...]. String form is disallowed for security.",
			"items": map[string]interface{}{
				"type": "string",
			},
			"minItems": 1,
		},
		"cwd": map[string]interface{}{
			"type":        "string",
			"description": "Working directory for the command. Must be within an allowed directory. Defaults to the workspace root.",
		},
	}
	// Per-call timeout: capped by maxPerCallExecTimeoutS; there is no
	// "infinite" option by design. Long-running work belongs to the
	// background tool.
	props["timeout"] = map[string]interface{}{
		"type":        "integer",
		"description": fmt.Sprintf("Per-call timeout in seconds (default %d, hard max %d — never infinite; use the background tool for longer jobs).", defaultExecTimeoutS, maxPerCallExecTimeoutS),
		"minimum":     1,
		"maximum":     maxPerCallExecTimeoutS,
	}
	// In yolo mode, accept string commands too
	if t.sandbox.IsYolo() {
		props["cmd"] = map[string]interface{}{
			"description": "Command to execute. Can be a string (shell) or array [program, arg1, arg2, ...].",
			"oneOf": []interface{}{
				map[string]interface{}{"type": "string"},
				map[string]interface{}{
					"type":     "array",
					"items":    map[string]interface{}{"type": "string"},
					"minItems": 1,
				},
			},
		}
	}
	return map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   []string{"cmd"},
	}
}

// Default blacklists by mode.
var strictBlacklist = map[string]struct{}{
	"rm":       {},
	"sudo":     {},
	"dd":       {},
	"mkfs":     {},
	"shutdown": {},
	"reboot":   {},
}

var permissiveBlacklist = map[string]struct{}{
	"sudo":     {},
	"dd":       {},
	"mkfs":     {},
	"shutdown": {},
	"reboot":   {},
}

var shellBuiltins = map[string]string{
	"cd":      "use the cwd parameter instead to set the working directory for a command",
	"source":  "",
	"export":  "",
	"alias":   "",
	"unset":   "",
	"set":     "",
	"shift":   "",
	"read":    "",
	"wait":    "",
	"trap":    "",
	"return":  "",
	"local":   "",
	"declare": "",
	"typeset": "",
	"let":     "",
	"eval":    "",
	"bg":      "",
	"fg":      "",
	"jobs":    "",
	"disown":  "",
	"builtin": "",
	"command": "",
	"type":    "",
	"hash":    "",
}

func (t *ExecTool) isBlocked(prog string) bool {
	if t.sandbox.IsYolo() {
		return false
	}

	base := strings.ToLower(filepath.Base(prog))

	// Check custom blocked commands
	for _, blocked := range t.sandbox.BlockedCommands {
		if strings.ToLower(blocked) == base {
			return true
		}
	}

	// Check mode-specific blacklist
	var blacklist map[string]struct{}
	if t.sandbox.IsPermissive() {
		blacklist = permissiveBlacklist
	} else {
		blacklist = strictBlacklist
	}
	_, blocked := blacklist[base]
	return blocked
}

func (t *ExecTool) isAllowed(prog string) bool {
	if t.sandbox.IsYolo() {
		return true
	}

	// If no whitelist, all non-blocked programs are allowed
	if len(t.sandbox.AllowedCommands) == 0 {
		return true
	}

	base := strings.ToLower(filepath.Base(prog))
	for _, allowed := range t.sandbox.AllowedCommands {
		if strings.ToLower(allowed) == base {
			return true
		}
	}
	return false
}

func isShellBuiltin(prog string) (hint string, ok bool) {
	hint, ok = shellBuiltins[strings.ToLower(filepath.Base(prog))]
	return
}

func (t *ExecTool) isArgUnsafe(s string) bool {
	if t.sandbox.IsYolo() {
		return false
	}

	if strings.HasPrefix(s, "~") || strings.Contains(s, "..") {
		return true
	}
	if !strings.HasPrefix(s, "/") {
		return false
	}

	// An explicit allowedDirs grant is honored in EVERY mode — it is
	// the operator's explicit authorization, stricter than mode defaults.
	// Root ("/") grants cover any absolute path.
	cleaned := filepath.Clean(s)
	for _, d := range t.allowedDirs {
		if cleaned == filepath.Clean(d) {
			return false
		}
		if d == "/" {
			return false
		}
		if strings.HasPrefix(cleaned, filepath.Clean(d)+string(filepath.Separator)) {
			return false
		}
	}
	if t.allowedDir != "" {
		ad := filepath.Clean(t.allowedDir)
		if cleaned == ad || strings.HasPrefix(cleaned, ad+string(filepath.Separator)) {
			return false
		}
	}

	// Beyond explicit grants, mode rules decide. Permissive (and yolo,
	// already returned above) allows absolute paths — but when allowedDirs
	// IS configured it stays the containment boundary: paths outside are
	// unsafe. Strict without allowAbsolutePaths blocks everything else.
	if t.sandbox.AllowsAbsolutePaths() {
		// Grants configured and the path is outside all of them -> the
		// grants remain the containment boundary even in permissive mode.
		return len(t.allowedDirs) > 0 || t.allowedDir != ""
	}
	return true
}

// isDirAllowed checks if a directory path is within one of the allowed directories.
func (t *ExecTool) isDirAllowed(dir string) bool {
	if t.sandbox.IsYolo() {
		return true
	}

	cleaned := filepath.Clean(dir)
	for _, d := range t.allowedDirs {
		if cleaned == d || cleaned == filepath.Clean(d) {
			return true
		}
		// Root grant covers everything; Clean("/")+"/" would be "//",
		// which prefixes nothing (same special case as filesystem.go).
		if d == "/" {
			return true
		}
		if strings.HasPrefix(cleaned, filepath.Clean(d)+string(filepath.Separator)) {
			return true
		}
	}
	if t.allowedDir != "" {
		ad := filepath.Clean(t.allowedDir)
		if cleaned == ad || strings.HasPrefix(cleaned, ad+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Validate applies this tool's full sandbox policy (blocklist, whitelist,
// shell-builtin check, arg safety, cwd containment) to a command that will be
// executed by another component (e.g. the background tool). It performs no
// execution — commands that pass are subject to the same restrictions as a
// direct exec call.
func (t *ExecTool) Validate(argv []string, cwd string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	prog := argv[0]

	if t.isBlocked(prog) {
		return fmt.Errorf("program %q is disallowed", prog)
	}
	if !t.isAllowed(prog) {
		return fmt.Errorf("program %q is not in the allowed list", prog)
	}
	if !t.sandbox.IsYolo() {
		if hint, ok := isShellBuiltin(prog); ok {
			if hint != "" {
				return fmt.Errorf("%s", hint)
			}
			return fmt.Errorf("shell builtin %q cannot be validated for background execution", prog)
		}
	}
	for _, a := range argv[1:] {
		if t.isArgUnsafe(a) {
			return fmt.Errorf("argument %q looks unsafe", a)
		}
	}
	if cwd != "" && !t.isDirAllowed(cwd) {
		return fmt.Errorf("cwd %q is outside the allowed directories", cwd)
	}
	return nil
}

func (t *ExecTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	// Per-call timeout override, clamped to the hard ceiling.
	if raw, ok := args["timeout"]; ok {
		secs := 0
		switch v := raw.(type) {
		case float64:
			secs = int(v)
		case int:
			secs = v
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				secs = n
			}
		}
		if secs > 0 {
			if secs > maxPerCallExecTimeoutS {
				secs = maxPerCallExecTimeoutS
			}
			ctx = withPerCallTimeout(ctx, time.Duration(secs)*time.Second)
		}
	}
	cmdRaw, ok := args["cmd"]

	if !ok {
		return "", fmt.Errorf("exec: 'cmd' argument required")
	}

	// Handle string commands
	if cmdStr, ok := cmdRaw.(string); ok {
		// Check if the string is actually a JSON-encoded array that the MCP framework
		// delivered as a string instead of a proper []interface{}
		trimmed := strings.TrimSpace(cmdStr)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			var parsed []interface{}
			if err := json.Unmarshal([]byte(cmdStr), &parsed); err == nil && len(parsed) > 0 {
				// All elements must be strings
				allStrings := true
				for _, v := range parsed {
					if _, ok := v.(string); !ok {
						allStrings = false
						break
					}
				}
				if allStrings {
					log.Printf("[DEBUG-EXEC] detected JSON array string, parsing as array with %d elements", len(parsed))
					// Re-inject as proper []interface{} and recurse
					newArgs := make(map[string]interface{})
					for k, v := range args {
						newArgs[k] = v
					}
					newArgs["cmd"] = parsed
					return t.Execute(ctx, newArgs)
				}
			}
		}

		// Genuine string command — only allowed in yolo mode
		if !t.sandbox.AllowsStringCommands() {
			return "", errors.New("exec: string commands are disallowed; use array form (or enable yolo mode)")
		}
		// Resolve working directory
		workDir, err := t.resolveWorkDir(args)
		if err != nil {
			return "", err
		}
		return t.runCmd(ctx, "sh", []string{"-c", cmdStr}, workDir)
	}

	var argv []string
	switch v := cmdRaw.(type) {
	case []interface{}:
		if len(v) == 0 {
			return "", fmt.Errorf("exec: empty cmd array")
		}
		for _, a := range v {
			s, ok := a.(string)
			if !ok {
				return "", fmt.Errorf("exec: cmd array must contain strings only")
			}
			argv = append(argv, s)
		}
	default:
		return "", fmt.Errorf("exec: unsupported cmd type")
	}

	// Resolve working directory
	workDir, err := t.resolveWorkDir(args)
	if err != nil {
		return "", err
	}

	prog := argv[0]

	// Check whitelist/blacklist
	if t.isBlocked(prog) {
		return "", fmt.Errorf("exec: program '%s' is disallowed", prog)
	}
	if !t.isAllowed(prog) {
		return "", fmt.Errorf("exec: program '%s' is not in the allowed list", prog)
	}

	// Handle shell builtins (not in yolo mode)
	if !t.sandbox.IsYolo() {
		if hint, ok := isShellBuiltin(prog); ok {
			if hint != "" {
				return "", fmt.Errorf("exec: %s", hint)
			}
			shellArg := strings.Join(argv, " ")
			for _, a := range argv[1:] {
				if t.isArgUnsafe(a) {
					return "", fmt.Errorf("exec: argument '%s' looks unsafe", a)
				}
			}
			return t.runCmd(ctx, "sh", []string{"-c", shellArg}, workDir)
		}
	}

	// Check arguments for unsafe content
	for _, a := range argv[1:] {
		if t.isArgUnsafe(a) {
			return "", fmt.Errorf("exec: argument '%s' looks unsafe", a)
		}
	}

	// Always run through sh -c for reliable PATH resolution.
	// Properly quote each argument so they survive shell interpretation.
	shellCmd := shellJoin(argv)
	return t.runCmd(ctx, "sh", []string{"-c", shellCmd}, workDir)
}

// SetDefaultTimeout updates the tool's default foreground timeout. Zero or
// negative restores the 300s built-in; anything above the hard ceiling is
// clamped. There is no infinite setting.
func (t *ExecTool) SetDefaultTimeout(secs int) {
	if secs <= 0 {
		secs = defaultExecTimeoutS
	}
	if secs > maxPerCallExecTimeoutS {
		secs = maxPerCallExecTimeoutS
	}
	t.mu.Lock()
	t.timeout = time.Duration(secs) * time.Second
	t.mu.Unlock()
}

// SetWorkspace atomically updates the default working directory (used when
// no cwd argument is provided). The allowed-dirs list is left untouched —
// previously allowed directories remain executable-in. Used by runtime
// project switching.
func (t *ExecTool) SetWorkspace(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("exec: resolve workspace path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("exec: workspace %q is not an accessible directory", abs)
	}
	t.mu.Lock()
	t.allowedDir = abs
	t.mu.Unlock()
	return nil
}

func (t *ExecTool) resolveWorkDir(args map[string]interface{}) (string, error) {
	t.mu.RLock()
	workDir := t.allowedDir
	t.mu.RUnlock()
	if cwdRaw, ok := args["cwd"]; ok {
		cwd, ok := cwdRaw.(string)
		if !ok {
			return workDir, nil
		}
		cleaned := filepath.Clean(cwd)
		if t.isDirAllowed(cleaned) {
			return cleaned, nil
		}
		// In yolo mode, allow any dir
		if t.sandbox.IsYolo() {
			return cleaned, nil
		}
		// Explicitly provided cwd is outside allowed dirs — error
		return "", fmt.Errorf("exec: cwd %q is not within an allowed directory", cwd)
	}
	return workDir, nil
}

func (t *ExecTool) runCmd(ctx context.Context, prog string, args []string, dir string) (string, error) {
	cctx := ctx
	// Foreground exec is ALWAYS deadline-bounded. The tool default applies
	// unless the call requested a per-call timeout; requests above the hard
	// ceiling are clamped, never honored. Zero tool default (legacy callers)
	// falls back to defaultExecTimeoutS. There is deliberately no "no
	// timeout" option — unbounded work belongs to the background tool.
	toolTimeout := t.timeout
	if toolTimeout <= 0 {
		toolTimeout = time.Duration(defaultExecTimeoutS) * time.Second
	}
	deadline := time.Duration(maxPerCallExecTimeoutS) * time.Second
	if perCall, ok := perCallTimeoutFromContext(ctx); ok && perCall > 0 && perCall < deadline {
		deadline = perCall
	} else if toolTimeout < deadline {
		deadline = toolTimeout
	}
	var cancel context.CancelFunc
	cctx, cancel = context.WithTimeout(ctx, deadline)
	defer cancel()

	cmd := exec.CommandContext(cctx, prog, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Run in its own process group so timeout/cancel kills the whole tree
	// (sh plus every child it spawned). Killing only the direct child —
	// the default CommandContext behavior — leaves grandchildren alive
	// holding the output pipe open, and CombinedOutput then blocks forever
	// even though the "timeout fired". This is the bug that made /stop
	// appear dead on runaway commands.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			// Negative pid = signal the entire process group.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return cmd.Process.Kill()
	}
	// Belt and braces: even if something still holds the pipes open, the
	// call returns shortly after the deadline instead of blocking forever.
	cmd.WaitDelay = 5 * time.Second

	// Register for /abort tracking keyed by the turn's origin session.
	ic := &inflightCmd{cmd: cmd}
	session := sessionKeyFromContext(ctx)
	t.registerInflight(session, ic)
	defer t.unregisterInflight(session, ic)

	// CombinedOutput equivalent: one shared buffer for both fds, assigned
	// BEFORE Start — os/exec captures the output descriptors at Start time,
	// so assignments after Start are silently ignored (output lost). With
	// an identical writer on both fields Start reuses a single pipe with
	// one copying goroutine (no interleaving race); the buffer is read
	// only after Wait has reaped the process.
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	// Start under inFlightMu so a concurrent abortMatching cannot read
	// ic.cmd.Process while Start mutates the Cmd — the -race DATA RACE this
	// closes. Once Start returns, Process/Pid stay immutable for the
	// command's lifetime, so abort may safely inspect and kill while Wait
	// runs. The lock is never held across Wait: a blocked abort must not
	// stall until the command's natural timeout (abort semantics unchanged).
	t.inFlightMu.Lock()
	startErr := cmd.Start()
	t.inFlightMu.Unlock()
	if startErr != nil {
		if ic.wasAborted() {
			return "", fmt.Errorf("exec aborted by user: %w", startErr)
		}
		if cctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("exec error: timed out after %s: %w", deadline, startErr)
		}
		return "", fmt.Errorf("exec error: %w", startErr)
	}

	err := cmd.Wait()
	b := buf.Bytes()
	if err != nil {
		if ic.wasAborted() {
			return string(b), fmt.Errorf("exec aborted by user: %w", err)
		}
		if cctx.Err() == context.DeadlineExceeded {
			return string(b), fmt.Errorf("exec error: timed out after %s: %w", deadline, err)
		}
		return string(b), fmt.Errorf("exec error: %w", err)
	}
	out := string(b)
	out = strings.TrimRight(out, "\n")
	return out, nil
}

// perCallTimeoutKey carries the caller-requested per-call timeout through
// the registry into runCmd.
type perCallTimeoutKey struct{}

// withPerCallTimeout is set by the registry when the call's arguments
// include a "timeout" (seconds) field.
func withPerCallTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, perCallTimeoutKey{}, d)
}

func perCallTimeoutFromContext(ctx context.Context) (time.Duration, bool) {
	d, ok := ctx.Value(perCallTimeoutKey{}).(time.Duration)
	return d, ok
}

// execSessionKey carries the turn's channel:chatID for /abort targeting.
type execSessionKey struct{}

// WithExecSession stamps the turn's origin session key onto a context so
// in-flight exec commands can be attributed to the chat that started them
// and aborted selectively.
func WithExecSession(ctx context.Context, sessionKey string) context.Context {
	if sessionKey == "" {
		return ctx
	}
	return context.WithValue(ctx, execSessionKey{}, sessionKey)
}

func sessionKeyFromContext(ctx context.Context) string {
	s, _ := ctx.Value(execSessionKey{}).(string)
	return s
}

func (t *ExecTool) registerInflight(session string, ic *inflightCmd) {
	if session == "" {
		return
	}
	t.mu.Lock()
	if t.inFlight == nil {
		t.inFlight = make(map[string]map[*inflightCmd]struct{})
	}
	if t.inFlight[session] == nil {
		t.inFlight[session] = make(map[*inflightCmd]struct{})
	}
	t.inFlight[session][ic] = struct{}{}
	t.mu.Unlock()
}

func (t *ExecTool) unregisterInflight(session string, ic *inflightCmd) {
	if session == "" {
		return
	}
	t.mu.Lock()
	if m := t.inFlight[session]; m != nil {
		delete(m, ic)
		if len(m) == 0 {
			delete(t.inFlight, session)
		}
	}
	t.mu.Unlock()
}

// AbortSession kills every in-flight foreground exec attributed to the
// given origin session key. The calls return with an "aborted by user"
// error; the turn itself keeps running. Returns the number of commands
// killed.
func (t *ExecTool) AbortSession(session string) int {
	return t.abortMatching(func(k string) bool { return k == session })
}

// AbortSessionPrefix kills in-flight execs for all sessions whose key
// starts with the given chat prefix (channel:chatID) — covers the bare
// key, namespaced project keys, and the signal namespace of one chat.
func (t *ExecTool) AbortSessionPrefix(chatKey string) int {
	if chatKey == "" {
		return 0
	}
	// Session keys that belong to one chat share the "channel:chatID" tail:
	// "telegram:111", "signal:telegram:111", "proj:book:telegram:111".
	// Match on the tail so an abort from the bare chat key reaches the
	// signal namespace and project namespaces too.
	return t.abortMatching(func(k string) bool {
		return k == chatKey || strings.HasSuffix(k, ":"+chatKey)
	})
}

func (t *ExecTool) abortMatching(match func(string) bool) int {
	t.mu.Lock()
	killed := 0
	for key, m := range t.inFlight {
		if !match(key) {
			continue
		}
		for ic := range m {
			ic.mu.Lock()
			ic.aborted = true
			// inFlightMu guards the Cmd-handle reads (ic.cmd / Process /
			// Pid) against exec.Cmd.Start inside runCmd — Start mutates
			// the Cmd struct, and concurrent reads were the -race DATA
			// RACE. After Start returns, Process/Pid are immutable, so
			// this kill is safe the rest of the command's life. The lock
			// is never held across runCmd's Wait: aborts still land
			// instantly on running commands (semantics unchanged).
			t.inFlightMu.Lock()
			if ic.cmd != nil && ic.cmd.Process != nil {
				// Process-group kill — same reason as runCmd's Cancel.
				_ = syscall.Kill(-ic.cmd.Process.Pid, syscall.SIGKILL)
				killed++
			}
			t.inFlightMu.Unlock()
			ic.mu.Unlock()
		}
		delete(t.inFlight, key)
	}
	t.mu.Unlock()
	return killed
}

// shellJoin joins args into a properly quoted shell command string.
func shellJoin(args []string) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = shellQuote(arg)
	}
	return strings.Join(parts, " ")
}

// shellQuote wraps a string in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// If the string contains only safe characters, no quoting needed
	needsQuote := false
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '/' || c == ':' || c == '@' || c == '=' || c == '+') {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return s
	}
	// Use single quotes. Escape embedded single quotes by ending the quote,
	// adding an escaped single quote, and starting a new quote.
	parts := strings.Split(s, "'")
	return "'" + strings.Join(parts, `'\''`) + "'"
}
