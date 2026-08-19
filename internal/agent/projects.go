package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Project represents a registered project directory the agent can switch
// between at runtime, mirroring the -project CLI flag semantics.
type Project struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
}

// projectRegistryState is the on-disk representation of the registry.
type projectRegistryState struct {
	Projects map[string]*Project `json:"projects"`
	// Active is the name of the globally active project ("" = profile workspace).
	Active string `json:"active"`
}

// ProjectRegistry manages named project directories plus the name of the
// globally active project. Exactly one workspace is active at a time,
// mirroring the -project CLI flag. It persists to projects.json in the Gino
// profile directory so state survives restarts.
//
// The registry is safe for concurrent use.
type ProjectRegistry struct {
	mu       sync.RWMutex
	projects map[string]*Project
	active   string
	path     string
}

// LoadProjectRegistry loads the project registry from <profile>/projects.json,
// creating an empty registry file if none exists.
func LoadProjectRegistry(profileDir string) (*ProjectRegistry, error) {
	reg := &ProjectRegistry{
		projects: make(map[string]*Project),
	}
	// Empty profileDir (tests, embedded use): in-memory registry only.
	// saveLocked skips persistence when path is empty. Note filepath.Join
	// would yield a CWD-relative path, which must never happen.
	if profileDir != "" {
		reg.path = filepath.Join(profileDir, "projects.json")
		data, err := os.ReadFile(reg.path)
		if err != nil {
			if os.IsNotExist(err) {
				// Bootstrap with an empty registry file so future saves never
				// race the first write.
				if err := reg.Save(); err != nil {
					return nil, fmt.Errorf("project registry: create: %w", err)
				}
				return reg, nil
			}
			return nil, fmt.Errorf("project registry: read: %w", err)
		}
		var state projectRegistryState
		if err := json.Unmarshal(data, &state); err != nil {
			// Corrupt registry: back it up and start fresh rather than refuse to boot.
			_ = os.Rename(reg.path, reg.path+".corrupt")
			if err := reg.Save(); err != nil {
				return nil, fmt.Errorf("project registry: recreate after corrupt file: %w", err)
			}
			return reg, nil
		}
		if state.Projects != nil {
			reg.projects = state.Projects
		}
		reg.active = state.Active
		// Drop a stale active selection whose project no longer exists.
		if reg.active != "" {
			if _, ok := reg.projects[reg.active]; !ok {
				reg.active = ""
			}
		}
	}
	return reg, nil
}

// Save writes the registry state to disk atomically.
func (r *ProjectRegistry) Save() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.saveLocked()
}

func (r *ProjectRegistry) saveLocked() error {
	if r.path == "" {
		return nil
	}
	state := projectRegistryState{Projects: r.projects, Active: r.active}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("project registry: marshal: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("project registry: write temp: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("project registry: rename: %w", err)
	}
	return nil
}

// validProjectName enforces a conservative charset: letters, digits, dash,
// underscore, dot — but not a leading dot (no hidden names) and not empty.
// Names are capped at 32 chars so they always fit Telegram callback_data
// payloads (64-byte limit) alongside the "prj:" prefix.
func validProjectName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	if strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// Add registers a project by name and path. The path is validated the same
// way the -project CLI flag is: it must resolve to an existing directory.
func (r *ProjectRegistry) Add(name, path string) (*Project, error) {
	name = strings.TrimSpace(name)
	if !validProjectName(name) {
		return nil, fmt.Errorf("invalid project name %q: use letters, digits, '-', '_', '.' (max 32 chars)", name)
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("project path is empty")
	}
	resolved, err := ResolveProjectWorkspace(path, "")
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.projects[name]; ok {
		return nil, fmt.Errorf("project %q already registered → %s (use a different name or remove it first)", name, existing.Path)
	}
	p := &Project{Name: name, Path: resolved, CreatedAt: time.Now()}
	r.projects[name] = p
	if err := r.saveLocked(); err != nil {
		delete(r.projects, name)
		return nil, err
	}
	return p, nil
}

// Remove unregisters a project by name. If it was the active project, the
// active selection clears and the agent falls back to the profile workspace.
func (r *ProjectRegistry) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.projects[name]; !ok {
		return fmt.Errorf("project %q not found", name)
	}
	delete(r.projects, name)
	if r.active == name {
		r.active = ""
	}
	return r.saveLocked()
}

// List returns all registered projects sorted by name.
func (r *ProjectRegistry) List() []*Project {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Project, 0, len(r.projects))
	for _, p := range r.projects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns a project by name.
func (r *ProjectRegistry) Get(name string) (*Project, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.projects[name]
	return p, ok
}

// ActiveProject returns the name of the globally active project,
// or "" when the agent is on the profile workspace.
func (r *ProjectRegistry) ActiveProject() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.active == "" {
		return ""
	}
	if _, ok := r.projects[r.active]; !ok {
		return ""
	}
	return r.active
}

// SetActive records the globally active project. An empty name clears the
// selection (profile workspace). The change is persisted.
func (r *ProjectRegistry) SetActive(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name != "" {
		if _, ok := r.projects[name]; !ok {
			return fmt.Errorf("project %q not found", name)
		}
	}
	r.active = name
	return r.saveLocked()
}
