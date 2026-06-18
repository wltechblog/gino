package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FilesystemTool provides read/write/list operations within the filesystem.
// All operations are sandboxed using os.Root (Go 1.24+), which provides
// kernel-enforced path containment via openat() syscalls.
//
// Multiple roots can be opened for different allowed directories.
// Paths are matched to the most specific (longest) matching root.
type FilesystemTool struct {
	roots   []*os.Root
	rootDir string // primary workspace (for relative paths)
	dirs    []string // sorted longest-first for matching
}

// NewFilesystemTool opens os.Root handles for the workspace and any extra
// allowed directories. The workspace is always the primary root for relative paths.
func NewFilesystemTool(workspaceDir string, allowedDirs []string) (*FilesystemTool, error) {
	absWorkspace, err := filepath.Abs(workspaceDir)
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
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
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
	var firstErr error
	for _, r := range t.roots {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// WorkspaceRoot returns the primary workspace os.Root for use by other tools
// (e.g. SkillManager) that only operate within the workspace.
func (t *FilesystemTool) WorkspaceRoot() *os.Root {
	if len(t.roots) == 0 {
		return nil
	}
	return t.roots[0]
}

// resolve finds the matching root and returns (root, relativePath).
// For absolute paths, it matches against the allowed directories.
// For relative paths, it uses the primary workspace root.
func (t *FilesystemTool) resolve(pathStr string) (*os.Root, string, error) {
	if !strings.HasPrefix(pathStr, "/") {
		// Relative path — use workspace (first matching root)
		return t.roots[0], pathStr, nil
	}

	cleaned := filepath.Clean(pathStr)

	// dirs is sorted longest-first, so first match is most specific
	for i, d := range t.dirs {
		if cleaned == d {
			return t.roots[i], ".", nil
		}
		if strings.HasPrefix(cleaned, d+string(filepath.Separator)) {
			rel := strings.TrimPrefix(cleaned, d+string(filepath.Separator))
			return t.roots[i], rel, nil
		}
	}

	return nil, "", fmt.Errorf("filesystem: path %q is outside all allowed directories", pathStr)
}

func (t *FilesystemTool) Name() string        { return "filesystem" }
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
