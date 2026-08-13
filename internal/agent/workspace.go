package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveProjectWorkspace returns the directory the agent should treat as the
// active project. An empty projectFlag keeps the configured profile workspace
// so existing callers remain unchanged.
func ResolveProjectWorkspace(projectFlag, profileWS string) (string, error) {
	if strings.TrimSpace(projectFlag) == "" {
		return profileWS, nil
	}

	resolved, err := filepath.Abs(projectFlag)
	if err != nil {
		return "", fmt.Errorf("failed to resolve project %q: %w", projectFlag, err)
	}
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = real
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("failed to access project %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project is not a directory: %s", resolved)
	}
	return resolved, nil
}

func sameWorkspace(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}
