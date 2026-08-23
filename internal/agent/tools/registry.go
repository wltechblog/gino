package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/wltechblog/gino/internal/providers"
)

const maxToolResultBytes = 64 * 1024 // 64 KB

// Tool is the interface for tools callable by the agent.
type Tool interface {
	Name() string
	Description() string
	// Parameters returns the JSON Schema for tool arguments (nil if no params).
	Parameters() map[string]interface{}
	// Execute performs the tool action and returns a string result.
	Execute(ctx context.Context, args map[string]interface{}) (string, error)
}

// Registry holds registered tools.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry constructs a new tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool to the registry.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Unregister removes a tool from the registry by name.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// Get returns a tool by name (or nil if not found).
func (r *Registry) Get(name string) Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tools[name]
}

// Definitions returns the list of tool definitions to expose to the model.
// Tools are sorted by name so the serialized request is deterministic.
// Random map iteration order would reshuffle the tools array on every
// request, defeating provider-side prompt caching (which requires a
// byte-for-byte identical token prefix).
func (r *Registry) Definitions() []providers.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]providers.ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, providers.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// Execute executes a registered tool by name with args and returns result or error.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	if name == "" {
		return "", errors.New("tool name is required")
	}
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return "", errors.New("tool not found")
	}

	// Log tool execution start
	argsJSON, _ := json.Marshal(args)
	log.Printf("[tool] → %s %s", name, argsJSON)
	start := time.Now()

	// Recover from tool panics so a single buggy tool cannot take down the
	// entire gateway process. The panic is converted into a tool error that
	// the LLM can see and react to.
	var result string
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[tool] ✗ %s PANICKED after %s: %v\n%s", name, time.Since(start).Round(time.Millisecond), r, debug.Stack())
				result = ""
				err = fmt.Errorf("tool panicked: %v", r)
			}
		}()
		result, err = t.Execute(ctx, args)
	}()
	elapsed := time.Since(start).Round(time.Millisecond)

	if err != nil {
		log.Printf("[tool] ✗ %s failed after %s: %v", name, elapsed, err)
		return "", err
	}

	if len(result) > maxToolResultBytes {
		log.Printf("[tool] ⚠ %s response truncated: %d → %d bytes", name, len(result), maxToolResultBytes)
		dumpPath := fmt.Sprintf("/tmp/gino-tool-dump-%s-%d.json", name, time.Now().UnixMilli())
		if dumpErr := os.WriteFile(dumpPath, []byte(result), 0644); dumpErr != nil {
			log.Printf("[tool] ⚠ failed to dump response to %s: %v", dumpPath, dumpErr)
		} else {
			log.Printf("[tool] ⚠ full response saved to %s", dumpPath)
		}
		result = result[:maxToolResultBytes] + fmt.Sprintf("\n... [truncated %d bytes, full dump: %s]", len(result)-maxToolResultBytes, dumpPath)
	}

	log.Printf("[tool] ✓ %s completed in %s (%d bytes)", name, elapsed, len(result))
	return result, nil
}
