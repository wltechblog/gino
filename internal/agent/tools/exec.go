package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ExecTool runs shell commands with a timeout.
// For safety:
// - prefer array form: {"cmd": ["ls", "-la"]}
// - string form (shell) is disallowed to avoid shell injection
// - blacklist dangerous program names (rm, sudo, dd, mkfs, shutdown, reboot)
// - arguments containing absolute paths, ~ or .. are rejected
// - optional allowedDir enforces a working directory
// - optional cwd parameter overrides working directory per-command (must be within allowedDirs)

type ExecTool struct {
	timeout     time.Duration
	allowedDir  string
	allowedDirs []string
}

func NewExecTool(timeoutSecs int) *ExecTool {
	return &ExecTool{timeout: time.Duration(timeoutSecs) * time.Second}
}

func NewExecToolWithWorkspace(timeoutSecs int, allowedDir string) *ExecTool {
	return &ExecTool{timeout: time.Duration(timeoutSecs) * time.Second, allowedDir: allowedDir}
}

func NewExecToolWithAllowedDirs(timeoutSecs int, allowedDir string, allowedDirs []string) *ExecTool {
	dirs := make([]string, 0, len(allowedDirs))
	for _, d := range allowedDirs {
		if d != "" {
			dirs = append(dirs, filepath.Clean(d))
		}
	}
	return &ExecTool{timeout: time.Duration(timeoutSecs) * time.Second, allowedDir: allowedDir, allowedDirs: dirs}
}

func (t *ExecTool) Name() string { return "exec" }
func (t *ExecTool) Description() string {
	return "Execute shell commands (array form only, restricted for safety)"
}

func (t *ExecTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
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
		},
		"required": []string{"cmd"},
	}
}

var dangerous = map[string]struct{}{
	"rm":       {},
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

func isDangerousProg(prog string) bool {
	base := filepath.Base(prog)
	base = strings.ToLower(base)
	_, ok := dangerous[base]
	return ok
}

func isShellBuiltin(prog string) (hint string, ok bool) {
	hint, ok = shellBuiltins[strings.ToLower(filepath.Base(prog))]
	return
}

func hasUnsafeArg(s string, allowedDirs []string) bool {
	if strings.HasPrefix(s, "~") || strings.Contains(s, "..") {
		return true
	}
	if !strings.HasPrefix(s, "/") {
		return false
	}
	cleaned := filepath.Clean(s)
	for _, d := range allowedDirs {
		if cleaned == d {
			return false
		}
		if d == "/" {
			// root matches everything
			return false
		}
		if strings.HasPrefix(cleaned, d+string(filepath.Separator)) {
			return false
		}
	}
	return true
}

// isDirAllowed checks if a directory path is within one of the allowed directories.
func (t *ExecTool) isDirAllowed(dir string) bool {
	cleaned := filepath.Clean(dir)
	for _, d := range t.allowedDirs {
		if cleaned == d || cleaned == filepath.Clean(d) {
			return true
		}
		if strings.HasPrefix(cleaned, filepath.Clean(d)+string(filepath.Separator)) {
			return true
		}
	}
	// Also check the default allowedDir
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
	if !ok {
		return "", fmt.Errorf("exec: 'cmd' argument required")
	}

	// Disallow shell-string commands for safety
	if _, ok := cmdRaw.(string); ok {
		return "", errors.New("exec: string commands are disallowed; use array form")
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
	workDir := t.allowedDir
	if cwdRaw, ok := args["cwd"]; ok {
		cwd, ok := cwdRaw.(string)
		if !ok {
			return "", fmt.Errorf("exec: cwd must be a string")
		}
		cleaned := filepath.Clean(cwd)
		if !t.isDirAllowed(cleaned) {
			return "", fmt.Errorf("exec: cwd '%s' is not within an allowed directory", cwd)
		}
		workDir = cleaned
	}

	prog := argv[0]
	if isDangerousProg(prog) {
		return "", fmt.Errorf("exec: program '%s' is disallowed", prog)
	}

	if hint, ok := isShellBuiltin(prog); ok {
		if hint != "" {
			return "", fmt.Errorf("exec: %s", hint)
		}
		shellArg := strings.Join(argv, " ")
		for _, a := range argv[1:] {
			if hasUnsafeArg(a, t.allowedDirs) {
				return "", fmt.Errorf("exec: argument '%s' looks unsafe", a)
			}
		}
		return t.runCmd(ctx, "sh", []string{"-c", shellArg}, workDir)
	}

	for _, a := range argv[1:] {
		if hasUnsafeArg(a, t.allowedDirs) {
			return "", fmt.Errorf("exec: argument '%s' looks unsafe", a)
		}
	}

	return t.runCmd(ctx, prog, argv[1:], workDir)
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
	b, err := cmd.CombinedOutput()
	if err != nil {
		return string(b), fmt.Errorf("exec error: %w", err)
	}
	out := string(b)
	out = strings.TrimRight(out, "\n")
	return out, nil
}
