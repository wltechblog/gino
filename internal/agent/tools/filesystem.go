package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/wltechblog/gino/internal/config"
)

// FilesystemTool provides read/write/list operations within the filesystem.
// All operations are sandboxed using os.Root (Go 1.24+), which provides
// kernel-enforced path containment via openat() syscalls.
//
// Multiple roots can be opened for different allowed directories.
// Paths are matched to the most specific (longest) matching root.
type FilesystemTool struct {
	mu      sync.RWMutex
	roots   []*os.Root
	rootDir string   // primary workspace (for relative paths)
	dirs    []string // sorted longest-first for matching
}

// NewFilesystemTool opens os.Root handles for the workspace and any extra
// allowed directories. The workspace is always the primary root for relative paths.
func NewFilesystemTool(workspaceDir string, allowedDirs []string, sandbox config.SandboxConfig) (*FilesystemTool, error) {
	// In yolo mode, allow access to the entire filesystem
	if sandbox.IsYolo() {
		allowedDirs = append([]string{"/"}, allowedDirs...)
	}
	absWorkspace, err := canonicalDir(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("filesystem: resolve workspace path: %w", err)
	}

	ft := &FilesystemTool{
		rootDir: absWorkspace,
	}

	// Collect all directories: workspace + allowed
	allDirs := make([]string, 0, 1+len(allowedDirs))
	allDirs = append(allDirs, absWorkspace)
	for _, d := range allowedDirs {
		if d == "" {
			continue
		}
		abs, err := canonicalDir(d)
		if err != nil {
			continue
		}
		// Skip duplicates
		duplicate := false
		for _, existing := range allDirs {
			if existing == abs {
				duplicate = true
				break
			}
		}
		if !duplicate {
			allDirs = append(allDirs, abs)
		}
	}

	// Sort longest-first so we match the most specific root first
	sort.Slice(allDirs, func(i, j int) bool {
		return len(allDirs[i]) > len(allDirs[j])
	})

	ft.dirs = allDirs

	// Open a root for each directory
	for _, d := range allDirs {
		root, err := os.OpenRoot(d)
		if err != nil {
			// Close already-opened roots on failure
			for _, r := range ft.roots {
				_ = r.Close()
			}
			return nil, fmt.Errorf("filesystem: open root %q: %w", d, err)
		}
		ft.roots = append(ft.roots, root)
	}

	return ft, nil
}

// Close releases all underlying os.Root file descriptors.
func (t *FilesystemTool) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var firstErr error
	for _, r := range t.roots {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	t.roots = nil
	return firstErr
}

// SetWorkspace atomically swaps the primary workspace to dir. Relative paths
// resolve against it; the previously configured allowed directories remain
// accessible. Used by runtime project switching.
func (t *FilesystemTool) SetWorkspace(dir string) error {
	abs, err := canonicalDir(dir)
	if err != nil {
		return fmt.Errorf("filesystem: resolve workspace path: %w", err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("filesystem: workspace %q is not an accessible directory", abs)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return fmt.Errorf("filesystem: open root %q: %w", abs, err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	var oldPrimaryDir string
	var oldPrimaryRoot *os.Root
	if len(t.dirs) > 0 {
		oldPrimaryDir = t.dirs[0]
		oldPrimaryRoot = t.roots[0]
	}

	// New set: new workspace first, then carry over the existing
	// non-primary allowed dirs (deduped against the new primary).
	newDirs := []string{abs}
	newRoots := []*os.Root{root}
	for i, d := range t.dirs {
		if i == 0 {
			continue // old primary handled separately below
		}
		if d == abs {
			// Same dir as the new primary; keep the new root, drop the old one.
			_ = t.roots[i].Close()
			continue
		}
		newDirs = append(newDirs, d)
		newRoots = append(newRoots, t.roots[i])
	}

	// Retain the old primary as a plain allowed dir if it differs from the
	// new primary — switching projects should not revoke access to files
	// written under the previous project (e.g. session work products).
	if oldPrimaryDir != "" && oldPrimaryDir != abs {
		newDirs = append(newDirs, oldPrimaryDir)
		newRoots = append(newRoots, oldPrimaryRoot)
	}

	// Sort longest-first so we match the most specific root first.
	type pair struct {
		dir  string
		root *os.Root
	}
	pairs := make([]pair, len(newDirs))
	for i := range pairs {
		pairs[i] = pair{newDirs[i], newRoots[i]}
	}
	sort.SliceStable(pairs, func(a, b int) bool { return len(pairs[a].dir) > len(pairs[b].dir) })
	t.dirs = make([]string, len(pairs))
	t.roots = make([]*os.Root, len(pairs))
	for i := range pairs {
		t.dirs[i] = pairs[i].dir
		t.roots[i] = pairs[i].root
	}
	t.rootDir = abs
	return nil
}

// WorkspaceDir returns the current primary workspace directory path.
func (t *FilesystemTool) WorkspaceDir() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.rootDir
}

// WorkspaceRoot returns the primary workspace os.Root for use by other tools
// (e.g. SkillManager) that only operate within the workspace.
func (t *FilesystemTool) WorkspaceRoot() *os.Root {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.roots) == 0 {
		return nil
	}
	return t.roots[0]
}

// RootForDir returns the managed os.Root corresponding to an allowed directory.
// The returned root is owned by FilesystemTool and must not be closed by callers.
func (t *FilesystemTool) RootForDir(dir string) *os.Root {
	abs, err := canonicalDir(dir)
	if err != nil {
		return nil
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	for i, d := range t.dirs {
		if d == abs && i < len(t.roots) {
			return t.roots[i]
		}
	}
	return nil
}

func canonicalDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// resolve finds the matching root and returns (root, relativePath).
// For absolute paths, it matches against the allowed directories.
// For relative paths, it uses the primary workspace root.
func (t *FilesystemTool) resolve(pathStr string) (*os.Root, string, error) {
	if !strings.HasPrefix(pathStr, "/") {
		// Relative path — use workspace (first matching root)
		t.mu.RLock()
		defer t.mu.RUnlock()
		return t.roots[0], pathStr, nil
	}

	cleaned := filepath.Clean(pathStr)

	t.mu.RLock()
	defer t.mu.RUnlock()

	// dirs is sorted longest-first, so first match is most specific
	for i, d := range t.dirs {
		if cleaned == d {
			return t.roots[i], ".", nil
		}
		// Handle root "/" specially to avoid "//" prefix
		if d == "/" {
			rel := strings.TrimPrefix(cleaned, "/")
			return t.roots[i], rel, nil
		}
		if strings.HasPrefix(cleaned, d+string(filepath.Separator)) {
			rel := strings.TrimPrefix(cleaned, d+string(filepath.Separator))
			return t.roots[i], rel, nil
		}
	}

	return nil, "", fmt.Errorf("filesystem: path %q is outside all allowed directories", pathStr)
}

func (t *FilesystemTool) Name() string { return "filesystem" }
func (t *FilesystemTool) Description() string {
	return "Read, write, edit (find-and-replace), and list files in the workspace and allowed directories. For editing source code and project files, use action 'edit' with old_text/new_text — do NOT use edit_memory (that is only for memory/notes files)."
}

func (t *FilesystemTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"description": "The filesystem operation to perform",
				"enum":        []string{"read", "write", "edit", "list"},
			},
			"path": map[string]interface{}{
				"type":        "string",
				"description": "The file or directory path (relative to workspace, or absolute within allowedDirs)",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Content to write (required when action is 'write')",
			},
			"old_text": map[string]interface{}{
				"type":        "string",
				"description": "Exact text to find and replace (required when action is 'edit')",
			},
			"new_text": map[string]interface{}{
				"type":        "string",
				"description": "Replacement text for edit action (omit or empty string to delete the matched text)",
			},
		},
		"required": []string{"action", "path"},
	}
}

func (t *FilesystemTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	actionRaw, ok := args["action"]
	if !ok {
		return "", fmt.Errorf("filesystem: 'action' is required")
	}
	action, ok := actionRaw.(string)
	if !ok {
		return "", fmt.Errorf("filesystem: 'action' must be a string")
	}
	pathRaw := args["path"]
	pathStr := ""
	if pathRaw != nil {
		switch v := pathRaw.(type) {
		case string:
			pathStr = v
		default:
			return "", fmt.Errorf("filesystem: 'path' must be a string")
		}
	}
	if pathStr == "" {
		pathStr = "."
	}

	root, relPath, err := t.resolve(pathStr)
	if err != nil {
		return "", err
	}

	switch action {
	case "read":
		b, err := root.ReadFile(relPath)
		if err != nil {
			return "", err
		}
		return string(b), nil
	case "write":
		contentRaw := args["content"]
		content := ""
		switch v := contentRaw.(type) {
		case string:
			content = v
		default:
			return "", fmt.Errorf("filesystem: 'content' must be a string")
		}
		// Create parent directories if needed
		dir := filepath.Dir(relPath)
		if dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
		}
		if err := root.WriteFile(relPath, []byte(content), 0o644); err != nil {
			return "", err
		}
		return "written", nil
	case "edit":
		oldTextRaw := args["old_text"]
		oldText, ok := oldTextRaw.(string)
		if !ok || oldText == "" {
			return "", fmt.Errorf("filesystem: 'old_text' is required for edit action")
		}
		newText, _ := args["new_text"].(string)
		b, err := root.ReadFile(relPath)
		if err != nil {
			return "", err
		}
		content := string(b)
		if !strings.Contains(content, oldText) {
			return "", fmt.Errorf("filesystem: old_text not found in %s", pathStr)
		}
		updated := strings.ReplaceAll(content, oldText, newText)
		if err := root.WriteFile(relPath, []byte(updated), 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("edited %s", pathStr), nil
	case "list":
		f, err := root.Open(relPath)
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		entries, err := f.ReadDir(-1)
		if err != nil {
			return "", err
		}
		out := ""
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			out += name + "\n"
		}
		return out, nil
	default:
		return "", fmt.Errorf("filesystem: unknown action %s", action)
	}
}
