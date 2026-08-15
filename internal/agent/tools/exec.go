package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wltechblog/gino/internal/config"
)

// ExecTool runs shell commands with a timeout.
// Behavior depends on SandboxConfig mode:
//   - "strict":     array-only commands, no absolute paths, full blacklist (default, backward compatible)
//   - "permissive": block truly dangerous commands (dd, mkfs, shutdown), allow absolute paths, array-only
//   - "yolo":       no restrictions — string commands allowed, no path validation, no blacklist

type ExecTool struct {
	timeout     time.Duration
	allowedDir  string
	allowedDirs []string
	sandbox     config.SandboxConfig
}

func NewExecTool(timeoutSecs int) *ExecTool {
	return &ExecTool{timeout: time.Duration(timeoutSecs) * time.Second, sandbox: config.SandboxConfig{}}
}

func NewExecToolWithWorkspace(timeoutSecs int, allowedDir string) *ExecTool {
	return &ExecTool{timeout: time.Duration(timeoutSecs) * time.Second, allowedDir: allowedDir, sandbox: config.SandboxConfig{}}
}

func NewExecToolWithAllowedDirs(timeoutSecs int, allowedDir string, allowedDirs []string) *ExecTool {
	dirs := make([]string, 0, len(allowedDirs))
	for _, d := range allowedDirs {
		if d != "" {
			dirs = append(dirs, filepath.Clean(d))
		}
	}
	return &ExecTool{timeout: time.Duration(timeoutSecs) * time.Second, allowedDir: allowedDir, allowedDirs: dirs, sandbox: config.SandboxConfig{Mode: "permissive"}}
}

// NewExecToolWithSandbox creates an ExecTool with full sandbox configuration.
func NewExecToolWithSandbox(timeoutSecs int, allowedDir string, allowedDirs []string, sandbox config.SandboxConfig) *ExecTool {
	dirs := make([]string, 0, len(allowedDirs))
	for _, d := range allowedDirs {
		if d != "" {
			dirs = append(dirs, filepath.Clean(d))
		}
	}
	return &ExecTool{timeout: time.Duration(timeoutSecs) * time.Second, allowedDir: allowedDir, allowedDirs: dirs, sandbox: sandbox}
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

	// If sandbox allows absolute paths, check if path is within allowed dirs
	if t.sandbox.AllowsAbsolutePaths() {
		cleaned := filepath.Clean(s)
		for _, d := range t.allowedDirs {
			if cleaned == d || (d == "/") {
				return false
			}
			if strings.HasPrefix(cleaned, d+string(filepath.Separator)) {
				return false
			}
		}
		if t.allowedDir != "" {
			ad := filepath.Clean(t.allowedDir)
			if cleaned == ad || strings.HasPrefix(cleaned, ad+string(filepath.Separator)) {
				return false
			}
		}
		// Absolute path outside allowed dirs — still unsafe
		return true
	}

	// Strict mode: all absolute paths are unsafe
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

func (t *ExecTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	cmdRaw, ok := args["cmd"]
	log.Printf("[DEBUG-EXEC] cmdRaw type: %T, value: %v", cmdRaw, cmdRaw)

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

func (t *ExecTool) resolveWorkDir(args map[string]interface{}) (string, error) {
	workDir := t.allowedDir
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
	if t.timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(cctx, prog, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	log.Printf("[DEBUG-EXEC] runCmd: prog=%s args=%v dir=%s", prog, args, dir)
	b, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[DEBUG-EXEC] error: %v, output: %s", err, string(b))
		return string(b), fmt.Errorf("exec error: %w", err)
	}
	out := string(b)
	out = strings.TrimRight(out, "\n")
	return out, nil
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
